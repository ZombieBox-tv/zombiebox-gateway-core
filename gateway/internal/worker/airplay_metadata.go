package worker

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const airplayConnectionGrace = 2 * time.Second

type airplayConnectionEvidence struct {
	mu          sync.Mutex
	fingerprint [32]byte
	revision    string
	connected   bool
	known       bool
	lastSeen    time.Time
}

func newAirplayConnectionEvidence() *airplayConnectionEvidence {
	return &airplayConnectionEvidence{}
}

// observe treats UxPlay's transient export as private connection evidence.
// Its contents are never returned or logged. A brief missing or partial file
// keeps the last observed connection through UxPlay's truncate-and-write.
func (e *airplayConnectionEvidence) observe(dir string, now time.Time, audioFlow bool) (bool, string, bool) {
	fingerprint, present := airplayConnectionFingerprint(dir)
	e.mu.Lock()
	defer e.mu.Unlock()

	if present {
		e.known = true
		if !e.connected || fingerprint != e.fingerprint {
			var nonce [8]byte
			if _, err := rand.Read(nonce[:]); err == nil {
				e.revision = hex.EncodeToString(nonce[:])
			} else {
				e.revision = ""
			}
			e.fingerprint = fingerprint
		}
		e.connected = true
		e.lastSeen = now
		return true, e.revision, true
	}
	if e.connected && now.Sub(e.lastSeen) <= airplayConnectionGrace {
		return true, e.revision, true
	}
	e.connected = false
	if !audioFlow {
		e.known = false
		e.fingerprint = [32]byte{}
		e.revision = ""
	}
	return false, e.revision, e.known
}

func airplayConnectionFingerprint(dir string) ([32]byte, bool) {
	var empty [32]byte
	path := filepath.Join(dir, "receiver.dacp")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 3 || info.Size() > 4<<10 {
		return empty, false
	}
	file, err := os.Open(path)
	if err != nil {
		return empty, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (4<<10)+1))
	if err != nil || len(data) > 4<<10 {
		return empty, false
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if len(lines) != 2 || len(lines[0]) == 0 || len(lines[1]) == 0 {
		return empty, false
	}
	hash := sha256.New()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		var identity [24]byte
		binary.LittleEndian.PutUint64(identity[0:8], uint64(stat.Dev))
		binary.LittleEndian.PutUint64(identity[8:16], uint64(stat.Ino))
		binary.LittleEndian.PutUint64(identity[16:24], uint64(info.ModTime().UnixNano()))
		_, _ = hash.Write(identity[:])
	}
	_, _ = hash.Write(data)
	copy(empty[:], hash.Sum(nil))
	return empty, true
}

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

func airplayMetadataForConnection(dir string, connected bool) map[string]string {
	metadata := airplayMetadata(dir)
	if len(metadata) == 0 {
		return metadata
	}
	connectionInfo, connectionErr := os.Lstat(filepath.Join(dir, "receiver.dacp"))
	if os.IsNotExist(connectionErr) && !connected {
		return metadata
	}
	metadataInfo, metadataErr := os.Lstat(filepath.Join(dir, "metadata.txt"))
	if metadataErr != nil || connectionErr != nil || !metadataInfo.Mode().IsRegular() || !connectionInfo.Mode().IsRegular() || !metadataInfo.ModTime().After(connectionInfo.ModTime()) {
		return map[string]string{}
	}
	return metadata
}

// airplayArtworkRevision mirrors the provider's metadata-only artwork key.
// Connection epochs belong to stream sessions and are deliberately excluded.
func airplayArtworkRevision(metadata map[string]string) string {
	if metadata["title"] == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(metadata["title"] + "\x00" + metadata["artist"] + "\x00" + metadata["album"]))
	return hex.EncodeToString(sum[:8])
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

func airplayArtworkForRevision(dir string, connected bool, revision string, w http.ResponseWriter, r *http.Request) {
	if len(revision) != 16 {
		http.NotFound(w, r)
		return
	}
	if _, err := hex.DecodeString(revision); err != nil {
		http.NotFound(w, r)
		return
	}
	metadata := airplayMetadataForConnection(dir, connected)
	if airplayArtworkRevision(metadata) != revision {
		http.NotFound(w, r)
		return
	}
	metadataInfo, err := os.Lstat(filepath.Join(dir, "metadata.txt"))
	if err != nil || !metadataInfo.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	artworkInfo, err := os.Lstat(filepath.Join(dir, "coverart"))
	if err != nil || !artworkInfo.Mode().IsRegular() || !artworkInfo.ModTime().After(metadataInfo.ModTime()) {
		http.NotFound(w, r)
		return
	}
	airplayArtwork(dir, w, r)
}
