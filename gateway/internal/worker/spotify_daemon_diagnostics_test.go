package worker

import (
	"encoding/json"
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

func TestSpotifyDaemonDiagnosticsStopsSkipStormAtRefusalLimit(t *testing.T) {
	d := NewSpotifyDaemonDiagnostics()
	called := 0
	doneCh := make(chan struct{}, 1)
	d.SetOnRefusalLimit(func() {
		called++
		select {
		case doneCh <- struct{}{}:
		default:
		}
	})

	private := "spotify:track:private-uri private-account secret-token"
	refusalLine := "skipping track: Spotify refused the audio key (code 1) for this playback context: " + private + "\n"

	// 1st refusal: below threshold (spotifyMaxConsecutiveRefusals = 3)
	d.Write([]byte(refusalLine))
	snap := d.snapshot(true, false)
	if snap.FailureCounts["audioKeyRefused"] != 1 || snap.RefusalLimited || snap.ConsecutiveRefusals != 1 || called != 0 {
		t.Fatalf("1st refusal should not trigger limit: %+v called=%d", snap, called)
	}

	// 2nd refusal: still below threshold
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

	// 4th refusal: bounded callback; should NOT fire again while limit is active
	d.Write([]byte(refusalLine))
	snap = d.snapshot(true, false)
	if snap.FailureCounts["audioKeyRefused"] != 4 || !snap.RefusalLimited || snap.ConsecutiveRefusals != 4 || called != 1 {
		t.Fatalf("subsequent refusal must not re-trigger callback: %+v called=%d", snap, called)
	}

	// Verify zero secret exposure in JSON
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{private, "private-uri", "private-account", "secret-token", "refused the audio key"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("secret leaked in snapshot: %s", raw)
		}
	}

	// Explicit user action permits another bounded attempt.
	d.ResetRefusals()
	snap = d.snapshot(false, false)
	if snap.RefusalLimited || snap.ConsecutiveRefusals != 0 {
		t.Fatalf("user retry must reset refusal limit: %+v", snap)
	}

	// Recovery via active audio flow
	d.Write([]byte(refusalLine))
	snap = d.snapshot(true, false)
	if snap.ConsecutiveRefusals != 1 {
		t.Fatalf("expected 1 consecutive refusal, got %d", snap.ConsecutiveRefusals)
	}
	snap = d.snapshot(false, true) // audioActive = true
	if snap.RefusalLimited || snap.ConsecutiveRefusals != 0 {
		t.Fatalf("active audio must clear consecutive refusals: %+v", snap)
	}
}
