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
	"os/exec"
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(data)
	}
	if c.Mode == "spotify" {
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
		streams := make(chan struct{}, 1)
		mux.HandleFunc("GET /audio", func(w http.ResponseWriter, r *http.Request) {
			select {
			case streams <- struct{}{}:
				defer func() { <-streams }()
			default:
				http.Error(w, "audio receiver busy", 409)
				return
			}
			streamContext, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
			defer cancel()
			stop := context.AfterFunc(ctx, cancel)
			defer stop()
			cmd := exec.CommandContext(streamContext, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1", "-f", "s16le", "-ar", "44100", "-ac", "2", "-i", filepath.Join(c.StateDir, "audio.pcm"), "-c:a", "libmp3lame", "-b:a", "128k", "-f", "mp3", "-flush_packets", "1", "pipe:1")
			cmd.WaitDelay = time.Second
			stdout, err := cmd.StdoutPipe()
			if err != nil || cmd.Start() != nil {
				http.Error(w, "audio unavailable", 503)
				return
			}
			defer stdout.Close()
			w.Header().Set("Content-Type", "audio/mpeg")
			buffer := make([]byte, 8192)
			for {
				n, readErr := stdout.Read(buffer)
				if n > 0 {
					if _, err = w.Write(buffer[:n]); err != nil {
						cancel()
						break
					}
					_ = http.NewResponseController(w).Flush()
				}
				if readErr != nil {
					break
				}
			}
			cancel()
			_ = cmd.Wait()
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
