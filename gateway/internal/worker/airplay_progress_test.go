package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleAirPlayProgress = "audio progress (min:sec):   2:03; remaining:   3:57; track length 6:00\r"

func TestAirPlayProgressParserAcceptsBoundedUxPlaySample(t *testing.T) {
	progress := NewAirPlayProgress()
	progress.ObserveTrack("track-a")
	if _, err := progress.Write([]byte(sampleAirPlayProgress)); err != nil {
		t.Fatal(err)
	}

	sample := progress.Snapshot(time.Now())
	if !sample.Known || sample.PositionMS != 123000 || sample.DurationMS != 360000 || sample.AgeMS < 0 || sample.AgeMS > 5000 {
		t.Fatalf("unexpected sender position: %+v", sample)
	}

	maxDuration := "audio progress (min:sec): 0:00; remaining: 1440:00; track length 1440:00\r"
	if _, err := progress.Write([]byte(maxDuration)); err != nil {
		t.Fatal(err)
	}
	sample = progress.Snapshot(time.Now())
	if !sample.Known || sample.PositionMS != 0 || sample.DurationMS != int64(24*time.Hour/time.Millisecond) {
		t.Fatalf("bounded 24-hour sample was not accepted: %+v", sample)
	}
}

func TestAirPlayProgressParserDiscardsMalformedAndSecretOutput(t *testing.T) {
	progress := NewAirPlayProgress()
	progress.ObserveTrack("track-a")
	lines := []string{
		"password=private-output\r",
		"audio progress (min:sec): -1:00; remaining: 4:00; track length 3:00\r",
		"audio progress (min:sec): 4:00; remaining: 0:00; track length 3:00\r",
		"audio progress (min:sec): 1:60; remaining: 1:00; track length 3:00\r",
		"audio progress (min:sec): 1:00; remaining: 1:59; track length 3:00\r",
		"audio progress (min:sec): 0:00; remaining: 1441:00; track length 1441:00\r",
		"private=" + strings.Repeat("s", maxAirPlayProgressLineBytes+20) + "\r",
	}
	for _, line := range lines {
		if _, err := progress.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if sample := progress.Snapshot(time.Now()); sample.Known {
		t.Fatalf("malformed or oversized stdout created position evidence: %+v", sample)
	}
	raw, err := json.Marshal(progress.Snapshot(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-output") || strings.Contains(string(raw), strings.Repeat("s", 20)) {
		t.Fatalf("raw upstream output escaped the parsed snapshot: %s", raw)
	}
}

func TestAirPlayProgressFencesTrackTransitionsAndPartialLines(t *testing.T) {
	progress := NewAirPlayProgress()
	progress.ObserveTrack("track-a")
	if _, err := progress.Write([]byte(sampleAirPlayProgress)); err != nil {
		t.Fatal(err)
	}
	if sample := progress.Snapshot(time.Now()); !sample.Known {
		t.Fatal("expected a fresh sample for the first track")
	}

	progress.ObserveTrack("track-b")
	if sample := progress.Snapshot(time.Now()); sample.Known {
		t.Fatalf("previous track's sample survived the revision fence: %+v", sample)
	}

	partial := "audio progress (min:sec):   2:03"
	if _, err := progress.Write([]byte(partial)); err != nil {
		t.Fatal(err)
	}
	progress.ObserveTrack("track-c")
	if _, err := progress.Write([]byte("; remaining:   3:57; track length 6:00\r")); err != nil {
		t.Fatal(err)
	}
	if sample := progress.Snapshot(time.Now()); sample.Known {
		t.Fatalf("a record crossing track revisions was accepted: %+v", sample)
	}

	if _, err := progress.Write([]byte(sampleAirPlayProgress)); err != nil {
		t.Fatal(err)
	}
	if sample := progress.Snapshot(time.Now()); !sample.Known || sample.PositionMS != 123000 {
		t.Fatalf("current track sample was not accepted after the fence: %+v", sample)
	}
}

func TestAirPlayProgressExpiresOldSenderPosition(t *testing.T) {
	progress := NewAirPlayProgress()
	progress.ObserveTrack("track-a")
	if _, err := progress.Write([]byte(sampleAirPlayProgress)); err != nil {
		t.Fatal(err)
	}

	sample := progress.Snapshot(time.Now().Add(maxAirPlayProgressAge + time.Second))
	if sample.Known {
		t.Fatalf("stale sample remained known: %+v", sample)
	}
}

func TestAirPlayProgressTrackRevisionUsesLabelsAndConnection(t *testing.T) {
	metadata := map[string]string{"title": "Same song", "artist": "Artist", "album": "Album"}
	first := airPlayProgressTrackRevision(metadata, "0123456789abcdef")
	rewritten := airPlayProgressTrackRevision(metadata, "0123456789abcdef")
	if first == "" || rewritten == "" || first != rewritten {
		t.Fatalf("identical labels in the same session changed progress revision: %q %q", first, rewritten)
	}

	changedLabels := map[string]string{"title": "Another song", "artist": "Artist", "album": "Album"}
	if revision := airPlayProgressTrackRevision(changedLabels, "0123456789abcdef"); revision == "" || revision == first {
		t.Fatalf("genuine label change did not change progress revision: %q", revision)
	}
	if revision := airPlayProgressTrackRevision(metadata, "fedcba9876543210"); revision == "" || revision == first {
		t.Fatalf("genuine session change did not change progress revision: %q", revision)
	}
	if invalid := airPlayProgressTrackRevision(metadata, "not-a-revision"); invalid != "" {
		t.Fatalf("unvalidated connection revision was accepted: %q", invalid)
	}
}

func TestAirPlayProgressAcceptsBackwardSeekWithoutTrackTransition(t *testing.T) {
	progress := NewAirPlayProgress()
	progress.ObserveTrack("track-a")
	if _, err := progress.Write([]byte(sampleAirPlayProgress)); err != nil {
		t.Fatal(err)
	}
	if sample := progress.Snapshot(time.Now()); !sample.Known || sample.PositionMS != 123000 {
		t.Fatalf("initial sender sample was not accepted: %+v", sample)
	}

	seeked := "audio progress (min:sec): 1:00; remaining: 5:00; track length 6:00\r"
	if _, err := progress.Write([]byte(seeked)); err != nil {
		t.Fatal(err)
	}
	if sample := progress.Snapshot(time.Now()); !sample.Known || sample.PositionMS != 60000 {
		t.Fatalf("backward seek did not replace the authoritative sender position: %+v", sample)
	}
}

func TestAirPlayProgressObserveTrackClearsOnLabelOrSessionChange(t *testing.T) {
	metadata := map[string]string{"title": "Same song", "artist": "Artist", "album": "Album"}
	connection := "0123456789abcdef"
	progress := NewAirPlayProgress()
	progress.ObserveTrack(airPlayProgressTrackRevision(metadata, connection))
	if _, err := progress.Write([]byte(sampleAirPlayProgress)); err != nil {
		t.Fatal(err)
	}

	metadata["title"] = "Another song"
	labelRevision := airPlayProgressTrackRevision(metadata, connection)
	progress.ObserveTrack(labelRevision)
	if sample := progress.Snapshot(time.Now()); sample.Known {
		t.Fatalf("sender sample survived a genuine label transition: %+v", sample)
	}
	if _, err := progress.Write([]byte(sampleAirPlayProgress)); err != nil {
		t.Fatal(err)
	}

	sessionRevision := airPlayProgressTrackRevision(metadata, "fedcba9876543210")
	progress.ObserveTrack(sessionRevision)
	if sample := progress.Snapshot(time.Now()); sample.Known {
		t.Fatalf("sender sample survived a genuine connection transition: %+v", sample)
	}
}

func TestAirPlayStatusAssociatesProgressWithCurrentConnectionAndTrack(t *testing.T) {
	dir := t.TempDir()
	connectionPath := filepath.Join(dir, "receiver.dacp")
	metadataPath := filepath.Join(dir, "metadata.txt")
	if err := os.WriteFile(connectionPath, []byte("private-dacp-a\nprivate-dacp-b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte("Title: First song\nArtist: Artist\nAlbum: Album\n"), 0600); err != nil {
		t.Fatal(err)
	}
	connectionTime := time.Now().Add(-2 * time.Second)
	metadataTime := connectionTime.Add(time.Second)
	if err := os.Chtimes(connectionPath, connectionTime, connectionTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metadataPath, metadataTime, metadataTime); err != nil {
		t.Fatal(err)
	}

	progress := NewAirPlayProgress()
	config := Config{Mode: "airplay", Token: strings.Repeat("t", 32), StateDir: dir, Pin: "1234"}
	handler := HandlerWithAirPlayProgress(context.Background(), config, nil, progress)
	getStatus := func() map[string]json.RawMessage {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/status", nil)
		request.Header.Set("Authorization", "Bearer "+config.Token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status returned %d: %s", response.Code, response.Body.String())
		}
		var values map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &values); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(response.Body.String(), "private-dacp") || strings.Contains(response.Body.String(), config.Token) {
			t.Fatalf("private worker data escaped status: %s", response.Body.String())
		}
		return values
	}
	assertProgress := func(fields map[string]json.RawMessage, known bool, position int64) {
		t.Helper()
		var actual bool
		if err := json.Unmarshal(fields["progressKnown"], &actual); err != nil || actual != known {
			t.Fatalf("progressKnown=%v, want %v (%v)", actual, known, err)
		}
		if !known {
			if _, present := fields["positionMs"]; present {
				t.Fatalf("unknown sender position was serialized as a value: %s", fields["positionMs"])
			}
			return
		}
		var actualPosition int64
		var duration, age int64
		if err := json.Unmarshal(fields["positionMs"], &actualPosition); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(fields["durationMs"], &duration); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(fields["positionAgeMs"], &age); err != nil {
			t.Fatal(err)
		}
		if actualPosition != position || duration != 360000 || age < 0 || age > 5000 {
			t.Fatalf("unexpected progress fields: position=%d duration=%d age=%d", actualPosition, duration, age)
		}
	}

	assertProgress(getStatus(), false, 0)
	if _, err := progress.Write([]byte(sampleAirPlayProgress)); err != nil {
		t.Fatal(err)
	}
	assertProgress(getStatus(), true, 123000)

	// UxPlay can rewrite metadata several seconds after it writes the cover art.
	// The file timestamp is not a track identity, so this must retain the sample.
	if err := os.WriteFile(metadataPath, []byte("Title: First song\nArtist: Artist\nAlbum: Album\n"), 0600); err != nil {
		t.Fatal(err)
	}
	metadataTime = metadataTime.Add(2 * time.Second)
	if err := os.Chtimes(metadataPath, metadataTime, metadataTime); err != nil {
		t.Fatal(err)
	}
	assertProgress(getStatus(), true, 123000)

	if err := os.WriteFile(metadataPath, []byte("Title: Second song\nArtist: Artist\nAlbum: Album\n"), 0600); err != nil {
		t.Fatal(err)
	}
	metadataTime = metadataTime.Add(2 * time.Second)
	if err := os.Chtimes(metadataPath, metadataTime, metadataTime); err != nil {
		t.Fatal(err)
	}
	assertProgress(getStatus(), false, 0)
	if _, err := progress.Write([]byte(sampleAirPlayProgress)); err != nil {
		t.Fatal(err)
	}
	assertProgress(getStatus(), true, 123000)
}
