package worker

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSpotifyDaemonDiagnosticsClassifiesWithoutRetainingRawOutput(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newSpotifyDaemonDiagnostics(func() time.Time { return now })
	private := "spotify:track:private-uri private-account secret-token"
	for _, part := range []string{
		"current track unplayable: ",
		"Spotify refused the audio key (code 1) for this playback context: " + private + "\n",
		"failed resolving track storage: " + private + "\n",
		"vorbis decoder failed: " + private + "\n",
		"broken pipe: " + private + "\n",
		"no supported formats: " + private + "\n",
		"failed loading current track: " + private + "\n",
	} {
		if n, err := d.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("stderr must be drained: n=%d err=%v", n, err)
		}
	}
	snapshot := d.snapshot(true, false)
	for _, name := range spotifyFailureNames {
		if snapshot.FailureCounts[name] != 1 {
			t.Fatalf("missing fixed category %s: %+v", name, snapshot)
		}
	}
	if snapshot.StalledBuffering {
		t.Fatal("buffering must not be called stalled on the first observation")
	}
	now = now.Add(spotifyStallThreshold + time.Second)
	if !d.snapshot(true, false).StalledBuffering {
		t.Fatal("continuous trackless buffering must be reported")
	}
	if d.snapshot(false, false).StalledBuffering {
		t.Fatal("stalled state must clear when buffering ends")
	}
	raw, err := json.Marshal(d.snapshot(false, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"private-uri", "private-account", "secret-token", "spotify:track", "refused the audio key"} {
		if strings.Contains(string(raw), value) {
			t.Fatalf("daemon output leaked through diagnostics: %s", raw)
		}
	}
}

func TestSpotifyDaemonDiagnosticsBoundsLongLinesAndAudioRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newSpotifyDaemonDiagnostics(func() time.Time { return now })
	longLine := strings.Repeat("x", 5000) + "refused the audio key\n"
	d.Write([]byte(longLine))
	if d.snapshot(false, false).FailureCounts["audioKeyRefused"] != 0 {
		t.Fatal("unbounded line tail should be discarded")
	}
	d.Write([]byte("refused the audio key\n"))
	if d.snapshot(true, false).FailureCounts["audioKeyRefused"] != 1 {
		t.Fatal("classification did not resume after long line")
	}
	now = now.Add(spotifyStallThreshold + time.Second)
	if d.snapshot(true, true).StalledBuffering {
		t.Fatal("active encoded audio must clear stalled state")
	}
}

func TestSpotifyDaemonDiagnosticsClassifiesPinnedKeyRetrievalErrors(t *testing.T) {
	d := NewSpotifyDaemonDiagnostics()
	private := "spotify:track:private-uri private-account secret-token"
	for _, part := range []string{
		"failed retrieving aes key with ",
		"code 4: " + private + "\n",
		"failed retrieving audio ",
		"key: " + private + "\n",
	} {
		if n, err := d.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("stderr must be drained: n=%d err=%v", n, err)
		}
	}
	raw, err := json.Marshal(d.snapshot(false, false))
	if err != nil {
		t.Fatal(err)
	}
	if d.snapshot(false, false).FailureCounts["audioKeyRefused"] != 2 {
		t.Fatalf("pinned key errors not classified: %s", raw)
	}
	for _, value := range []string{private, "private-uri", "private-account", "secret-token", "failed retrieving"} {
		if strings.Contains(string(raw), value) {
			t.Fatalf("daemon output leaked through diagnostics: %s", raw)
		}
	}
}

func TestSpotifyDaemonDiagnosticsCountsPairingOutcomesWithoutRetainingSenderData(t *testing.T) {
	d := NewSpotifyDaemonDiagnostics()
	privateDevice := "Kitchen iPad private-user private-auth-blob"
	for _, part := range []string{
		"accepted zeroconf from " + privateDevice + "\n",
		"refused zeroconf from " + privateDevice + "\n",
		"zeroconf received request with bad checksum\n",
		"zeroconf is authenticating another user\n",
		"failed handling zeroconf add user request: " + privateDevice + "\n",
	} {
		if n, err := d.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("stderr must be drained: n=%d err=%v", n, err)
		}
	}

	snapshot := d.snapshot(false, false)
	want := (spotifyPairingDiagnostics{Accepted: 1, Refused: 1, BadChecksum: 1, Busy: 1, RequestError: 1})
	if snapshot.Pairing != want {
		t.Fatalf("pairing outcomes = %+v, want %+v", snapshot.Pairing, want)
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"Kitchen iPad", "private-user", "private-auth-blob", "accepted zeroconf", "refused zeroconf"} {
		if strings.Contains(string(raw), value) {
			t.Fatalf("sender data or raw log text leaked through pairing diagnostics: %s", raw)
		}
	}
}

func TestSpotifyDaemonDiagnosticsStopsSkipStormAtRefusalLimit(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newSpotifyDaemonDiagnostics(func() time.Time { return now })
	called := 0
	doneCh := make(chan struct{}, 1)
	d.SetOnRefusalLimit(func() {
		called++
		d.RecordStopResult(http.StatusOK, "")
		select {
		case doneCh <- struct{}{}:
		default:
		}
	})

	private := "spotify:track:private-uri private-account secret-token"
	refusalLine := "skipping track: Spotify refused the audio key (code 1) for this playback context: " + private + "\n"

	// 1. First refusal sequence: below threshold (spotifyMaxConsecutiveRefusals = 3)
	d.Write([]byte(refusalLine))
	snap := d.snapshot(true, false)
	if snap.FailureCounts["audioKeyRefused"] != 1 || snap.RefusalLimited || snap.ConsecutiveRefusals != 1 || called != 0 {
		t.Fatalf("1st refusal should not trigger limit: %+v called=%d", snap, called)
	}

	d.Write([]byte(refusalLine))
	snap = d.snapshot(true, false)
	if snap.FailureCounts["audioKeyRefused"] != 2 || snap.RefusalLimited || snap.ConsecutiveRefusals != 2 || called != 0 {
		t.Fatalf("2nd refusal should not trigger limit: %+v called=%d", snap, called)
	}

	// 3rd refusal: threshold reached -> triggers refusal limit callback
	d.Write([]byte(refusalLine))
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for refusal limit callback")
	}
	snap = d.snapshot(true, false)
	if snap.FailureCounts["audioKeyRefused"] != 3 || !snap.RefusalLimited || snap.ConsecutiveRefusals != 3 || called != 1 {
		t.Fatalf("3rd refusal must trigger limit: %+v called=%d", snap, called)
	}
	if snap.StopAttempts != 1 || snap.LastStopResult != "ok" || snap.LastStopStatus != http.StatusOK {
		t.Fatalf("expected recorded stop callback result: %+v", snap)
	}

	// 2. Overlapping in-flight refusal: min stop interval prevents rapid re-triggering
	d.Write([]byte(refusalLine))
	snap = d.snapshot(true, false)
	if snap.FailureCounts["audioKeyRefused"] != 4 || !snap.RefusalLimited || snap.ConsecutiveRefusals != 4 || called != 1 {
		t.Fatalf("subsequent refusal must not re-trigger callback before interval: %+v called=%d", snap, called)
	}

	// 3. Playback becomes stopped; refusal remains visible
	d.observePlayback(true, false, false)
	if !d.snapshot(false, false).RefusalLimited {
		t.Fatal("keep the refusal visible while playback is stopped")
	}

	// 4. Fresh auto-reconnect after stop within cooldown window (e.g. 2s < 30s):
	// circuit breaker remains open to block skip cascades across repeated reconnects.
	now = now.Add(2 * time.Second)
	d.observePlayback(false, true, false)
	if !d.snapshot(true, false).RefusalLimited {
		t.Fatal("circuit breaker must remain open during cooldown on automatic reconnect")
	}

	// 5. Further refused keys while circuit open: re-asserts stop after paced interval
	now = now.Add(1 * time.Second) // total 3s >= spotifyMinStopInterval (1s)
	d.Write([]byte(refusalLine))
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second stop on auto-reconnect skip")
	}
	if called != 2 {
		t.Fatalf("expected stop re-asserted for refused auto-reconnect, called=%d", called)
	}
	snap = d.snapshot(true, false)
	if snap.StopAttempts != 2 || snap.LastStopResult != "ok" || snap.LastStopStatus != http.StatusOK {
		t.Fatalf("expected updated stop attempts: %+v", snap)
	}

	// Immediate next in-flight refusal from second context: paced interval prevents spam
	d.Write([]byte(refusalLine))
	if called != 2 {
		t.Fatalf("expected no redundant stop within min interval, called=%d", called)
	}

	// 6. Explicit user action permits another bounded attempt.
	d.ResetRefusals()
	snap = d.snapshot(false, false)
	if snap.RefusalLimited || snap.ConsecutiveRefusals != 0 {
		t.Fatalf("user retry must reset refusal limit: %+v", snap)
	}

	// 7. Intentional later retry after circuit cooldown:
	// Cause limit to trip again
	for i := 0; i < spotifyMaxConsecutiveRefusals; i++ {
		d.Write([]byte(refusalLine))
	}
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for 3rd stop")
	}
	if called != 3 {
		t.Fatalf("expected 3rd stop call, got %d", called)
	}
	d.observePlayback(true, false, false)
	// Now advance time past the 30s cooldown
	now = now.Add(spotifyCircuitCooldown + 5*time.Second)
	d.observePlayback(false, true, false)
	snap = d.snapshot(true, false)
	if snap.RefusalLimited || snap.ConsecutiveRefusals != 0 {
		t.Fatalf("playback attempt after cooldown must re-arm the refusal limit: %+v", snap)
	}

	// 8. Recovery via active audio flow clears cumulative failure counts
	d.Write([]byte(refusalLine))
	snap = d.snapshot(true, false)
	if snap.ConsecutiveRefusals != 1 || snap.FailureCounts["audioKeyRefused"] == 0 {
		t.Fatalf("expected 1 refusal count, got %+v", snap)
	}
	snap = d.snapshot(false, true) // audioActive = true
	if snap.RefusalLimited || snap.ConsecutiveRefusals != 0 {
		t.Fatalf("active audio must clear consecutive refusals: %+v", snap)
	}
	if snap.FailureCounts["audioKeyRefused"] != 0 {
		t.Fatalf("active audio recovery must clear stale cumulative error counts: %+v", snap)
	}

	// 9. Verify zero secret exposure in JSON
	raw, err := json.Marshal(d.snapshot(false, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{private, "private-uri", "private-account", "secret-token", "refused the audio key"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("secret leaked in snapshot: %s", raw)
		}
	}
}

func TestSpotifyDaemonDiagnosticsStopCallbackResults(t *testing.T) {
	d := NewSpotifyDaemonDiagnostics()

	// 200 OK
	d.RecordStopResult(http.StatusOK, "")
	snap := d.snapshot(false, false)
	if snap.LastStopResult != "ok" || snap.LastStopStatus != 200 {
		t.Fatalf("expected ok result, got %+v", snap)
	}

	// 204 No Content
	d.RecordStopResult(http.StatusNoContent, "")
	snap = d.snapshot(false, false)
	if snap.LastStopResult != "no_session" || snap.LastStopStatus != 204 {
		t.Fatalf("expected no_session result, got %+v", snap)
	}

	// 500 HTTP Error
	d.RecordStopResult(http.StatusInternalServerError, "")
	snap = d.snapshot(false, false)
	if snap.LastStopResult != "http_error" || snap.LastStopStatus != 500 {
		t.Fatalf("expected http_error result, got %+v", snap)
	}

	// Timeout
	d.RecordStopResult(0, "timeout")
	snap = d.snapshot(false, false)
	if snap.LastStopResult != "timeout" || snap.LastStopStatus != 0 {
		t.Fatalf("expected timeout result, got %+v", snap)
	}

	// Transport error
	d.RecordStopResult(0, "transport_error")
	snap = d.snapshot(false, false)
	if snap.LastStopResult != "transport_error" || snap.LastStopStatus != 0 {
		t.Fatalf("expected transport_error result, got %+v", snap)
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"http://", "127.0.0.1", "secret", "token"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("leaked forbidden string in stop diagnostics: %s", raw)
		}
	}
}
