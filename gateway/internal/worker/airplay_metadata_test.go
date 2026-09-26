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

func TestAirplayArtworkRevisionWaitsForCurrentCover(t *testing.T) {
	dir := t.TempDir()
	metadataPath := filepath.Join(dir, "metadata.txt")
	coverPath := filepath.Join(dir, "coverart")
	writeMetadata := func(title string) {
		t.Helper()
		if err := os.WriteFile(metadataPath, []byte("Title: "+title+"\nArtist: Artist\nAlbum: Album\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	serve := func(revision string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		airplayArtworkForRevision(dir, false, revision, response, httptest.NewRequest("GET", "/artwork", nil))
		return response
	}
	cover := []byte("\x89PNG\r\n\x1a\n")
	now := time.Now()
	writeMetadata("Track A")
	if err := os.WriteFile(coverPath, cover, 0600); err != nil {
		t.Fatal(err)
	}
	metadataTime := now.Add(-2 * time.Second)
	if err := os.Chtimes(metadataPath, metadataTime, metadataTime); err != nil {
		t.Fatal(err)
	}
	coverTime := now.Add(-time.Second)
	if err := os.Chtimes(coverPath, coverTime, coverTime); err != nil {
		t.Fatal(err)
	}
	trackARevision := airplayArtworkRevision(airplayMetadata(dir))
	if response := serve(trackARevision); response.Code != 200 {
		t.Fatalf("fresh track A artwork was not served: %d", response.Code)
	}

	writeMetadata("Track B")
	trackBTime := now
	if err := os.Chtimes(metadataPath, trackBTime, trackBTime); err != nil {
		t.Fatal(err)
	}
	trackBRevision := airplayArtworkRevision(airplayMetadata(dir))
	if trackARevision == trackBRevision {
		t.Fatal("metadata revision did not change with the track")
	}
	if response := serve(trackARevision); response.Code != 404 {
		t.Fatalf("old artwork revision was served for new metadata: %d", response.Code)
	}
	if err := os.Chtimes(coverPath, trackBTime, trackBTime); err != nil {
		t.Fatal(err)
	}
	if response := serve(trackBRevision); response.Code != 404 {
		t.Fatalf("equal-timestamp artwork was accepted as fresh: %d", response.Code)
	}
	artworkFirstTime := trackBTime.Add(-time.Second)
	if err := os.Chtimes(coverPath, artworkFirstTime, artworkFirstTime); err != nil {
		t.Fatal(err)
	}
	if response := serve(trackBRevision); response.Code != 404 {
		t.Fatalf("artwork received before metadata was served: %d", response.Code)
	}
	coverTime = trackBTime.Add(time.Second)
	if err := os.Chtimes(coverPath, coverTime, coverTime); err != nil {
		t.Fatal(err)
	}
	if response := serve(trackBRevision); response.Code != 200 {
		t.Fatalf("new track artwork was not served after its file updated: %d", response.Code)
	}
}
