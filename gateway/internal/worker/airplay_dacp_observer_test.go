package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDACPObserveStatusUsesFixedReadOnlyEndpointAndDiscardsBody(t *testing.T) {
	path := writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n")
	resolver := &fakeDACPResolver{services: []DACPService{matchingDACPService("192.168.1.40")}}
	var requestMethod, requestPath, requestQuery, remote string
	controller := newDACPControllerWithDependencies(path, resolver, &http.Client{
		Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
			requestMethod = request.Method
			requestPath = request.URL.Path
			requestQuery = request.URL.RawQuery
			remote = request.Header.Get("Active-Remote")
			return response(http.StatusOK, "private title and artwork URL", request), nil
		}),
	})

	receivedAt, err := controller.ObserveStatus(context.Background())
	if err != nil || receivedAt.IsZero() {
		t.Fatalf("ObserveStatus() = %v, %v", receivedAt, err)
	}
	if requestMethod != http.MethodGet || requestPath != "/ctrl-int/1/playstatusupdate" || requestQuery != "" || remote != testActiveRemote {
		t.Fatalf("unexpected read-only DACP request: method=%q path=%q query=%q activeRemoteMatches=%v", requestMethod, requestPath, requestQuery, remote == testActiveRemote)
	}
}

func TestDACPObserveStatusRejectsUntrustedEndpointAndResponses(t *testing.T) {
	path := writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n")
	for _, test := range []struct {
		name     string
		service  DACPService
		response *http.Response
		wantErr  error
	}{
		{
			name:    "public endpoint",
			service: matchingDACPService("8.8.8.8"),
			wantErr: errDACPUnavailable,
		},
		{
			name:    "redirect",
			service: matchingDACPService("192.168.1.41"),
			response: func() *http.Response {
				result := response(http.StatusFound, "", nil)
				result.Header.Set("Location", "http://192.168.1.42/ctrl-int/1/playstatusupdate")
				return result
			}(),
			wantErr: errDACPResponse,
		},
		{
			name:     "oversized response",
			service:  matchingDACPService("192.168.1.43"),
			response: response(http.StatusOK, strings.Repeat("x", dacpResponseLimit+1), nil),
			wantErr:  errDACPResponse,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			resolver := &fakeDACPResolver{services: []DACPService{test.service}}
			controller := newDACPControllerWithDependencies(path, resolver, &http.Client{
				Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
					requests++
					if test.response != nil {
						result := *test.response
						result.Request = request
						return &result, nil
					}
					return response(http.StatusOK, "", request), nil
				}),
			})
			_, err := controller.ObserveStatus(context.Background())
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ObserveStatus() error = %v, want %v", err, test.wantErr)
			}
			if test.name == "public endpoint" && requests != 0 {
				t.Fatalf("untrusted endpoint received %d requests", requests)
			}
		})
	}
}

func TestDACPObserveStatusTimesOut(t *testing.T) {
	path := writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n")
	resolver := &fakeDACPResolver{services: []DACPService{matchingDACPService("192.168.1.44")}}
	controller := newDACPControllerWithDependencies(path, resolver, &http.Client{
		Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
	})
	startedAt := time.Now()
	_, err := controller.ObserveStatus(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ObserveStatus() error = %v, want request timeout", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("DACP timeout took %v, want at most two seconds", elapsed)
	}
}

func TestDACPObserveStatusRejectsResponseFromChangedCredentials(t *testing.T) {
	path := writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n")
	resolver := &fakeDACPResolver{services: []DACPService{matchingDACPService("192.168.1.45")}}
	controller := newDACPControllerWithDependencies(path, resolver, &http.Client{
		Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
			if err := os.WriteFile(path, []byte(testDACPIdentifier+"\n9876543210\n"), 0600); err != nil {
				return nil, err
			}
			return response(http.StatusOK, "old session body", request), nil
		}),
	})
	if _, err := controller.ObserveStatus(context.Background()); !errors.Is(err, errDACPChanged) {
		t.Fatalf("ObserveStatus() accepted stale receiver response: %v", err)
	}
}

func TestDACPObserverCredentialGenerationTracksReconnectAndTeardown(t *testing.T) {
	path := writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n")
	tracker := newDACPStatusSessionTracker(path)
	first := tracker.observe()
	if first == 0 || tracker.observe() != first {
		t.Fatal("unchanged receiver credentials did not keep one active generation")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if generation := tracker.observe(); generation != 0 {
		t.Fatalf("removed receiver credentials left generation %d active", generation)
	}
	if err := os.WriteFile(path, []byte(testDACPIdentifier+"\n"+testActiveRemote+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second := tracker.observe()
	if second <= first {
		t.Fatalf("reconnect generation = %d, want greater than %d", second, first)
	}
}

func TestDACPObserverCancelsReconnectWithoutOverlapAndDiscardsStaleResponse(t *testing.T) {
	var generation atomic.Uint64
	generation.Store(1)
	firstStarted := make(chan struct{})
	firstCancelled := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var calls atomic.Uint32
	var active atomic.Int32
	var maximumActive atomic.Int32
	probe := func(ctx context.Context) (time.Time, error) {
		call := calls.Add(1)
		inUse := active.Add(1)
		for {
			observed := maximumActive.Load()
			if inUse <= observed || maximumActive.CompareAndSwap(observed, inUse) {
				break
			}
		}
		defer active.Add(-1)
		if call == 1 {
			close(firstStarted)
			<-ctx.Done()
			close(firstCancelled)
			<-releaseFirst // Deliberately ignores cancellation to test stale-result handling.
			return time.Now(), nil
		}
		close(secondStarted)
		return time.Now(), nil
	}
	observer := newAirPlayDACPObserverWithProbe(probe)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		observer.run(ctx, generation.Load, 5*time.Millisecond, time.Hour, 10*time.Second)
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("first generation was not probed")
	}
	generation.Store(2)
	select {
	case <-firstCancelled:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("generation change did not cancel the in-flight request")
	}
	if calls.Load() != 1 {
		cancel()
		t.Fatalf("new generation overlapped stale request: calls=%d", calls.Load())
	}
	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("new generation was not probed after stale request completed")
	}
	if !waitForDACPObserver(t, observer, func(status AirPlayDACPDiagnosticStatus) bool {
		return status.Generation == 2 && status.StaleResponseCount == 1 && status.SuccessCount == 1
	}) {
		cancel()
		t.Fatal("stale response was not discarded while current generation succeeded")
	}
	if maximumActive.Load() != 1 {
		cancel()
		t.Fatalf("maximum concurrent DACP requests = %d, want 1", maximumActive.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer did not stop after cancellation")
	}
}

func TestDACPObserverCancelsWhenReceiverSessionTearsDown(t *testing.T) {
	var generation atomic.Uint64
	generation.Store(1)
	requestStarted := make(chan struct{})
	requestCancelled := make(chan struct{})
	observer := newAirPlayDACPObserverWithProbe(func(ctx context.Context) (time.Time, error) {
		close(requestStarted)
		<-ctx.Done()
		close(requestCancelled)
		return time.Time{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		observer.run(ctx, generation.Load, 5*time.Millisecond, time.Hour, 10*time.Second)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("receiver session did not start a status request")
	}
	generation.Store(0)
	select {
	case <-requestCancelled:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("receiver teardown did not cancel the status request")
	}
	if !waitForDACPObserver(t, observer, func(status AirPlayDACPDiagnosticStatus) bool {
		return status.Generation == 0 && status.Stage == "waiting_for_session"
	}) {
		cancel()
		t.Fatal("receiver teardown did not clear the active diagnostic generation")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("observer did not stop after cancellation")
	}
}

func waitForDACPObserver(t *testing.T, observer *AirPlayDACPObserver, predicate func(AirPlayDACPDiagnosticStatus) bool) bool {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate(observer.AirPlayDACPDiagnosticSnapshot(time.Now())) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestAirPlayStatusExposesOnlyBoundedDiagnostics(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "receiver.dacp"), []byte(testDACPIdentifier+"\n"+testActiveRemote+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	observer := newAirPlayDACPObserverWithProbe(func(context.Context) (time.Time, error) { return time.Time{}, nil })
	config := Config{Mode: "airplay", Token: strings.Repeat("t", 32), StateDir: dir, Pin: "0427"}
	handler := handlerWithAirPlayDACPAndDiagnostics(context.Background(), config, nil, NewAirPlayProgress(), nil, nil, nil, observer)
	request := httptest.NewRequest(http.MethodGet, "/status", nil)
	request.Header.Set("Authorization", "Bearer "+config.Token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status response = %d: %s", response.Code, response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	var diagnostics AirPlayDACPDiagnosticStatus
	if err := json.Unmarshal(payload["dacpDiagnostics"], &diagnostics); err != nil || !diagnostics.Enabled || diagnostics.Stage != "waiting_for_session" {
		t.Fatalf("missing typed DACP diagnostics: %+v, err=%v", diagnostics, err)
	}
	var artwork AirPlayArtworkDiagnosticStatus
	if err := json.Unmarshal(payload["artworkDiagnostics"], &artwork); err != nil || artwork.Stage != "waiting_for_candidate" {
		t.Fatalf("missing bounded artwork diagnostics: %+v, err=%v", artwork, err)
	}
	for _, secret := range []string{testDACPIdentifier, testActiveRemote, "playstatusupdate", "private title", "http://"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("private DACP or artwork data leaked through status: %q", secret)
		}
	}
}

func TestDACPObserverCountersSaturate(t *testing.T) {
	observer := newAirPlayDACPObserverWithProbe(nil)
	observer.status.RequestCount = ^uint8(0) - 1
	observer.recordRequest(0)
	observer.recordRequest(0)
	if observer.status.RequestCount != ^uint8(0) {
		t.Fatalf("request counter wrapped: %d", observer.status.RequestCount)
	}
}

var _ AirPlayDACPDiagnostics = (*AirPlayDACPObserver)(nil)
