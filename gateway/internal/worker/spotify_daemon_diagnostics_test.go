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
