// Package worker provides private HTTP boundaries for external media processes.
// Upstream DTOs are translated by providers, never served directly to clients.
package worker

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Mode     string `json:"mode"`
	Listen   string `json:"listen"`
	Token    string `json:"token"`
	StateDir string `json:"stateDir"`
	Pin      string `json:"pin,omitempty"`
}

// The private worker health response exposes only bounded audio-flow evidence.
// The daemon's account, track URI, title and artwork never enter this payload.
type spotifyWorkerHealth struct {
	spotifyHealthResult
	Audio  spotifyAudioDiagnostic `json:"audio"`
	Daemon *spotifyDaemonHealth   `json:"daemon,omitempty"`
}

func (c Config) Validate() error {
	if (c.Mode != "spotify" && c.Mode != "airplay") || c.Listen == "" || len(c.Token) < 32 || !filepath.IsAbs(c.StateDir) {
		return errors.New("invalid worker configuration")
	}
	if c.Mode == "airplay" && !regexp.MustCompile(`^[0-9]{4}$`).MatchString(c.Pin) {
		return errors.New("AirPlay requires a four-digit receiver PIN")
	}
	return nil
}

func Handler(ctx context.Context, c Config) http.Handler {
	return HandlerWithSpotifyDiagnostics(ctx, c, nil)
}

// HandlerWithSpotifyDiagnostics adds only fixed failure categories to the
// authenticated private worker health route; raw daemon output is never served.
func HandlerWithSpotifyDiagnostics(ctx context.Context, c Config, diagnostics *SpotifyDaemonDiagnostics) http.Handler {
	mux := http.NewServeMux()
	var bridge *spotifyBridge
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	proxy := func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://127.0.0.1:3678"+r.URL.Path, strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			http.Error(w, "service unavailable", 503)
			return
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
		if err != nil || len(data) > 1<<20 {
			http.Error(w, "invalid response", 502)
			return
		}
		if bridge != nil && (res.StatusCode == 200 || res.StatusCode == 204) {
			if r.URL.Path == "/status" {
				if res.StatusCode == 204 {
					bridge.OnStopped()
					diagnostics.observe(false, false)
				} else {
					var st struct {
						Stopped   bool `json:"stopped"`
						Buffering bool `json:"buffering"`
						Track     *struct {
							URI string `json:"uri"`
						} `json:"track"`
					}
					if json.Unmarshal(data, &st) == nil {
						if st.Stopped {
							bridge.OnStopped()
						} else if st.Track != nil && st.Track.URI != "" {
							bridge.OnTrack(st.Track.URI)
						}
						diagnostics.observe(!st.Stopped && st.Buffering && st.Track == nil, bridge.diagnostic().Active)
					}
				}
			} else if r.URL.Path == "/player/stop" {
				bridge.OnStopped()
			} else if r.URL.Path == "/player/next" || r.URL.Path == "/player/prev" || r.URL.Path == "/player/seek" {
				bridge.ResetBuffer()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(data)
	}
	if c.Mode == "spotify" {
		bridge = newSpotifyBridge(ctx, filepath.Join(c.StateDir, "audio.pcm"))
		if bridge != nil {
			context.AfterFunc(ctx, bridge.Close)
		}
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
			health, err := spotifyHealth(r.Context(), client, "http://127.0.0.1:3678", c.StateDir)
			if err != nil {
				http.Error(w, "daemon unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			audio := bridge.diagnostic()
			_ = json.NewEncoder(w).Encode(spotifyWorkerHealth{spotifyHealthResult: health, Audio: audio, Daemon: diagnostics.snapshot(health.BufferingWithoutTrack, audio.Active)})
		})
		mux.HandleFunc("GET /status", proxy)
		mux.HandleFunc("GET /auth/code", proxy)
		for _, command := range []string{"pause", "resume", "next", "prev", "stop", "seek", "volume"} {
			mux.HandleFunc("POST /player/"+command, proxy)
		}
		mux.HandleFunc("GET /audio", func(w http.ResponseWriter, r *http.Request) {
			if bridge == nil {
				http.Error(w, "audio unavailable", 503)
				return
			}
			bridge.ServeHTTP(w, r)
		})
	} else {
		mux.HandleFunc("GET /pairing", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				PIN string `json:"pin"`
			}{PIN: c.Pin})
		})
		mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
			info, err := os.Stat(filepath.Join(c.StateDir, "hls", "index.m3u8"))
			active := err == nil && time.Since(info.ModTime()) < 15*time.Second
			audioActive := airplayHLSPlayable(filepath.Join(c.StateDir, "hls"), "audio.m3u8")
			w.Header().Set("Content-Type", "application/json")
			status := map[string]any{"active": active, "audioActive": audioActive}
			meta := airplayMetadata(c.StateDir)
			if len(meta) > 0 {
				status["metadata"] = meta
			}
			_ = json.NewEncoder(w).Encode(status)
		})
		mux.HandleFunc("GET /artwork", func(w http.ResponseWriter, r *http.Request) { airplayArtwork(c.StateDir, w, r) })
		mux.HandleFunc("GET /stream/{file}", func(w http.ResponseWriter, r *http.Request) {
			name := r.PathValue("file")
			if name != "index.m3u8" && name != "audio.m3u8" && !regexp.MustCompile(`^(segment|audio)[0-9]+\.ts$`).MatchString(name) {
				http.NotFound(w, r)
				return
			}
			if name == "index.m3u8" || name == "audio.m3u8" {
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			} else {
				w.Header().Set("Content-Type", "video/mp2t")
			}
			http.ServeFile(w, r, filepath.Join(c.StateDir, "hls", name))
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+c.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// airplayHLSPlayable validates that an HLS manifest contains at least one
// complete, nontrivial media segment (preferring multiple seconds of continuous media),
// rejecting empty, stale, or tiny manifests and surviving atomic in-flight writes.
func airplayHLSPlayable(hlsDir, manifestName string) bool {
	manifestPath := filepath.Join(hlsDir, manifestName)
	info, err := os.Stat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 64<<10 {
		return false
	}
	if time.Since(info.ModTime()) > 15*time.Second {
		return false
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(data) == 0 || len(data) > 64<<10 {
		return false
	}
	content := string(bytes.ToValidUTF8(data, nil))
	if !strings.HasPrefix(content, "#EXTM3U") {
		return false
	}
	lines := strings.Split(content, "\n")
	targetDuration := 0
	hasTargetDuration := false
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			val, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")))
			if err == nil {
				targetDuration = val
				hasTargetDuration = true
			}
			break
		}
	}
	if !hasTargetDuration || targetDuration < 1 {
		return false
	}

	segRegex := regexp.MustCompile(`^(audio|segment)[0-9]+\.ts$`)
	totalSegments := 0
	validSegments := 0
	totalDuration := 0.0

	var (
		pendingDuration    float64
		hasPendingDuration bool
	)

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			durStr := strings.TrimPrefix(line, "#EXTINF:")
			if comma := strings.Index(durStr, ","); comma >= 0 {
				durStr = durStr[:comma]
			}
			dur, err := strconv.ParseFloat(strings.TrimSpace(durStr), 64)
			if err == nil {
				pendingDuration = dur
				hasPendingDuration = true
			} else {
				hasPendingDuration = false
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		totalSegments++
		if !hasPendingDuration {
			continue
		}
		dur := pendingDuration
		hasPendingDuration = false

		if dur < 0.5 {
			continue
		}
		if idx := strings.Index(line, "?"); idx >= 0 {
			line = line[:idx]
		}
		segName := filepath.Base(line)
		if segName != line || !segRegex.MatchString(segName) {
			continue
		}
		segPath := filepath.Join(hlsDir, segName)
		segInfo, err := os.Stat(segPath)
		if err != nil || !segInfo.Mode().IsRegular() {
			continue
		}
		if time.Since(segInfo.ModTime()) > 15*time.Second {
			continue
		}
		if segInfo.Size() < 4096 {
			continue
		}
		validSegments++
		totalDuration += dur
	}

	if totalSegments == 0 || validSegments != totalSegments {
		return false
	}
	return validSegments >= 1 && totalDuration >= 0.5
}
