package worker

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// spotifyFallbackRouter activates the separately isolated, optional runtime
// only before audio has been qualified. No active session is switched. A new
// worker lifetime retries go-librespot first; fallback never persists a winner.
type spotifyFallbackRouter struct {
	primary         http.Handler
	client          *http.Client
	streamClient    *http.Client
	endpoint, token string
	primaryHealth   func(context.Context) (spotifyHealthResult, error)
	output          func() spotifyAudioDiagnostic
	refusal         func() bool
	stopPrimary     func()
	mu              sync.Mutex
	backend         SpotifyBackendKind
	pinned          bool
	attempted       bool
	waitingSince    time.Time
	now             func() time.Time
}

func newSpotifyFallbackRouter(primary http.Handler, client *http.Client, c Config, upstream string, bridge *spotifyBridge, diagnostics *SpotifyDaemonDiagnostics) http.Handler {
	endpoint := os.Getenv("ZOMBIE_SOLOIST_URL")
	// The Full runtime publishes its bearer API only on this host loopback
	// port. Never accept an arbitrary URL, provider credential or redirect.
	if endpoint != "http://127.0.0.1:8097" {
		return primary
	}
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: 5 * time.Second, IdleConnTimeout: 30 * time.Second}
	return &spotifyFallbackRouter{
		primary: primary, client: client, streamClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		endpoint: endpoint, token: c.Token, stopPrimary: c.StopSpotify, backend: SpotifyBackendGoLibrespot, now: time.Now,
		primaryHealth: func(ctx context.Context) (spotifyHealthResult, error) {
			return spotifyHealth(ctx, client, upstream, c.StateDir)
		},
		output: func() spotifyAudioDiagnostic { return bridge.diagnostic() },
		refusal: func() bool {
			snapshot := diagnostics.snapshot(false, false)
			return snapshot != nil && (snapshot.RefusalLimited || snapshot.FailureCounts["audioKeyRefused"] > 0)
		},
	}
}

func (r *spotifyFallbackRouter) choose(ctx context.Context) SpotifyBackendKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pinned || r.attempted {
		return r.backend
	}
	output := r.output()
	if output.Active && output.EncodedBytes > 0 {
		r.pinned = true
		return r.backend
	}
	// Previously published output forbids fallback even if the stream is now
	// stale, stopped, or the primary process has crashed.
	if output.EncodedBytes > 0 {
		r.pinned = true
		return r.backend
	}
	health, err := r.primaryHealth(ctx)
	unhealthy := err != nil || r.refusal()
	if !unhealthy && !health.Stopped && !health.Paused && health.Ready {
		if r.waitingSince.IsZero() {
			r.waitingSince = r.now()
		}
		unhealthy = r.now().Sub(r.waitingSince) >= 10*time.Second
	} else if !unhealthy {
		r.waitingSince = time.Time{}
	}
	if !unhealthy {
		return r.backend
	}
	// Only one fallback activation attempt per process lifetime. A failed
	// optional runtime remains unavailable without disturbing other providers.
	r.attempted = true
	if r.stopPrimary != nil {
		r.stopPrimary()
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint+"/activate", nil)
	request.Header.Set("Authorization", "Bearer "+r.token)
	response, activateErr := r.client.Do(request)
	if activateErr != nil {
		return r.backend
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return r.backend
	}
	r.backend = SpotifyBackendSoloist
	return r.backend
}

func (r *spotifyFallbackRouter) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	allowed := request.URL.Path == "/health" || request.URL.Path == "/status" || request.URL.Path == "/audio" || request.URL.Path == "/auth/code" || strings.HasPrefix(request.URL.Path, "/player/")
	if !allowed || r.choose(request.Context()) != SpotifyBackendSoloist {
		r.primary.ServeHTTP(w, request)
		return
	}
	if request.URL.Path == "/auth/code" {
		// Connect onboarding uses the phone. There is no Soloist device code
		// to expose, and the API key never enters this worker or the Gateway.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	client := r.client
	if request.URL.Path == "/audio" {
		client = r.streamClient
	}
	if request.ContentLength > 1024 {
		http.Error(w, "invalid request", 400)
		return
	}
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, r.endpoint+request.URL.Path, http.MaxBytesReader(w, request.Body, 1024))
	if err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	upstream.Header.Set("Authorization", "Bearer "+r.token)
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("X-Zombie-Session", soloistPCMRequestSession(request))
	response, err := client.Do(upstream)
	if err != nil {
		http.Error(w, "Spotify unavailable", 503)
		return
	}
	defer response.Body.Close()
	if request.URL.Path == "/audio" {
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != soloistPCMStreamMIME {
			http.Error(w, "audio unavailable", 503)
			return
		}
		r.mu.Lock()
		r.pinned = true
		r.mu.Unlock()
		w.Header().Set("Content-Type", soloistPCMStreamMIME)
		w.Header().Set("Cache-Control", "no-store, no-cache")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = io.Copy(w, response.Body)
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		http.Error(w, "invalid response", 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}
