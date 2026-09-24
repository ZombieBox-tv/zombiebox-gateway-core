// Package worker provides private HTTP boundaries for external media processes.
// Upstream DTOs are translated by providers, never served directly to clients.
package worker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
				} else {
					var st struct {
						Stopped bool `json:"stopped"`
						Track   *struct {
							URI string `json:"uri"`
						} `json:"track"`
					}
					if json.Unmarshal(data, &st) == nil {
						if st.Stopped {
							bridge.OnStopped()
						} else if st.Track != nil && st.Track.URI != "" {
							bridge.OnTrack(st.Track.URI)
						}
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
			_ = json.NewEncoder(w).Encode(health)
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
			audio, audioErr := os.Stat(filepath.Join(c.StateDir, "hls", "audio.m3u8"))
			audioActive := audioErr == nil && time.Since(audio.ModTime()) < 15*time.Second
			w.Header().Set("Content-Type", "application/json")
			status := map[string]any{"active": active, "audioActive": audioActive}
			if audioActive {
				status["metadata"] = airplayMetadata(c.StateDir)
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
