package artwork

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestPersistentDerivativeProfilesAndPrivateIdentity(t *testing.T) {
	directory := t.TempDir()
	var original bytes.Buffer
	_ = jpeg.Encode(&original, image.NewRGBA(image.Rect(0, 0, 1200, 800)), nil)
	calls := 0
	client := doFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(original.Bytes()))}, nil
	})
	source := domain.Source{ArtworkURL: "https://provider.invalid/private.jpg", ArtworkHeaders: http.Header{"Cookie": {"session=secret"}}}
	for iteration := 0; iteration < 2; iteration++ {
		disk, err := NewDiskCache(directory, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		images := New(client, disk) // Recreate both tiers, as after a process restart.
		for profile, bounds := range map[domain.ArtworkProfile][2]int{
			domain.ArtworkLandscapeSmall: {240, 135}, domain.ArtworkLandscapeMedium: {320, 180},
			domain.ArtworkHeroSmall: {640, 360}, domain.ArtworkHeroMedium: {960, 540},
			domain.ArtworkPosterSmall: {180, 270}, domain.ArtworkPosterMedium: {320, 480},
		} {
			data, err := images.Image(t.Context(), source, profile)
			if err != nil {
				t.Fatal(err)
			}
			config, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || config.Width > bounds[0] || config.Height > bounds[1] {
				t.Fatal(profile, config, err)
			}
		}
	}
	if calls != 6 {
		t.Fatal("restart fetched already processed artwork", calls)
	}
	disk, _ := NewDiskCache(directory, 1<<20)
	source.ArtworkHeaders.Set("Cookie", "session=changed")
	if _, err := New(client, disk).Image(t.Context(), source, domain.ArtworkHeroSmall); err != nil {
		t.Fatal(err)
	}
	if calls != 7 {
		t.Fatal("cookie change reused private derivative")
	}
	entries, _ := os.ReadDir(directory)
	for _, file := range entries {
		if len(file.Name()) != 70 {
			t.Fatal("non-digest filename", file.Name())
		}
		path := filepath.Join(directory, file.Name())
		info, _ := os.Stat(path)
		data, _ := os.ReadFile(path)
		if info.Mode().Perm() != 0600 || bytes.Contains(data, []byte("session=")) || bytes.Contains(data, []byte("provider.invalid")) {
			t.Fatal("private metadata persisted")
		}
	}
}

func TestDiskExpiryCorruptionAndEviction(t *testing.T) {
	disk, err := NewDiskCache(t.TempDir(), 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 0, 100<<10)
	data = append(data, encoded.Bytes()[:encoded.Len()-2]...)
	for _, payloadLength := range []int{50_000, 50_000} {
		data = append(data, 0xff, 0xfe, byte((payloadLength+2)>>8), byte(payloadLength+2))
		data = append(data, bytes.Repeat([]byte{'x'}, payloadLength)...)
	}
	data = append(data, 0xff, 0xd9)
	first, second, third := sha256.Sum256([]byte("first")), sha256.Sum256([]byte("second")), sha256.Sum256([]byte("third"))
	expires := time.Now().Add(time.Hour)
	disk.Put(first, data, expires)
	disk.Put(second, data, expires)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(disk.path(second), old, old)
	if _, _, ok := disk.Get(first); !ok {
		t.Fatal("cache miss")
	}
	disk.Put(third, data, expires)
	if _, _, ok := disk.Get(second); ok {
		t.Fatal("oldest image not evicted")
	}
	if _, _, ok := disk.Get(first); !ok {
		t.Fatal("recent image evicted")
	}
	header := make([]byte, 8)
	binary.BigEndian.PutUint64(header, uint64(old.Unix()))
	_ = os.WriteFile(disk.path(first), append(header, data...), 0600)
	if _, _, ok := disk.Get(first); ok {
		t.Fatal("expired derivative returned")
	}
	_ = os.WriteFile(disk.path(third), []byte("broken"), 0600)
	if _, _, ok := disk.Get(third); ok {
		t.Fatal("truncated derivative returned")
	}
	restarted, err := NewDiskCache(disk.directory, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(restarted.directory)
	if len(entries) != 0 {
		t.Fatal("startup retained corrupt files", entries)
	}
}
