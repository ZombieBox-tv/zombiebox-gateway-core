package companionmedia

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadBoundsIsolationAndCleanup(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := strings.Repeat("a", 32)
	file := []byte("\x00\x00\x00\x18ftypisomcontents")
	a, err := s.Put(t.Context(), "owner", id, int64(len(file)), bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if a.MIME != "video/mp4" {
		t.Fatal(a)
	}
	if _, err = s.Get("other", id); err == nil {
		t.Fatal("foreign access")
	}
	s.Remove("other", id)
	if _, err = s.Get("owner", id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Put(t.Context(), "owner", id, 1, strings.NewReader("a")); err != ErrBusy {
		t.Fatal("duplicate", err)
	}
	if err = s.Retain("owner", id); err != nil {
		t.Fatal(err)
	}
	s.RemoveOwner("owner")
	if _, err = os.Stat(a.Path); !os.IsNotExist(err) {
		t.Fatal("file retained", err)
	}
	for _, content := range []string{"#EXTM3U\nfile:///etc/passwd", "https://example.com/video", "<MPD/>", "bad"} {
		if _, err = s.Put(t.Context(), "owner", id, int64(len(content)), strings.NewReader(content)); err == nil {
			t.Fatal("accepted text input")
		}
	}
	for _, size := range []int64{0, -1, MaxBytes + 1, int64(len(file) + 1), int64(len(file) - 1)} {
		if _, err = s.Put(t.Context(), "owner", id, size, bytes.NewReader(file)); err == nil {
			t.Fatal("accepted bad size", size)
		}
	}
	if len(s.entries) != 0 {
		t.Fatal("failed uploads consume quota")
	}
}
func TestCancelWhileWritingCannotResurrectOrDeleteNewUpload(t *testing.T) {
	s, _ := New(t.TempDir())
	defer s.Close()
	input, writer := io.Pipe()
	id := strings.Repeat("b", 32)
	done := make(chan error, 1)
	go func() { _, err := s.Put(context.Background(), "owner", id, 20, input); done <- err }()
	_, _ = writer.Write([]byte("\x00\x00\x00\x18ftypisom"))
	s.RemoveOwner("owner")
	if _, err := s.Put(t.Context(), "owner", id, 20, strings.NewReader("")); err != ErrBusy {
		t.Fatal("writing reservation reused", err)
	}
	_ = writer.Close()
	if err := <-done; err == nil {
		t.Fatal("cancelled upload succeeded")
	}
	files, _ := os.ReadDir(s.root)
	if len(files) != 0 {
		t.Fatal("orphan file")
	}
}

func TestQuotaExpiryAndStartupOnlyRemoveOwnedTemporaries(t *testing.T) {
	root := t.TempDir()
	unrelated := filepath.Join(root, "keep.txt")
	stale := filepath.Join(root, "upload-"+strings.Repeat("f", 32)+".media")
	for _, path := range []string{unrelated, stale} {
		if err := os.WriteFile(path, []byte("retained"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = os.Stat(unrelated); err != nil {
		t.Fatal("removed unrelated data")
	}
	if _, err = os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale upload survived startup")
	}
	content := "\x00\x00\x00\x18ftypisomcontents"
	for _, letter := range []string{"a", "b"} {
		if _, err = s.Put(t.Context(), "owner", strings.Repeat(letter, 32), int64(len(content)), strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Put(t.Context(), "owner", strings.Repeat("c", 32), int64(len(content)), strings.NewReader(content)); err != ErrBusy {
		t.Fatal("quota bypass", err)
	}
	s.entries[strings.Repeat("a", 32)].expires = time.Now().Add(-time.Second)
	s.Sweep()
	if _, err = s.Put(t.Context(), "owner", strings.Repeat("c", 32), int64(len(content)), strings.NewReader(content)); err != nil {
		t.Fatal("expired reservation leaked", err)
	}
}

func TestUnknownLengthIsBoundedAndDoesNotAcceptDocuments(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := strings.Repeat("d", 32)
	for _, body := range []string{"", "<html>not media</html>", "#EXTM3U\nhttps://example.invalid/video"} {
		if _, err := s.PutStream(t.Context(), "owner", id, strings.NewReader(body)); err == nil {
			t.Fatal("accepted non-media")
		}
	}
	asset, err := s.PutStream(t.Context(), "owner", id, strings.NewReader("\x00\x00\x00\x18ftypisomcontents"))
	if err != nil || asset.MIME != "video/mp4" {
		t.Fatal(asset, err)
	}
}
