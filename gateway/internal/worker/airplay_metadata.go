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

// A changed cover can precede its metadata. Hold it while UxPlay has time to
// publish the matching labels, then use a repeated write as ownership evidence.
const airplayArtworkCandidateHold = 5 * time.Second

// A cover-only refresh with unchanged labels needs more evidence than a track
// change: AirPlay can deliver the next track's image before its metadata.
const airplayArtworkSameTrackRefreshWait = 15 * time.Second

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

// airplayArtworkRevision identifies the visible labels. The accepted artwork
// revision also includes the connection and image bytes.
func airplayArtworkRevision(metadata map[string]string) string {
	if metadata["title"] == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(metadata["title"] + "\x00" + metadata["artist"] + "\x00" + metadata["album"]))
	return hex.EncodeToString(sum[:8])
}

// airplayArtworkEvidence associates independently written UxPlay files within
// one receiver connection. Rewrites with identical labels do not invalidate an
// accepted cover; a changed cover waits for another write or a bounded hold.
// Same-album reuse requires matching artist/album labels and an exact previously
// accepted image; other ambiguous transitions without a fresh write stay blank.
type airplayArtworkEvidence struct {
	mu                    sync.Mutex
	connection            string
	track                 string
	artist                string
	album                 string
	previousArtist        string
	previousAlbum         string
	firstMetadataWrite    time.Time
	hadPriorTrack         bool
	previousAcceptedCover [32]byte
	previousCoverKnown    bool
	lastAcceptedCover     [32]byte
	lastAcceptedKnown     bool
	pendingWithinTrack    bool
	cover                 [32]byte
	coverKnown            bool
	coverFirstSeen        time.Time
	coverLastWrite        time.Time
	postMetadataWrites    int
	acceptedRevision      string
}

func newAirplayArtworkEvidence() *airplayArtworkEvidence { return &airplayArtworkEvidence{} }

func (e *airplayArtworkEvidence) observe(dir, connection string, metadata map[string]string, now time.Time) (string, []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	track := airplayArtworkRevision(metadata)
	_, connectionErr := hex.DecodeString(connection)
	if len(connection) != 16 || connectionErr != nil || track == "" {
		e.connection = ""
		e.track = ""
		e.artist = ""
		e.album = ""
		e.previousArtist = ""
		e.previousAlbum = ""
		e.firstMetadataWrite = time.Time{}
		e.hadPriorTrack = false
		e.previousAcceptedCover = [32]byte{}
		e.previousCoverKnown = false
		e.lastAcceptedCover = [32]byte{}
		e.lastAcceptedKnown = false
		e.pendingWithinTrack = false
		e.cover = [32]byte{}
		e.coverKnown = false
		e.coverFirstSeen = time.Time{}
		e.coverLastWrite = time.Time{}
		e.postMetadataWrites = 0
		e.acceptedRevision = ""
		return "", nil
	}
	metadataInfo, err := os.Lstat(filepath.Join(dir, "metadata.txt"))
	if err != nil || !metadataInfo.Mode().IsRegular() {
		return "", nil
	}
	if e.connection != connection || e.track != track {
		if e.connection != connection {
			e.cover = [32]byte{}
			e.coverKnown = false
			e.coverFirstSeen = time.Time{}
			e.coverLastWrite = time.Time{}
			e.lastAcceptedCover = [32]byte{}
			e.lastAcceptedKnown = false
		}
		priorConnection := e.connection == connection && e.track != ""
		e.previousArtist = ""
		e.previousAlbum = ""
		if priorConnection {
			e.previousArtist = e.artist
			e.previousAlbum = e.album
		}
		e.previousAcceptedCover = [32]byte{}
		e.previousCoverKnown = false
		if priorConnection && e.lastAcceptedKnown {
			e.previousAcceptedCover = e.lastAcceptedCover
			e.previousCoverKnown = true
		}
		e.hadPriorTrack = priorConnection
		e.pendingWithinTrack = false
		e.connection = connection
		e.track = track
		e.artist = metadata["artist"]
		e.album = metadata["album"]
		// A retained file can be rewritten with identical bytes for a new
		// song. Give it a new hold, but preserve the first sighting of a
		// different cover that arrived before the matching metadata.
		if !e.coverKnown || !e.previousCoverKnown || e.cover == e.previousAcceptedCover {
			e.coverFirstSeen = now
		}
		e.firstMetadataWrite = metadataInfo.ModTime()
		e.postMetadataWrites = 0
		e.acceptedRevision = ""
	}
	coverPath := filepath.Join(dir, "coverart")
	before, err := os.Lstat(coverPath)
	if err != nil || !before.Mode().IsRegular() || before.Size() == 95 {
		e.acceptedRevision = ""
		return "", nil
	}
	data, err := receiverFile(dir, "coverart", 2<<20)
	if err != nil || len(data) == 95 {
		return "", nil
	}
	mime := http.DetectContentType(data)
	if mime != "image/jpeg" && mime != "image/png" {
		return "", nil
	}
	after, err := os.Lstat(coverPath)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", nil
	}
	hash := sha256.Sum256(data)
	if !e.coverKnown || hash != e.cover {
		if e.acceptedRevision != "" {
			e.pendingWithinTrack = true
		}
		e.cover, e.coverKnown = hash, true
		e.coverFirstSeen = now
		e.coverLastWrite = after.ModTime()
		e.postMetadataWrites = 0
		e.acceptedRevision = ""
		if after.ModTime().After(e.firstMetadataWrite) {
			e.postMetadataWrites = 1
		}
	} else if after.ModTime().After(e.coverLastWrite) {
		e.coverLastWrite = after.ModTime()
		if after.ModTime().After(e.firstMetadataWrite) {
			e.postMetadataWrites++
		}
	}
	if e.pendingWithinTrack && e.postMetadataWrites >= 2 && now.Sub(e.coverFirstSeen) >= airplayArtworkSameTrackRefreshWait {
		e.pendingWithinTrack = false
	}
	if e.pendingWithinTrack {
		return "", nil
	}
	postMetadataCover := e.coverLastWrite.After(e.firstMetadataWrite) && e.postMetadataWrites > 0
	// UxPlay can leave album artwork untouched when the next song comes from
	// the same album. Reuse only the exact cover already accepted for this
	// connection and only when both artist and album still match.
	sameAlbumReuse := !postMetadataCover && e.hadPriorTrack && e.previousCoverKnown &&
		e.cover == e.previousAcceptedCover && e.album != "" && e.artist != "" &&
		e.album == e.previousAlbum && e.artist == e.previousArtist
	// Some Apple Music tracks send the real image seconds before their labels and
	// do not repeat it. This bounded association is withheld when it could be
	// the last accepted cover or precedes the current connection itself.
	earlyCover := false
	if !postMetadataCover && e.firstMetadataWrite.After(e.coverLastWrite) &&
		e.firstMetadataWrite.Sub(e.coverLastWrite) <= airplayArtworkCandidateHold &&
		(!e.hadPriorTrack || (e.previousCoverKnown && e.cover != e.previousAcceptedCover)) {
		if connectionInfo, err := os.Lstat(filepath.Join(dir, "receiver.dacp")); err == nil && connectionInfo.Mode().IsRegular() && e.coverLastWrite.After(connectionInfo.ModTime()) {
			earlyCover = true
		}
	}
	if !postMetadataCover && !earlyCover && !sameAlbumReuse {
		return "", nil
	}
	if e.acceptedRevision == "" {
		changedFromPrevious := e.previousCoverKnown && e.cover != e.previousAcceptedCover
		readyAt := e.coverFirstSeen
		if earlyCover && e.firstMetadataWrite.After(readyAt) {
			readyAt = e.firstMetadataWrite
		}
		if now.Sub(readyAt) < airplayArtworkCandidateHold {
			return "", nil
		}
		// A new song on the same album can legitimately reuse the previous
		// accepted cover or repeat its bytes in a fresh post-metadata write.
		sameAlbumRewrite := e.album != "" && e.artist != "" &&
			e.album == e.previousAlbum && e.artist == e.previousArtist &&
			postMetadataCover
		if e.hadPriorTrack && !changedFromPrevious && e.postMetadataWrites < 2 && !sameAlbumRewrite && !sameAlbumReuse {
			return "", nil
		}
		key := sha256.Sum256([]byte(connection + "\x00" + track + "\x00" + hex.EncodeToString(hash[:])))
		e.acceptedRevision = hex.EncodeToString(key[:8])
		e.lastAcceptedCover, e.lastAcceptedKnown = hash, true
	}
	return e.acceptedRevision, data
}

func airplayArtwork(dir string, w http.ResponseWriter, r *http.Request) {
	data, err := receiverFile(dir, "coverart", 2<<20)
	mime := http.DetectContentType(data)
	// UxPlay writes a fixed 95-byte transparent placeholder when audio starts.
	if err != nil || len(data) == 95 || (mime != "image/jpeg" && mime != "image/png") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	_, _ = w.Write(data)
}
