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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Mode     string `json:"mode"`
	Listen   string `json:"listen"`
	Token    string `json:"token"`
	StateDir string `json:"stateDir"`
	Pin      string `json:"pin,omitempty"`
}

// AirPlayMirrorStatus contains only fixed protocol state and bounded runtime
// evidence. It intentionally has no field for upstream logs, packet contents,
// addresses, URLs, pairing state, or media metadata.
type AirPlayMirrorStatus struct {
	Protocol                     string                      `json:"protocol"`
	Mode                         string                      `json:"mode"`
	VideoRTPAdvancedRecently     bool                        `json:"videoRtpAdvancedRecently"`
	VideoRTPPacketCount          uint64                      `json:"videoRtpPacketCount"`
	VideoRTPLastPacketAgeMS      int64                       `json:"videoRtpLastPacketAgeMs,omitempty"`
	HLSManifestReady             bool                        `json:"hlsManifestReady"`
	HLSSegmentReady              bool                        `json:"hlsSegmentReady"`
	HLSSegmentAgeMS              int64                       `json:"hlsSegmentAgeMs,omitempty"`
	BridgeStage                  string                      `json:"bridgeStage"`
	BridgeFailureStage           string                      `json:"bridgeFailureStage,omitempty"`
	DirectVideoRequestCount      uint8                       `json:"directVideoRequestCount"`
	PhotoAppAttributionAvailable bool                        `json:"photoAppAttributionAvailable"`
	SessionSummary               AirPlayMirrorSessionSummary `json:"sessionSummary"`
}

// AirPlayMirrorSessionSummary retains one fixed, sanitized session record in
// memory through teardown. Ages are relative to the status snapshot time, not
// wall-clock timestamps. Enum fields use "not_observed" when a stage was not
// reached; Available=false means no retained session record exists.
type AirPlayMirrorSessionSummary struct {
	Version                  uint8  `json:"version"`
	Available                bool   `json:"available"`
	Generation               uint64 `json:"generation,omitempty"`
	SessionAgeMS             int64  `json:"sessionAgeMs,omitempty"`
	FirstVideoRTPObserved    bool   `json:"firstVideoRtpObserved"`
	FirstVideoRTPAgeMS       int64  `json:"firstVideoRtpAgeMs,omitempty"`
	LastVideoRTPObserved     bool   `json:"lastVideoRtpObserved"`
	LastVideoRTPAgeMS        int64  `json:"lastVideoRtpAgeMs,omitempty"`
	VideoRTPPacketCount      uint64 `json:"videoRtpPacketCount,omitempty"`
	SelectedMode             string `json:"selectedMode"`
	FFmpegStartClass         string `json:"ffmpegStartClass"`
	FFmpegExitClass          string `json:"ffmpegExitClass"`
	FirstHLSManifestObserved bool   `json:"firstHlsManifestObserved"`
	FirstHLSManifestAgeMS    int64  `json:"firstHlsManifestAgeMs,omitempty"`
	FirstHLSSegmentObserved  bool   `json:"firstHlsSegmentObserved"`
	FirstHLSSegmentAgeMS     int64  `json:"firstHlsSegmentAgeMs,omitempty"`
	FailureClass             string `json:"failureClass"`
}

// AirPlayMirrorDiagnostics supplies a sanitized snapshot from the process
// adapter to the authenticated private worker status route.
type AirPlayMirrorDiagnostics interface {
	AirPlayMirrorSnapshot(time.Time) AirPlayMirrorStatus
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

func spotifyDaemonURL() string {
	if raw := os.Getenv("ZOMBIE_SPOTIFY_DAEMON_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err == nil && u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") && u.Port() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" {
			return raw
		}
	}
	return "http://127.0.0.1:3678"
}

// HandlerWithSpotifyDiagnostics adds only fixed failure categories to the
// authenticated private worker health route; raw daemon output is never served.
func HandlerWithSpotifyDiagnostics(ctx context.Context, c Config, diagnostics *SpotifyDaemonDiagnostics) http.Handler {
	return HandlerWithAirPlayProgress(ctx, c, diagnostics, NewAirPlayProgress())
}

// HandlerWithAirPlayProgress wires the process-owned, parsed UxPlay progress
// stream into the private worker status route.
func HandlerWithAirPlayProgress(ctx context.Context, c Config, diagnostics *SpotifyDaemonDiagnostics, progress *AirPlayProgress) http.Handler {
	return handlerWithAirPlayDACP(ctx, c, diagnostics, progress, nil, nil)
}

// HandlerWithAirPlayMirrorDiagnostics adds bounded RTP/bridge evidence to the
// private AirPlay status response without changing the gateway-facing model.
func HandlerWithAirPlayMirrorDiagnostics(ctx context.Context, c Config, progress *AirPlayProgress, mirror AirPlayMirrorDiagnostics) http.Handler {
	return handlerWithAirPlayDACPAndMirror(ctx, c, nil, progress, nil, nil, mirror)
}

// handlerWithAirPlayDACP exposes testable DACP dependencies while keeping the
// production handler on the pinned mDNS resolver and direct HTTP transport.
func handlerWithAirPlayDACP(ctx context.Context, c Config, diagnostics *SpotifyDaemonDiagnostics, progress *AirPlayProgress, resolver DACPResolver, dacpHTTP *http.Client) http.Handler {
	return handlerWithAirPlayDACPAndMirror(ctx, c, diagnostics, progress, resolver, dacpHTTP, nil)
}

func handlerWithAirPlayDACPAndMirror(ctx context.Context, c Config, diagnostics *SpotifyDaemonDiagnostics, progress *AirPlayProgress, resolver DACPResolver, dacpHTTP *http.Client, mirror AirPlayMirrorDiagnostics) http.Handler {
	if progress == nil {
		progress = NewAirPlayProgress()
	}
	mux := http.NewServeMux()
	var bridge *spotifyBridge
	var airplayEvidence *airplayConnectionEvidence
	daemonURL := spotifyDaemonURL()
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	proxy := func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
		if err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, daemonURL+r.URL.Path, strings.NewReader(string(body)))
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
		if c.Mode == "spotify" && (res.StatusCode == 200 || res.StatusCode == 204) {
			if r.URL.Path == "/status" {
				if res.StatusCode == 204 {
					if bridge != nil {
						bridge.OnStopped()
					}
					diagnostics.observePlayback(true, false, false)
				} else {
					var st struct {
						Stopped     bool `json:"stopped"`
						Paused      bool `json:"paused"`
						Buffering   bool `json:"buffering"`
						Volume      int  `json:"volume"`
						VolumeSteps int  `json:"volume_steps"`
						Track       *struct {
							URI      string   `json:"uri"`
							Name     string   `json:"name"`
							Cover    *string  `json:"album_cover_url"`
							Artists  []string `json:"artist_names"`
							Duration int64    `json:"duration"`
							Position int64    `json:"position"`
						} `json:"track"`
					}
					if json.Unmarshal(data, &st) != nil {
						http.Error(w, "invalid response", 502)
						return
					}
					if st.Stopped {
						if bridge != nil {
							bridge.OnStopped()
						}
					} else if st.Track != nil && st.Track.URI != "" {
						if bridge != nil {
							bridge.OnTrack(st.Track.URI)
						}
					}
					var audioActive bool
					var encodedBytes uint64
					if bridge != nil {
						audioDiag := bridge.diagnostic()
						audioActive = audioDiag.Active
						encodedBytes = audioDiag.EncodedBytes
					}
					if diagnostics != nil {
						diagnostics.observePlayback(st.Stopped, !st.Stopped && st.Buffering && st.Track == nil, audioActive)
					}
					var snap *spotifyDaemonHealth
					if diagnostics != nil {
						snap = diagnostics.snapshot(!st.Stopped && st.Buffering && st.Track == nil, audioActive)
					}
					stopped := st.Stopped
					buffering := st.Buffering
					if snap != nil && (snap.RefusalLimited || snap.ConsecutiveRefusals > 0 || snap.FailureCounts["audioKeyRefused"] > 0) && encodedBytes == 0 {
						stopped = true
						buffering = false
					}
					// Forward only the fields the gateway needs. The daemon's raw
					// status also contains account, device and track identifiers.
					safe := map[string]any{
						"stopped": stopped, "paused": st.Paused,
						"buffering": buffering, "volume": st.Volume,
						"volume_steps": st.VolumeSteps, "track": nil,
					}
					if st.Track != nil {
						safe["track"] = map[string]any{
							"name": st.Track.Name, "album_cover_url": st.Track.Cover,
							"artist_names": st.Track.Artists, "duration": st.Track.Duration,
							"position": st.Track.Position,
						}
					}
					if snap != nil {
						safe["daemon"] = snap
					}
					if sanitized, err := json.Marshal(safe); err == nil {
						data = sanitized
					} else {
						http.Error(w, "invalid response", 502)
						return
					}
				}
			} else if r.URL.Path == "/player/stop" {
				if bridge != nil {
					bridge.OnStopped()
				}
				if diagnostics != nil {
					diagnostics.observePlayback(true, false, false)
				}
			} else if r.URL.Path == "/player/next" || r.URL.Path == "/player/prev" || r.URL.Path == "/player/seek" {
				if bridge != nil {
					bridge.ResetBuffer()
				}
				if diagnostics != nil {
					diagnostics.ResetRefusals()
				}
			} else if r.URL.Path == "/player/resume" {
				if diagnostics != nil {
					diagnostics.ResetRefusals()
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(data)
	}
	if c.Mode == "spotify" {
		if diagnostics != nil {
			var stopMu sync.Mutex
			diagnostics.SetOnRefusalLimit(func() {
				if !stopMu.TryLock() {
					return
				}
				defer stopMu.Unlock()

				stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer stopCancel()
				req, err := http.NewRequestWithContext(stopCtx, http.MethodPost, daemonURL+"/player/stop", nil)
				if err != nil {
					diagnostics.RecordStopResult(0, "request_error")
					return
				}
				res, doErr := client.Do(req)
				if doErr != nil {
					if errors.Is(doErr, context.DeadlineExceeded) || (stopCtx.Err() == context.DeadlineExceeded) {
						diagnostics.RecordStopResult(0, "timeout")
					} else {
						diagnostics.RecordStopResult(0, "transport_error")
					}
					return
				}
				defer res.Body.Close()
				diagnostics.RecordStopResult(res.StatusCode, "")
				if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusNoContent {
					if bridge != nil {
						bridge.OnStopped()
					}
					diagnostics.observePlayback(true, false, false)
				}
			})
		}
		bridge = newSpotifyBridge(ctx, filepath.Join(c.StateDir, "audio.pcm"))
		if bridge != nil {
			context.AfterFunc(ctx, bridge.Close)
		}
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
			health, err := spotifyHealth(r.Context(), client, daemonURL, c.StateDir)
			if err != nil {
				http.Error(w, "daemon unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			audio := bridge.diagnostic()
			if diagnostics != nil {
				diagnostics.observePlayback(health.Stopped, health.BufferingWithoutTrack, audio.Active)
			}
			snap := diagnostics.snapshot(health.BufferingWithoutTrack, audio.Active)
			if snap != nil && (snap.RefusalLimited || snap.ConsecutiveRefusals > 0 || snap.FailureCounts["audioKeyRefused"] > 0) && audio.EncodedBytes == 0 {
				health.Stopped = true
				health.BufferingWithoutTrack = false
			}
			_ = json.NewEncoder(w).Encode(spotifyWorkerHealth{spotifyHealthResult: health, Audio: audio, Daemon: snap})
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
		airplayEvidence = newAirplayConnectionEvidence()
		artworkEvidence := newAirplayArtworkEvidence()
		dacp := newDACPControllerWithDependencies(filepath.Join(c.StateDir, "receiver.dacp"), resolver, dacpHTTP)
		mux.HandleFunc("GET /pairing", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				PIN string `json:"pin"`
			}{PIN: c.Pin})
		})
		mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
			now := time.Now()
			active, audioFlow := airplayStreamActivity(c.StateDir)
			connected, connectionRevision, connectionKnown := airplayEvidence.observe(c.StateDir, now, audioFlow)
			audioActive := audioFlow && !(connectionKnown && !connected)
			status := map[string]any{"active": active, "audioActive": audioActive, "connected": connected, "connectionKnown": connectionKnown}
			if mirror != nil {
				status["mirrorDiagnostics"] = mirror.AirPlayMirrorSnapshot(now)
			}
			if connected && connectionRevision != "" {
				status["connectionRevision"] = connectionRevision
			}
			metadata := airplayMetadataForConnection(c.StateDir, connected)
			artworkRevision, _ := artworkEvidence.observe(c.StateDir, connectionRevision, metadata, now)
			status["artworkRevision"] = artworkRevision
			trackRevision := ""
			if connected && metadata["title"] != "" {
				trackRevision = airPlayProgressTrackRevision(metadata, connectionRevision)
			}
			progress.ObserveTrack(trackRevision)
			sample := progress.Snapshot(time.Now())
			status["progressKnown"] = sample.Known
			if sample.Known {
				status["positionMs"] = sample.PositionMS
				status["durationMs"] = sample.DurationMS
				status["positionAgeMs"] = sample.AgeMS
			}
			if audioActive || connected {
				if len(metadata) > 0 {
					status["metadata"] = metadata
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(status)
		})
		mux.HandleFunc("POST /control", func(w http.ResponseWriter, r *http.Request) {
			var input struct {
				Command DACPCommand `json:"command"`
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1024)
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if _, ok := dacpCommandPaths[input.Command]; !ok {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			connected, _, known := airplayEvidence.observe(c.StateDir, time.Now(), false)
			if !known || !connected {
				http.Error(w, "receiver unavailable", http.StatusConflict)
				return
			}
			if err := dacp.Send(r.Context(), input.Command); err != nil {
				switch {
				case errors.Is(err, errInvalidDACPFile), errors.Is(err, errDACPUnavailable), errors.Is(err, errDACPChanged):
					http.Error(w, "receiver unavailable", http.StatusConflict)
				case errors.Is(err, errDACPCommand):
					http.Error(w, "invalid request", http.StatusBadRequest)
				case errors.Is(err, context.DeadlineExceeded):
					http.Error(w, "receiver timeout", http.StatusGatewayTimeout)
				default:
					http.Error(w, "receiver command failed", http.StatusBadGateway)
				}
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("GET /artwork", func(w http.ResponseWriter, r *http.Request) {
			active, audioFlow := airplayStreamActivity(c.StateDir)
			connected, connectionRevision, connectionKnown := airplayEvidence.observe(c.StateDir, time.Now(), audioFlow)
			audioActive := audioFlow && !(connectionKnown && !connected)
			if !active && !audioActive && !connected {
				http.NotFound(w, r)
				return
			}
			metadata := airplayMetadataForConnection(c.StateDir, connected)
			revision, data := artworkEvidence.observe(c.StateDir, connectionRevision, metadata, time.Now())
			if revision == "" || r.URL.Query().Get("rev") != revision || len(data) == 0 {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", http.DetectContentType(data))
			_, _ = w.Write(data)
		})
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

func airplayStreamActivity(stateDir string) (video, audio bool) {
	hlsDir := filepath.Join(stateDir, "hls")
	video = airplayHLSPlayable(hlsDir, "index.m3u8")
	audio = airplayHLSPlayable(hlsDir, "audio.m3u8")
	return video, audio
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
