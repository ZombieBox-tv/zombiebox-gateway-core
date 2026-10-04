package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func soloistSignalBlock() []byte {
	b := make([]byte, 4096)
	for i := 0; i < len(b); i += 4 {
		b[i] = 1
		b[i+2] = 2
	}
	return b
}

func TestSoloistOutputProbeRequiresAdvancingNonSilentPCM(t *testing.T) {
	now := time.Now()
	h := &SoloistPCMHub{listeners: make(map[*soloistPCMListener]struct{}), now: func() time.Time { return now }}
	for i := 0; i < 10; i++ {
		h.publish(make([]byte, 4096))
	}
	if h.Active() {
		t.Fatal("silent sink qualified output")
	}
	for i := 0; i < 4; i++ {
		h.publish(soloistSignalBlock())
	}
	if h.Active() {
		t.Fatal("less than 100 ms qualified output")
	}
	h.publish(soloistSignalBlock())
	if !h.Active() {
		t.Fatal("advancing PCM did not qualify")
	}
	now = now.Add(soloistPCMOutputAge + time.Nanosecond)
	if h.Active() {
		t.Fatal("stale PCM qualified output")
	}
	for i := 0; i < 5; i++ {
		h.publish(soloistSignalBlock())
	}
	h.publish(make([]byte, 4096))
	if h.Active() {
		t.Fatal("silence did not revoke evidence")
	}
	h.ended = true
	if h.Active() {
		t.Fatal("EOF left output ready")
	}
}

func TestSoloistOutputProbeFailsClosedOnTimeoutAndEOF(t *testing.T) {
	reader, writer := io.Pipe()
	h, err := NewSoloistPCMHub(t.Context(), reader)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := h.WaitActive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing output accepted: %v", err)
	}
	_ = writer.Close()
	<-h.done
	if err := h.WaitActive(t.Context()); !errors.Is(err, ErrSoloistReadinessUnverifiable) {
		t.Fatalf("EOF accepted: %v", err)
	}
}

func TestSoloistOutputStreamsExactBytesAndCancelsListener(t *testing.T) {
	reader, writer := io.Pipe()
	h, err := NewSoloistPCMHub(t.Context(), reader)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	defer writer.Close()
	go func() {
		for i := 0; i < 5; i++ {
			_, _ = writer.Write(soloistSignalBlock())
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := h.WaitActive(ctx); err != nil {
		t.Fatal(err)
	}
	streamCtx, stop := context.WithCancel(t.Context())
	stream, err := h.Stream(streamCtx, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Stream(t.Context(), "session-b"); err == nil {
		t.Fatal("second listener admitted")
	}
	block := soloistSignalBlock()
	go func() { _, _ = writer.Write(block) }()
	got := make([]byte, len(block))
	if _, err := io.ReadFull(stream, got); err != nil || !bytes.Equal(got, block) {
		t.Fatalf("PCM changed: %v", err)
	}
	stop()
	result := make(chan error, 1)
	go func() { _, err := stream.Read(make([]byte, 4)); result <- err }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled stream readable")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled listener blocked")
	}
}

func TestSoloistOutputDropsSlowListenerWithoutReplay(t *testing.T) {
	h := &SoloistPCMHub{listeners: make(map[*soloistPCMListener]struct{}), now: time.Now}
	l := &soloistPCMListener{hub: h, chunks: make(chan []byte, 8), closed: make(chan struct{})}
	h.listeners[l] = struct{}{}
	for i := 0; i < 9; i++ {
		h.publish(soloistSignalBlock())
	}
	if len(h.listeners) != 0 {
		t.Fatal("slow listener retained")
	}
	if _, err := l.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("audio exported after disconnect: %v", err)
	}
}

type soloistFallbackTransport func(*http.Request) (*http.Response, error)

func (f soloistFallbackTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSoloistFallbackActivatesOnceAndPreservesPinnedPrimary(t *testing.T) {
	for _, published := range []bool{false, true} {
		var activations, stops atomic.Int32
		client := &http.Client{Transport: soloistFallbackTransport(func(req *http.Request) (*http.Response, error) {
			activations.Add(1)
			if req.URL.Path != "/activate" || req.Header.Get("Authorization") != "Bearer "+soloistBackendTestToken {
				t.Error("unscoped activation")
			}
			return &http.Response{StatusCode: 204, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
		})}
		r := &spotifyFallbackRouter{client: client, endpoint: "http://127.0.0.1:8097", token: soloistBackendTestToken, backend: SpotifyBackendGoLibrespot, now: time.Now,
			primaryHealth: func(context.Context) (spotifyHealthResult, error) {
				return spotifyHealthResult{}, errors.New("daemon unavailable")
			},
			output: func() spotifyAudioDiagnostic {
				if published {
					return spotifyAudioDiagnostic{EncodedBytes: 1}
				}
				return spotifyAudioDiagnostic{}
			},
			refusal: func() bool { return true }, stopPrimary: func() { stops.Add(1) }}
		want := SpotifyBackendSoloist
		if published {
			want = SpotifyBackendGoLibrespot
		}
		for i := 0; i < 4; i++ {
			if got := r.choose(t.Context()); got != want {
				t.Fatalf("chosen %s, want %s", got, want)
			}
		}
		expected := int32(1)
		if published {
			expected = 0
		}
		if activations.Load() != expected || stops.Load() != expected {
			t.Fatalf("activation/stop: %d/%d", activations.Load(), stops.Load())
		}
	}
}

func TestSoloistFallbackWaitsForPrimaryOutputAndIdlePairing(t *testing.T) {
	now := time.Now()
	called := false
	client := &http.Client{Transport: soloistFallbackTransport(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 204, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})}
	health := spotifyHealthResult{Ready: true, Stopped: true}
	r := &spotifyFallbackRouter{client: client, endpoint: "http://127.0.0.1:8097", token: soloistBackendTestToken, backend: SpotifyBackendGoLibrespot, now: func() time.Time { return now },
		primaryHealth: func(context.Context) (spotifyHealthResult, error) { return health, nil }, output: func() spotifyAudioDiagnostic { return spotifyAudioDiagnostic{} }, refusal: func() bool { return false }}
	r.choose(t.Context())
	now = now.Add(time.Hour)
	r.choose(t.Context())
	if called {
		t.Fatal("idle receiver activated fallback")
	}
	health.Stopped = false
	r.choose(t.Context())
	now = now.Add(9 * time.Second)
	r.choose(t.Context())
	if called {
		t.Fatal("primary output grace skipped")
	}
	now = now.Add(time.Second)
	if got := r.choose(t.Context()); got != SpotifyBackendSoloist || !called {
		t.Fatal("stalled output did not activate fallback")
	}
}

func TestSoloistRuntimeRejectsUnauthorizedActivationAndHostBoundary(t *testing.T) {
	r, err := NewSoloistRuntime(t.Context(), soloistBackendTestToken)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	req := httptest.NewRequest(http.MethodPost, "/activate", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 || r.attempted {
		t.Fatal("unauthorized request activated runtime")
	}
	if err := verifySoloistContainer(); err == nil {
		t.Fatal("test PID accepted as container PID 1")
	}
	req.Header.Set("Authorization", "Bearer "+soloistBackendTestToken)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 503 || !r.failed {
		t.Fatal("unverified isolation left runtime ready")
	}
}

func TestSoloistOutputInvalidatesOldSession(t *testing.T) {
	h := &SoloistPCMHub{listeners: make(map[*soloistPCMListener]struct{}), now: time.Now}
	l := &soloistPCMListener{hub: h, chunks: make(chan []byte, 8), closed: make(chan struct{})}
	h.listeners[l] = struct{}{}
	for i := 0; i < 5; i++ {
		h.publish(soloistSignalBlock())
	}
	if !h.Active() {
		t.Fatal("fixture never qualified")
	}
	h.Invalidate()
	if h.Active() {
		t.Fatal("old session evidence survived invalidation")
	}
	if _, err := l.Read(make([]byte, 4)); err != io.EOF {
		t.Fatal("old queued samples survived invalidation")
	}
}

func TestSoloistBackendReadinessRequiresPCMAndRevokesOnLogout(t *testing.T) {
	state := NewSoloistState(nil)
	session := state.BeginSession()
	applySoloistFrame(t, state, session, `{"type":"auth_state","logged_in":true,"is_active":true}`)
	applySoloistFrame(t, state, session, soloistPlaybackFixture("playing", true, 65))
	revision, _, _, err := state.queryRevisions(session)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	current := &soloistBackendConnection{session: session, authRevision: revision, ready: ready}
	b := &SoloistBackend{state: state, current: current}
	h := &SoloistPCMHub{listeners: make(map[*soloistPCMListener]struct{}), now: time.Now}
	b.AttachOutput(h)
	w := httptest.NewRecorder()
	b.serveHealth(w)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"ready":false`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"accountReady":true`)) {
		t.Fatalf("metadata qualified output: %s", w.Body)
	}
	for i := 0; i < 5; i++ {
		h.publish(soloistSignalBlock())
	}
	w = httptest.NewRecorder()
	b.serveHealth(w)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"ready":true`)) || !bytes.Contains(w.Body.Bytes(), []byte(`"audioReady":true`)) {
		t.Fatalf("PCM not wired to readiness: %s", w.Body)
	}
	applySoloistFrame(t, state, session, `{"type":"auth_state","logged_in":false,"is_active":false}`)
	w = httptest.NewRecorder()
	b.serveHealth(w)
	if bytes.Contains(w.Body.Bytes(), []byte(`"ready":true`)) || bytes.Contains(w.Body.Bytes(), []byte(`"audioReady":true`)) {
		t.Fatal("logout retained output readiness")
	}
}

func TestSoloistWorkerProbeEmitsOnlyBoundedEvidence(t *testing.T) {
	client := &http.Client{Transport: soloistFallbackTransport(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer "+soloistBackendTestToken {
			t.Fatal("private bearer missing")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(`{"backend":"soloist","ready":false,"accountReady":true,"audioReady":false,"api_key":"private-key","track":"private-track"}`))}, nil
	})}
	result, err := ProbeSpotifyWorker(t.Context(), Config{Mode: "spotify", Listen: "127.0.0.1:8092", StateDir: "/state", Token: soloistBackendTestToken}, client)
	if err != nil || !result.AccountReady || result.AudioReady {
		t.Fatalf("wrong probe evidence: %+v %v", result, err)
	}
	data, _ := json.Marshal(result)
	if bytes.Contains(data, []byte("private")) || bytes.Contains(data, []byte(soloistBackendTestToken)) {
		t.Fatal("probe leaked private upstream fields")
	}
}
