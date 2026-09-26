package worker

import (
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAirplayMetadataIsBoundedOptionalAndFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.txt")
	if len(airplayMetadata(dir)) != 0 {
		t.Fatal("invented metadata")
	}
	os.WriteFile(path, []byte("Title: Track\nArtist: Artist\nAlbum: Album\nUnknown: ignored\n"), 0600)
	if values := airplayMetadata(dir); len(values) != 3 || values["title"] != "Track" {
		t.Fatal(values)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	if len(airplayMetadata(dir)) != 0 {
		t.Fatal("stale metadata exposed")
	}
	os.WriteFile(filepath.Join(dir, "coverart"), []byte("not an image"), 0600)
	response := httptest.NewRecorder()
	airplayArtwork(dir, response, httptest.NewRequest("GET", "/artwork", nil))
	if response.Code != 404 {
		t.Fatal(response.Code)
	}
	placeholder := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 87)...)
	if err := os.WriteFile(filepath.Join(dir, "coverart"), placeholder, 0600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	airplayArtwork(dir, response, httptest.NewRequest("GET", "/artwork", nil))
	if response.Code != 404 {
		t.Fatal("UxPlay placeholder exposed as artwork")
	}
}

func TestAirplayConnectionEvidenceUsesOpaqueEpochAndDebouncesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "receiver.dacp")
	evidence := newAirplayConnectionEvidence()
	now := time.Now()

	connected, revision, known := evidence.observe(dir, now, false)
	if connected || revision != "" || known {
		t.Fatalf("missing connection file reported active: %v %q %v", connected, revision, known)
	}
	firstData := []byte{0x11, '\n', 0x22, '\n'}
	if err := os.WriteFile(path, firstData, 0600); err != nil {
		t.Fatal(err)
	}
	connected, firstRevision, known := evidence.observe(dir, now, false)
	if !connected || !known || len(firstRevision) != 16 {
		t.Fatalf("connection was not represented by a bounded opaque epoch: %v %q", connected, firstRevision)
	}
	if _, err := hex.DecodeString(firstRevision); err != nil {
		t.Fatalf("connection revision is not opaque hex: %q", firstRevision)
	}
	connected, repeatedRevision, known := evidence.observe(dir, now.Add(500*time.Millisecond), false)
	if !connected || !known || repeatedRevision != firstRevision {
		t.Fatal("unchanged connection changed its revision")
	}

	secondData := []byte{0x33, '\n', 0x44, '\n'}
	if err := os.WriteFile(path, secondData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	connected, secondRevision, known := evidence.observe(dir, now.Add(time.Second), false)
	if !connected || !known || secondRevision == firstRevision {
		t.Fatal("changed connection identity did not get a new revision")
	}

	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, secondData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, now.Add(1500*time.Millisecond), now.Add(1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	connected, reconnectRevision, known := evidence.observe(dir, now.Add(1500*time.Millisecond), false)
	if !connected || !known || reconnectRevision == secondRevision {
		t.Fatal("same-identity reconnect did not get a fresh connection epoch")
	}
	if err := os.WriteFile(path, secondData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now.Add(1600*time.Millisecond), now.Add(1600*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	connected, reconnectRevision, known = evidence.observe(dir, now.Add(1600*time.Millisecond), false)
	if !connected || !known || reconnectRevision == secondRevision {
		t.Fatal("same DACP values rewritten on the same inode did not get a fresh connection epoch")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	connected, retainedRevision, known := evidence.observe(dir, now.Add(2*time.Second), true)
	if !connected || !known || retainedRevision != reconnectRevision {
		t.Fatal("brief file absence interrupted the observed connection")
	}
	connected, revision, known = evidence.observe(dir, now.Add(4*time.Second), true)
	if connected || !known || revision != reconnectRevision {
		t.Fatal("known disconnect did not retain its bounded epoch while old flow remained")
	}
	connected, revision, known = evidence.observe(dir, now.Add(5*time.Second), false)
	if connected || known || revision != "" {
		t.Fatal("disconnected sender stayed connected after the grace period")
	}
}

func TestAirplayArtworkEvidenceTracksRewritesAndConnection(t *testing.T) {
	dir := t.TempDir()
	metadataPath := filepath.Join(dir, "metadata.txt")
	coverPath := filepath.Join(dir, "coverart")
	base := time.Now().Add(-40 * time.Second)
	connection := "aaaaaaaaaaaaaaaa"
	evidence := newAirplayArtworkEvidence()
	metadataFor := func(title, artist, album string, modified time.Time) map[string]string {
		t.Helper()
		if err := os.WriteFile(metadataPath, []byte("Title: "+title+"\nArtist: "+artist+"\nAlbum: "+album+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(metadataPath, modified, modified); err != nil {
			t.Fatal(err)
		}
		return airplayMetadata(dir)
	}
	metadata := func(title string, modified time.Time) map[string]string {
		return metadataFor(title, "Artist", "Album", modified)
	}
	cover := func(value byte, modified time.Time) {
		t.Helper()
		// A valid PNG signature is enough to exercise association, independent of decoding.
		if err := os.WriteFile(coverPath, append([]byte("\x89PNG\r\n\x1a\n"), value), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(coverPath, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	observe := func(label map[string]string, at time.Time) string {
		t.Helper()
		revision, _ := evidence.observe(dir, connection, label, at)
		return revision
	}

	trackA := metadata("Track A", base)
	cover('A', base.Add(-2500*time.Millisecond))
	if revision := observe(trackA, base); revision != "" {
		t.Fatal("early cover was accepted before a later write")
	}
	cover('A', base.Add(time.Second))
	if revision := observe(trackA, base.Add(time.Second)); revision != "" {
		t.Fatal("cover was accepted before the candidate hold")
	}
	revisionA := observe(trackA, base.Add(6*time.Second))
	if len(revisionA) != 16 {
		t.Fatal("current cover did not become available after a repeated write")
	}
	trackA = metadata("Track A", base.Add(9*time.Second))
	if got := observe(trackA, base.Add(9*time.Second)); got != revisionA {
		t.Fatal("identical metadata rewrite invalidated artwork")
	}

	// The next cover arrives before its metadata. It must never appear under A.
	cover('B', base.Add(12*time.Second))
	if got := observe(trackA, base.Add(12*time.Second)); got != "" {
		t.Fatal("next-track cover appeared under old labels")
	}
	trackB := metadata("Track B", base.Add(14*time.Second+500*time.Millisecond))
	if got := observe(trackB, base.Add(15*time.Second)); got != "" {
		t.Fatal("pre-metadata cover was adopted without a later sender write")
	}
	cover('B', base.Add(16*time.Second))
	if got := observe(trackB, base.Add(16*time.Second)); got != "" {
		t.Fatal("new cover was accepted before the candidate hold")
	}
	revisionB := observe(trackB, base.Add(18*time.Second))
	if len(revisionB) != 16 || revisionB == revisionA {
		t.Fatal("new track did not get new artwork revision")
	}
	trackB = metadata("Track B", base.Add(21*time.Second))
	if got := observe(trackB, base.Add(21*time.Second)); got != revisionB {
		t.Fatal("later identical metadata rewrite hid artwork")
	}

	// New metadata alone cannot adopt a previous cover, even inside five seconds.
	trackC := metadataFor("Track C", "Other Artist", "Other Album", base.Add(22*time.Second))
	if got := observe(trackC, base.Add(22*time.Second)); got != "" {
		t.Fatal("stale previous-track cover was adopted")
	}
	if got := observe(trackC, base.Add(30*time.Second)); got != "" {
		t.Fatal("old cover became valid only by aging")
	}
	connection = "bbbbbbbbbbbbbbbb"
	if got := observe(trackC, base.Add(31*time.Second)); got != "" {
		t.Fatal("previous connection artwork crossed a new session")
	}
}

func TestAirplayArtworkEvidenceVersionsSameLabelRefresh(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-50 * time.Second)
	path := filepath.Join(dir, "metadata.txt")
	if err := os.WriteFile(path, []byte("Title: Same\nArtist: Artist\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, base, base); err != nil {
		t.Fatal(err)
	}
	labels := airplayMetadata(dir)
	evidence := newAirplayArtworkEvidence()
	cover := func(value byte, at time.Time) {
		t.Helper()
		path := filepath.Join(dir, "coverart")
		if err := os.WriteFile(path, append([]byte("\x89PNG\r\n\x1a\n"), value), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	cover('A', base.Add(time.Second))
	_, _ = evidence.observe(dir, "aaaaaaaaaaaaaaaa", labels, base.Add(time.Second))
	original, _ := evidence.observe(dir, "aaaaaaaaaaaaaaaa", labels, base.Add(7*time.Second))
	if original == "" {
		t.Fatal("first artwork never became available")
	}
	cover('B', base.Add(8*time.Second))
	if revision, _ := evidence.observe(dir, "aaaaaaaaaaaaaaaa", labels, base.Add(8*time.Second)); revision != "" {
		t.Fatal("changed cover reused old revision")
	}
	cover('B', base.Add(10*time.Second))
	if revision, _ := evidence.observe(dir, "aaaaaaaaaaaaaaaa", labels, base.Add(20*time.Second)); revision != "" {
		t.Fatal("cover-only refresh crossed the hold early")
	}
	refreshed, _ := evidence.observe(dir, "aaaaaaaaaaaaaaaa", labels, base.Add(24*time.Second))
	if refreshed == "" || refreshed == original {
		t.Fatal("accepted new artwork reused previous revision")
	}
}

func TestAirplayArtworkEvidenceAcceptsCurrentSessionEarlyCoverWithoutRewrite(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-30 * time.Second)
	connectionPath := filepath.Join(dir, "receiver.dacp")
	if err := os.WriteFile(connectionPath, []byte("id\nremote\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(connectionPath, base.Add(-3*time.Second), base.Add(-3*time.Second)); err != nil {
		t.Fatal(err)
	}
	coverPath := filepath.Join(dir, "coverart")
	cover := []byte("\x89PNG\r\n\x1a\nA")
	if err := os.WriteFile(coverPath, cover, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(coverPath, base.Add(-2500*time.Millisecond), base.Add(-2500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(dir, "metadata.txt")
	if err := os.WriteFile(metadataPath, []byte("Title: New Track\nArtist: Artist\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metadataPath, base, base); err != nil {
		t.Fatal(err)
	}
	labels := airplayMetadata(dir)
	evidence := newAirplayArtworkEvidence()
	if revision, _ := evidence.observe(dir, "aaaaaaaaaaaaaaaa", labels, base); revision != "" {
		t.Fatal("early artwork appeared before association hold")
	}
	revision, data := evidence.observe(dir, "aaaaaaaaaaaaaaaa", labels, base.Add(6*time.Second))
	if len(revision) != 16 || string(data) != string(cover) {
		t.Fatal("current-session early artwork never became available")
	}
	// Rewriting the same metadata later does not invalidate the accepted bytes.
	if err := os.Chtimes(metadataPath, base.Add(9*time.Second), base.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, _ := evidence.observe(dir, "aaaaaaaaaaaaaaaa", labels, base.Add(9*time.Second)); got != revision {
		t.Fatal("identical metadata rewrite cleared early artwork")
	}
}

func TestAirplayArtworkEvidenceReusesSameAlbumCoverAndRejectsUnrelatedStaleCover(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-40 * time.Second)
	metadataPath := filepath.Join(dir, "metadata.txt")
	coverPath := filepath.Join(dir, "coverart")
	evidence := newAirplayArtworkEvidence()
	connection := "aaaaaaaaaaaaaaaa"
	metadata := func(title, artist, album string, at time.Time) map[string]string {
		t.Helper()
		data := []byte("Title: " + title + "\nArtist: " + artist + "\nAlbum: " + album + "\n")
		if err := os.WriteFile(metadataPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(metadataPath, at, at); err != nil {
			t.Fatal(err)
		}
		return airplayMetadata(dir)
	}
	cover := func(at time.Time) {
		t.Helper()
		if err := os.WriteFile(coverPath, []byte("\x89PNG\r\n\x1a\nA"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(coverPath, at, at); err != nil {
			t.Fatal(err)
		}
	}
	observe := func(labels map[string]string, at time.Time) string {
		t.Helper()
		revision, _ := evidence.observe(dir, connection, labels, at)
		return revision
	}

	trackA := metadata("Track A", "Artist", "Album", base)
	cover(base.Add(time.Second))
	if got := observe(trackA, base.Add(time.Second)); got != "" {
		t.Fatal("first cover was accepted before the candidate hold")
	}
	revisionA := observe(trackA, base.Add(6*time.Second))
	if revisionA == "" {
		t.Fatal("first cover did not become available")
	}

	trackB := metadata("Track B", "Artist", "Album", base.Add(8*time.Second))
	if got := observe(trackB, base.Add(8*time.Second)); got != "" {
		t.Fatal("same-album cover skipped the bounded association hold")
	}
	revisionB, reusedCover := evidence.observe(dir, connection, trackB, base.Add(14*time.Second))
	if revisionB == "" || revisionB == revisionA || string(reusedCover) != string([]byte("\x89PNG\r\n\x1a\nA")) {
		t.Fatal("accepted same-album cover was not reused for the new track")
	}

	trackC := metadata("Track C", "Artist", "Album", base.Add(16*time.Second))
	cover(base.Add(17 * time.Second))
	if got := observe(trackC, base.Add(17*time.Second)); got != "" {
		t.Fatal("same-album rewrite skipped the candidate hold")
	}
	revisionC := observe(trackC, base.Add(23*time.Second))
	if revisionC == "" || revisionC == revisionB {
		t.Fatal("fresh same-album cover rewrite was not associated with the new track")
	}

	trackD := metadata("Track D", "Other Artist", "Other Album", base.Add(25*time.Second))
	if got := observe(trackD, base.Add(25*time.Second)); got != "" {
		t.Fatal("stale artwork crossed an unrelated track transition")
	}
	if got := observe(trackD, base.Add(33*time.Second)); got != "" {
		t.Fatal("stale artwork became associated with an unrelated track after aging")
	}
}
