// Package providers isolates remote metadata and credentials from the client protocol.
package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

func Local(dir string) ([]Source, error) {
	out := []Source{}
	if dir == "" {
		return out, nil
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if len(out) >= 100 {
			break
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		mime := "video/mp4"
		kind := "video"
		switch ext {
		case ".mkv":
			mime = "video/x-matroska"
		case ".webm":
			mime = "video/webm"
		case ".mp4", ".m4v":
		case ".mp3":
			mime = "audio/mpeg"
			kind = "track"
		case ".m4a":
			mime = "audio/mp4"
			kind = "track"
		default:
			continue
		}
		sum := sha256.Sum256([]byte(entry.Name()))
		id := "local-" + hex.EncodeToString(sum[:8])
		title := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		out = append(out, Source{Item: domain.Item{ID: id, Provider: "local", Kind: kind, Title: title, Description: "From your gateway library", Playable: true}, Path: filepath.Join(root, entry.Name()), MIME: mime})
	}
	return out, nil
}
