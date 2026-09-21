package worker

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// UxPlay's pinned -md/-ca outputs are optional ALAC metadata. Mirroring does
// not promise those fields. Reject links, oversized and stale partial outputs.
func receiverFile(dir, name string, limit int64) ([]byte, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit || time.Since(info.ModTime()) > 15*time.Minute {
		return nil, os.ErrInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if len(data) > int(limit) {
		return nil, os.ErrInvalid
	}
	return data, err
}

func airplayMetadata(dir string) map[string]string {
	result := map[string]string{}
	data, err := receiverFile(dir, "metadata.txt", 16<<10)
	if err != nil {
		return result
	}
	for _, line := range strings.Split(string(bytes.ToValidUTF8(data, nil)), "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		switch key {
		case "Title", "Artist", "Album":
			result[strings.ToLower(key)] = string([]rune(value)[:min(len([]rune(value)), 500)])
		}
	}
	return result
}

func airplayArtwork(dir string, w http.ResponseWriter, r *http.Request) {
	data, err := receiverFile(dir, "coverart", 2<<20)
	mime := http.DetectContentType(data)
	if err != nil || (mime != "image/jpeg" && mime != "image/png") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	_, _ = w.Write(data)
}
