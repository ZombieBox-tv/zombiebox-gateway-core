package worker

import (
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
