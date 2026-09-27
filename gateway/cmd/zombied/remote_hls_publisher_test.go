package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvedYouTubeHLSPublishDirDefaultsBesideStateAndHonorsOverride(t *testing.T) {
	if got, want := resolvedYouTubeHLSPublishDir("/data/gateway.db", ""), "/data/youtube-hls"; got != want {
		t.Fatalf("default publish directory = %q, want %q", got, want)
	}
	if got, want := resolvedYouTubeHLSPublishDir("/data/gateway.db", "/mnt/media/hls"), "/mnt/media/hls"; got != want {
		t.Fatalf("configured publish directory = %q, want %q", got, want)
	}
}

func TestPrepareYouTubeHLSPublishDirIsPrivateAndWritable(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "youtube-hls")
	if err := prepareYouTubeHLSPublishDir(directory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("publish directory mode = %v, want private 0700 directory", info.Mode())
	}
	if err := os.WriteFile(filepath.Join(directory, "fixture"), []byte("ok"), 0600); err != nil {
		t.Fatalf("publish directory is not writable: %v", err)
	}
}
