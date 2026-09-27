package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"zombiebox.local/gateway/internal/domain"
)

const (
	remoteHLSPublisherMaxDuration   = 6 * time.Hour
	remoteHLSPublisherMaxRuntime    = remoteHLSPublisherMaxDuration + 30*time.Minute
	remoteHLSPublisherMaxBytes      = int64(8 << 30)
	remoteHLSPublisherDiskReserve   = int64(2 << 30)
	remoteHLSPublisherDiskSlack     = remoteHLSPublisherMaxSegment
	remoteHLSPublisherMinBudget     = int64(64 << 20)
	remoteHLSPublisherMaxSegment    = int64(256 << 20)
	remoteHLSPublisherMaxSegments   = int(remoteHLSPublisherMaxDuration/time.Second) + 2
	remoteHLSPublisherMaxPlaylist   = 2 << 20
	remoteHLSPublisherSegmentTime   = 4 * time.Second
	remoteHLSPublisherPollInterval  = 50 * time.Millisecond
	remoteHLSPublisherDurationSlack = 500 * time.Millisecond
)

var errRemoteHLSPublisherOutput = errors.New("invalid or over-limit HLS publisher output")

type RemoteHLSPublisherState string

const (
	RemoteHLSPublisherStarting  RemoteHLSPublisherState = "starting"
	RemoteHLSPublisherRunning   RemoteHLSPublisherState = "running"
	RemoteHLSPublisherComplete  RemoteHLSPublisherState = "complete"
	RemoteHLSPublisherFailed    RemoteHLSPublisherState = "failed"
	RemoteHLSPublisherCancelled RemoteHLSPublisherState = "cancelled"
	RemoteHLSPublisherClosed    RemoteHLSPublisherState = "closed"
)

// RemoteHLSSegment is one committed MPEG-TS segment named by the current
// playlist. Open it with RemoteHLSPublisherSession.OpenSegment; HTTP serving and
// Content-Length/Range policy remain the server's responsibility.
type RemoteHLSSegment struct {
	Sequence int           `json:"sequence"`
	Name     string        `json:"name"`
	Duration time.Duration `json:"duration"`
	Bytes    int64         `json:"bytes"`
}

// RemoteHLSPublisherSnapshot describes the playlist and the ordered segment
// files visible at one point in time. Playlist contains only local segment
// names; provider URLs and headers never leave the input bridge.
type RemoteHLSPublisherSnapshot struct {
	State         RemoteHLSPublisherState `json:"state"`
	Duration      time.Duration           `json:"duration"`
	StartPosition time.Duration           `json:"startPosition"`
	Playlist      []byte                  `json:"playlist,omitempty"`
	Segments      []RemoteHLSSegment      `json:"segments"`
	OutputBytes   int64                   `json:"outputBytes"`
	Error         string                  `json:"error,omitempty"`
	Complete      bool                    `json:"complete"`
}

// RemoteHLSPublisherSession owns a bounded, private directory and one
// cancellable FFmpeg process. Close cancels and reaps the process before it
// removes that directory. A session also expires after its bounded lifetime.
type RemoteHLSPublisherSession struct {
	remote        *RemoteTools
	directory     string
	playlistPath  string
	expected      time.Duration
	startPosition time.Duration
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	snapshotMu    sync.Mutex
	mu            sync.RWMutex
	state         RemoteHLSPublisherState
	resultErr     error
	playlistCache []byte
	segmentCache  map[string]RemoteHLSSegment
	outputBytes   int64
	maxBytes      int64
	closeOnce     sync.Once
	cleanupTimer  *time.Timer
}

// StartRemoteHLSPublisher starts a full-duration, incremental MPEG-TS EVENT
// playlist for a finite split H.264 video/AAC audio source. The duration comes
// from Source.Item.DurationMS; the source must carry that known finite duration.
// Quality is validated as semantic metadata; stream copy preserves the chosen
// source rendition. The returned session is available before probing or FFmpeg
// completes.
func (t *RemoteTools) StartRemoteHLSPublisher(parent context.Context, source domain.Source, selection domain.MediaSelection, outputRoot string) (*RemoteHLSPublisherSession, error) {
	if t == nil || t.tools == nil || t.http == nil {
		return nil, errors.New("remote media unavailable")
	}
	if !ValidQuality(selection.Quality) {
		return nil, errors.New("invalid media quality")
	}
	if selection.AudioID != nil {
		return nil, errors.New("split HLS publisher does not support audio selection overrides")
	}
	if source.Live || source.AudioURL == "" || ManifestKind(source) != "" || !RemoteCandidate(source) {
		return nil, errors.New("HLS publisher requires a finite split progressive source")
	}
	if source.Item.DurationMS <= 0 || source.Item.DurationMS > int64(remoteHLSPublisherMaxDuration/time.Millisecond) {
		return nil, errors.New("HLS publisher requires a known duration of six hours or less")
	}
	if selection.PositionMS < 0 || selection.PositionMS >= source.Item.DurationMS {
		return nil, errors.New("invalid HLS publisher start position")
	}
	directory, err := createPrivateHLSPublisherDirectory(outputRoot)
	if err != nil {
		return nil, err
	}
	maxBytes, err := remoteHLSPublisherDiskBudget(directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parent, remoteHLSPublisherMaxRuntime)
	session := &RemoteHLSPublisherSession{
		remote:        t,
		directory:     directory,
		playlistPath:  filepath.Join(directory, "index.m3u8"),
		expected:      time.Duration(source.Item.DurationMS) * time.Millisecond,
		startPosition: time.Duration(selection.PositionMS) * time.Millisecond,
		ctx:           ctx,
		cancel:        cancel,
		done:          make(chan struct{}),
		state:         RemoteHLSPublisherStarting,
		segmentCache:  make(map[string]RemoteHLSSegment),
		maxBytes:      maxBytes,
	}
	session.cleanupTimer = time.AfterFunc(remoteHLSPublisherMaxRuntime, session.Close)
	go session.run(source)
	return session, nil
}

// Wait blocks until FFmpeg and its input bridge have stopped. It returns nil
// only after the complete source duration has been validated and ENDLIST exists.
func (s *RemoteHLSPublisherSession) Wait(ctx context.Context) error {
	if s == nil {
		return errors.New("HLS publisher session unavailable")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.resultErr
	}
}

// Snapshot reads the most recently committed playlist and verifies newly
// published segments before caching their private-file metadata. Before FFmpeg
// publishes its first playlist, it returns an empty playlist and no segments.
func (s *RemoteHLSPublisherSession) Snapshot() (RemoteHLSPublisherSnapshot, error) {
	if s == nil {
		return RemoteHLSPublisherSnapshot{}, errors.New("HLS publisher session unavailable")
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	s.mu.RLock()
	snapshot := RemoteHLSPublisherSnapshot{
		State:         s.state,
		Duration:      s.expected,
		StartPosition: s.startPosition,
		Segments:      make([]RemoteHLSSegment, 0),
		Error:         safePublisherError(s.resultErr),
		Complete:      s.state == RemoteHLSPublisherComplete,
	}
	s.mu.RUnlock()
	directoryInfo, err := os.Lstat(s.directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 || directoryInfo.Mode().Perm()&0077 != 0 {
		return RemoteHLSPublisherSnapshot{}, errRemoteHLSPublisherOutput
	}

	data, err := os.ReadFile(s.playlistPath)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil || len(data) == 0 || len(data) > remoteHLSPublisherMaxPlaylist {
		return RemoteHLSPublisherSnapshot{}, errRemoteHLSPublisherOutput
	}
	entries, endList, err := parseRemoteHLSPublisherPlaylist(data)
	if err != nil {
		return RemoteHLSPublisherSnapshot{}, err
	}
	s.mu.RLock()
	cachedPlaylist := append([]byte(nil), s.playlistCache...)
	cachedSegments := make(map[string]RemoteHLSSegment, len(s.segmentCache))
	for name, segment := range s.segmentCache {
		cachedSegments[name] = segment
	}
	cachedBytes := s.outputBytes
	s.mu.RUnlock()
	if bytes.Equal(cachedPlaylist, data) {
		snapshot.Playlist = append([]byte(nil), data...)
		for _, entry := range entries {
			segment, ok := cachedSegments[entry.name]
			if !ok || segment.Sequence != entry.sequence || segment.Duration != entry.duration {
				return RemoteHLSPublisherSnapshot{}, errRemoteHLSPublisherOutput
			}
			snapshot.Segments = append(snapshot.Segments, segment)
		}
		snapshot.OutputBytes = cachedBytes
		return s.finishSnapshot(snapshot, endList)
	}
	if len(entries) < len(cachedSegments) {
		return RemoteHLSPublisherSnapshot{}, errRemoteHLSPublisherOutput
	}
	snapshot.Playlist = append([]byte(nil), data...)
	updatedSegments := make(map[string]RemoteHLSSegment, len(entries))
	for _, entry := range entries {
		segment, ok := cachedSegments[entry.name]
		if ok {
			if segment.Sequence != entry.sequence || segment.Duration != entry.duration {
				return RemoteHLSPublisherSnapshot{}, errRemoteHLSPublisherOutput
			}
		} else {
			path := filepath.Join(s.directory, entry.name)
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 188 || info.Size()%188 != 0 || info.Size() > remoteHLSPublisherMaxSegment {
				return RemoteHLSPublisherSnapshot{}, errRemoteHLSPublisherOutput
			}
			if err := validateMPEGTSFirstPacket(path); err != nil {
				return RemoteHLSPublisherSnapshot{}, err
			}
			segment = RemoteHLSSegment{
				Sequence: entry.sequence,
				Name:     entry.name,
				Duration: entry.duration,
				Bytes:    info.Size(),
			}
		}
		updatedSegments[entry.name] = segment
		snapshot.Segments = append(snapshot.Segments, segment)
		snapshot.OutputBytes += segment.Bytes
	}
	info, err := os.Lstat(s.playlistPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return RemoteHLSPublisherSnapshot{}, errRemoteHLSPublisherOutput
	}
	snapshot.OutputBytes += info.Size()
	if snapshot.OutputBytes > s.maxBytes {
		return RemoteHLSPublisherSnapshot{}, errRemoteHLSPublisherOutput
	}
	s.mu.Lock()
	s.playlistCache = append(s.playlistCache[:0], data...)
	s.segmentCache = updatedSegments
	s.outputBytes = snapshot.OutputBytes
	s.mu.Unlock()
	return s.finishSnapshot(snapshot, endList)
}

func (s *RemoteHLSPublisherSession) finishSnapshot(snapshot RemoteHLSPublisherSnapshot, endList bool) (RemoteHLSPublisherSnapshot, error) {
	s.mu.RLock()
	snapshot.State = s.state
	snapshot.Error = safePublisherError(s.resultErr)
	snapshot.Complete = s.state == RemoteHLSPublisherComplete
	s.mu.RUnlock()
	if endList && !snapshot.Complete {
		// FFmpeg can write ENDLIST for a clean early EOF. Only the verified
		// terminal state may expose a completed-playlist view to the server.
		snapshot.Playlist = withoutRemoteHLSEndList(snapshot.Playlist)
	}
	return snapshot, nil
}

// OpenSegment opens a segment currently named in the published playlist. The
// caller owns the returned file and implements HTTP range and length handling.
func (s *RemoteHLSPublisherSession) OpenSegment(name string) (*os.File, int64, error) {
	if s == nil || !validRemoteHLSSegmentName(name) {
		return nil, 0, errors.New("HLS segment unavailable")
	}
	var expectedBytes int64
	s.mu.RLock()
	segment, ok := s.segmentCache[name]
	s.mu.RUnlock()
	if !ok {
		if _, err := s.Snapshot(); err != nil {
			return nil, 0, err
		}
		s.mu.RLock()
		segment, ok = s.segmentCache[name]
		s.mu.RUnlock()
	}
	if ok {
		expectedBytes = segment.Bytes
	}
	if expectedBytes == 0 {
		return nil, 0, errors.New("HLS segment unavailable")
	}
	file, err := os.Open(filepath.Join(s.directory, name))
	if err != nil {
		return nil, 0, errors.New("HLS segment unavailable")
	}
	lstat, err := os.Lstat(filepath.Join(s.directory, name))
	if err != nil || !lstat.Mode().IsRegular() || lstat.Mode()&os.ModeSymlink != 0 || lstat.Size() != expectedBytes {
		file.Close()
		return nil, 0, errors.New("HLS segment unavailable")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedBytes {
		file.Close()
		return nil, 0, errors.New("HLS segment unavailable")
	}
	return file, expectedBytes, nil
}

// Close cancels and reaps any active process, then removes the session's
// private output directory. It is safe to call more than once.
func (s *RemoteHLSPublisherSession) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		if s.cleanupTimer != nil {
			s.cleanupTimer.Stop()
		}
		s.mu.Lock()
		s.state = RemoteHLSPublisherClosed
		s.mu.Unlock()
		_ = os.RemoveAll(s.directory)
	})
}

func (s *RemoteHLSPublisherSession) run(source domain.Source) {
	stage := "bridge"
	err := s.runPublisher(source, &stage)
	if err == nil {
		err = s.ctx.Err()
	}
	if err != nil {
		stripRemoteHLSEndList(s.playlistPath)
		if os.Getenv("ZOMBIE_YOUTUBE_HLS_QA_TRACE") == "1" && !errors.Is(err, context.Canceled) {
			log.Printf("youtube_hls_publisher stage=%s outcome=%s", stage, safePublisherError(err))
		}
	}
	s.mu.Lock()
	s.resultErr = err
	switch {
	case err == nil:
		s.state = RemoteHLSPublisherComplete
	case errors.Is(err, context.Canceled):
		s.state = RemoteHLSPublisherCancelled
	default:
		s.state = RemoteHLSPublisherFailed
	}
	s.mu.Unlock()
	close(s.done)
	if errors.Is(err, context.Canceled) || errors.Is(err, errRemoteHLSPublisherOutput) {
		go s.Close()
	}
}

func (s *RemoteHLSPublisherSession) runPublisher(source domain.Source, stage *string) error {
	bridge, err := s.remote.bridge(s.ctx, source)
	if err != nil {
		return err
	}
	defer bridge.close()

	*stage = "video_probe"
	videoMetadata, err := s.remote.tools.probe(s.ctx, bridge.video, true)
	if err != nil {
		return bridge.failureOr(err)
	}
	if err := bridge.failureOr(nil); err != nil {
		return err
	}
	if !hasOnlyExpectedCodec(videoMetadata, "video", "h264") {
		return errors.New("HLS publisher requires split H.264 video and AAC audio")
	}
	*stage = "audio_probe"
	audioMetadata, err := s.remote.tools.probe(s.ctx, bridge.audio, true)
	if err != nil {
		return bridge.failureOr(err)
	}
	if err := bridge.failureOr(nil); err != nil {
		return err
	}
	if !hasOnlyExpectedCodec(audioMetadata, "audio", "aac") {
		return errors.New("HLS publisher requires split H.264 video and AAC audio")
	}

	*stage = "job_slot"
	if err := s.remote.tools.acquireJob(s.ctx); err != nil {
		return err
	}
	defer func() { <-s.remote.tools.jobs }()

	args := remoteHLSPublisherArguments(bridge, s.expected, s.startPosition, s.directory, s.maxBytes)
	s.mu.Lock()
	s.state = RemoteHLSPublisherRunning
	s.mu.Unlock()
	*stage = "ffmpeg"
	if err := runRemoteHLSPublisher(s.ctx, s.remote.tools.runner, s.remote.tools.ffmpeg, args, s.directory, s.maxBytes); err != nil {
		return bridge.failureOr(err)
	}
	if err := bridge.failureOr(nil); err != nil {
		return err
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	*stage = "validation"
	if err := validateRemoteHLSPublisherOutput(s.directory, s.expected-s.startPosition, s.maxBytes); err != nil {
		return err
	}
	return s.ctx.Err()
}

func remoteHLSPublisherArguments(bridge inputBridge, duration, start time.Duration, directory string, maxBytes int64) []string {
	seconds := func(value time.Duration) string {
		return strconv.FormatFloat(value.Seconds(), 'f', 3, 64)
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-max_alloc", "67108864", "-threads", "2"}
	args = remoteArguments(args)
	if start > 0 {
		args = append(args, "-ss", seconds(start))
	}
	args = append(args, "-readrate", "1", "-readrate_initial_burst", "8")
	args = append(args, "-i", bridge.video)
	args = append(args, remoteArguments(nil)...)
	if start > 0 {
		args = append(args, "-ss", seconds(start))
	}
	args = append(args, "-readrate", "1", "-readrate_initial_burst", "8")
	playlist := filepath.Join(directory, "index.m3u8")
	segmentPattern := filepath.Join(directory, "segment-%05d.ts")
	args = append(args, "-i", bridge.audio,
		"-map", "0:v:0", "-map", "1:a:0",
		"-sn", "-dn", "-map_metadata", "-1",
		"-c:v", "copy", "-c:a", "copy",
		"-t", seconds(duration-start),
		"-fs", strconv.FormatInt(maxBytes, 10),
		"-f", "hls",
		"-hls_time", seconds(remoteHLSPublisherSegmentTime),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "temp_file",
		"-hls_segment_type", "mpegts",
		"-start_number", "1",
		"-hls_segment_filename", segmentPattern,
		playlist,
	)
	return args
}

func runRemoteHLSPublisher(parent context.Context, runner Runner, executable string, args []string, directory string, maxBytes int64) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, executable, args, io.Discard)
	}()
	ticker := time.NewTicker(remoteHLSPublisherPollInterval)
	defer ticker.Stop()
	monitor := &remoteHLSPublisherMonitor{maxBytes: maxBytes}
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return conversionProcessFailure(err)
			}
			return nil
		case <-ctx.Done():
			<-done // Runner promises to reap its child before returning.
			return ctx.Err()
		case <-ticker.C:
			if err := monitor.check(directory); err != nil {
				cancel()
				<-done // Keep the job slot until FFmpeg has exited.
				return err
			}
		}
	}
}

type remoteHLSPublisherMonitor struct {
	nextSequence int
	bytes        int64
	maxBytes     int64
}

func (m *remoteHLSPublisherMonitor) check(directory string) error {
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 || directoryInfo.Mode().Perm()&0077 != 0 {
		return errRemoteHLSPublisherOutput
	}
	var transientBytes int64
	for _, name := range []string{"index.m3u8", "index.m3u8.tmp"} {
		size, exists, err := publisherOutputFileSize(filepath.Join(directory, name), remoteHLSPublisherMaxPlaylist)
		if err != nil {
			return errRemoteHLSPublisherOutput
		}
		if exists {
			transientBytes += size
		}
	}
	for m.nextSequence < remoteHLSPublisherMaxSegments {
		name := remoteHLSPublisherSegmentNameForSequence(m.nextSequence)
		path := filepath.Join(directory, name)
		size, exists, err := publisherOutputFileSize(path, remoteHLSPublisherMaxSegment)
		if err != nil {
			return errRemoteHLSPublisherOutput
		}
		if exists {
			if size < 188 || size%188 != 0 || validateMPEGTSFirstPacket(path) != nil {
				return errRemoteHLSPublisherOutput
			}
			m.bytes += size
			m.nextSequence++
			continue
		}
		temporary := path + ".tmp"
		tempSize, tempExists, err := publisherOutputFileSize(temporary, remoteHLSPublisherMaxSegment)
		if err != nil {
			return errRemoteHLSPublisherOutput
		}
		if tempExists {
			transientBytes += tempSize
		}
		break
	}
	if m.nextSequence == remoteHLSPublisherMaxSegments {
		if _, exists, err := publisherOutputFileSize(filepath.Join(directory, remoteHLSPublisherSegmentNameForSequence(m.nextSequence)), remoteHLSPublisherMaxSegment); err != nil || exists {
			return errRemoteHLSPublisherOutput
		}
	}
	if m.bytes+transientBytes > m.maxBytes {
		return errRemoteHLSPublisherOutput
	}
	available, err := remoteHLSPublisherAvailableBytes(directory)
	if err != nil || available < remoteHLSPublisherDiskReserve+remoteHLSPublisherDiskSlack {
		return errRemoteHLSPublisherOutput
	}
	return nil
}

func publisherOutputFileSize(path string, maximum int64) (int64, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maximum {
		return 0, false, errRemoteHLSPublisherOutput
	}
	return info.Size(), true, nil
}

func remoteHLSPublisherSegmentNameForSequence(sequence int) string {
	return fmt.Sprintf("segment-%05d.ts", sequence+1)
}

type remoteHLSPublisherPlaylistEntry struct {
	sequence int
	name     string
	duration time.Duration
}

func parseRemoteHLSPublisherPlaylist(data []byte) ([]remoteHLSPublisherPlaylistEntry, bool, error) {
	if len(data) == 0 || len(data) > remoteHLSPublisherMaxPlaylist {
		return nil, false, errRemoteHLSPublisherOutput
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, false, errRemoteHLSPublisherOutput
	}
	var entries []remoteHLSPublisherPlaylistEntry
	var pending *time.Duration
	var sawVersion, sawTarget, sawSequence, sawEvent, endList bool
	target := 0
	var total time.Duration
	var longest time.Duration
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if pending != nil || endList {
				return nil, false, errRemoteHLSPublisherOutput
			}
			value := strings.TrimPrefix(line, "#EXTINF:")
			seconds, _, _ := strings.Cut(value, ",")
			parsed, err := strconv.ParseFloat(seconds, 64)
			if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 || parsed > 120 {
				return nil, false, errRemoteHLSPublisherOutput
			}
			duration := time.Duration(parsed * float64(time.Second))
			total += duration
			if duration > longest {
				longest = duration
			}
			if total > remoteHLSPublisherMaxDuration+2*remoteHLSPublisherSegmentTime || len(entries) >= remoteHLSPublisherMaxSegments {
				return nil, false, errRemoteHLSPublisherOutput
			}
			pending = &duration
			continue
		}
		if strings.HasPrefix(line, "#") {
			switch {
			case strings.HasPrefix(line, "#EXT-X-VERSION:"):
				version, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-VERSION:"))
				if err != nil || version < 1 || version > 7 || sawVersion {
					return nil, false, errRemoteHLSPublisherOutput
				}
				sawVersion = true
			case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
				seconds, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
				if err != nil || seconds < 1 || seconds > 120 || sawTarget {
					return nil, false, errRemoteHLSPublisherOutput
				}
				target = seconds
				sawTarget = true
			case line == "#EXT-X-MEDIA-SEQUENCE:1":
				if sawSequence {
					return nil, false, errRemoteHLSPublisherOutput
				}
				sawSequence = true
			case line == "#EXT-X-PLAYLIST-TYPE:EVENT":
				if sawEvent {
					return nil, false, errRemoteHLSPublisherOutput
				}
				sawEvent = true
			case line == "#EXT-X-ENDLIST":
				if pending != nil || endList {
					return nil, false, errRemoteHLSPublisherOutput
				}
				endList = true
			default:
				return nil, false, errRemoteHLSPublisherOutput
			}
			continue
		}
		if pending == nil || endList || !validRemoteHLSSegmentName(line) || len(entries) >= remoteHLSPublisherMaxSegments {
			return nil, false, errRemoteHLSPublisherOutput
		}
		sequence, _, ok := remoteHLSPublisherSegmentName(line)
		if !ok {
			return nil, false, errRemoteHLSPublisherOutput
		}
		entries = append(entries, remoteHLSPublisherPlaylistEntry{sequence: sequence, name: line, duration: *pending})
		pending = nil
	}
	// RFC 8216 compares TARGETDURATION with each EXTINF rounded to the
	// nearest integer. FFmpeg may emit 4.08 seconds with TARGETDURATION:4.
	if !sawVersion || !sawTarget || !sawSequence || !sawEvent || pending != nil || (len(entries) > 0 && float64(target) < math.Round(float64(longest)/float64(time.Second))) {
		return nil, false, errRemoteHLSPublisherOutput
	}
	for index, entry := range entries {
		if entry.sequence != index {
			return nil, false, errRemoteHLSPublisherOutput
		}
	}
	return entries, endList, nil
}

func validRemoteHLSSegmentName(name string) bool {
	if len(name) != len("segment-00001.ts") || !strings.HasPrefix(name, "segment-") || !strings.HasSuffix(name, ".ts") {
		return false
	}
	for _, digit := range strings.TrimSuffix(strings.TrimPrefix(name, "segment-"), ".ts") {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func remoteHLSPublisherSegmentName(name string) (int, bool, bool) {
	temporary := strings.HasSuffix(name, ".ts.tmp")
	suffix := ".ts"
	if temporary {
		suffix = ".ts.tmp"
	}
	if !strings.HasPrefix(name, "segment-") || !strings.HasSuffix(name, suffix) {
		return 0, false, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "segment-"), suffix)
	if len(digits) != 5 {
		return 0, false, false
	}
	index, err := strconv.Atoi(digits)
	return index - 1, temporary, err == nil && index > 0
}

func validateRemoteHLSPublisherOutput(directory string, expected time.Duration, maxBytes int64) error {
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) < 2 || len(entries) > remoteHLSPublisherMaxSegments+1 {
		return errRemoteHLSPublisherOutput
	}
	var totalBytes int64
	segments := make(map[int]int64)
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 {
			return errRemoteHLSPublisherOutput
		}
		switch name {
		case "index.m3u8":
			if info.Size() == 0 || info.Size() > remoteHLSPublisherMaxPlaylist {
				return errRemoteHLSPublisherOutput
			}
			totalBytes += info.Size()
		case "index.m3u8.tmp":
			return errRemoteHLSPublisherOutput
		default:
			index, temporary, ok := remoteHLSPublisherSegmentName(name)
			if !ok || temporary || index >= remoteHLSPublisherMaxSegments || info.Size() < 188 || info.Size()%188 != 0 || info.Size() > remoteHLSPublisherMaxSegment {
				return errRemoteHLSPublisherOutput
			}
			segments[index] = info.Size()
			totalBytes += info.Size()
			if err := validateMPEGTSFirstPacket(path); err != nil {
				return err
			}
		}
		if totalBytes > maxBytes {
			return errRemoteHLSPublisherOutput
		}
	}
	playlistPath := filepath.Join(directory, "index.m3u8")
	playlist, err := os.ReadFile(playlistPath)
	if err != nil {
		return errRemoteHLSPublisherOutput
	}
	playlistEntries, endList, err := parseRemoteHLSPublisherPlaylist(playlist)
	if err != nil || !endList || len(playlistEntries) != len(segments) {
		return errRemoteHLSPublisherOutput
	}
	var elapsed time.Duration
	for index, entry := range playlistEntries {
		if entry.sequence != index {
			return errRemoteHLSPublisherOutput
		}
		if _, ok := segments[index]; !ok {
			return errRemoteHLSPublisherOutput
		}
		elapsed += entry.duration
	}
	if expected <= 0 || elapsed+remoteHLSPublisherDurationSlack < expected || elapsed > expected+remoteHLSPublisherSegmentTime {
		return errors.New("HLS publisher did not cover the requested source duration")
	}
	if totalBytes > maxBytes {
		return errRemoteHLSPublisherOutput
	}
	return nil
}

func validateMPEGTSFirstPacket(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errRemoteHLSPublisherOutput
	}
	defer file.Close()
	packet := make([]byte, 188)
	if _, err := io.ReadFull(bufio.NewReaderSize(file, len(packet)), packet); err != nil || packet[0] != 0x47 {
		return errRemoteHLSPublisherOutput
	}
	return nil
}

func createPrivateHLSPublisherDirectory(root string) (string, error) {
	if root == "" {
		return "", errors.New("private HLS publisher root required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", errors.New("invalid HLS publisher root")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || resolved != absolute {
		return "", errors.New("HLS publisher root must not use symlinks")
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0700 != 0700 {
		return "", errors.New("HLS publisher root must be private and writable")
	}
	directory, err := os.MkdirTemp(absolute, "hls-publisher-")
	if err != nil {
		return "", errors.New("unable to create private HLS publisher directory")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		_ = os.RemoveAll(directory)
		return "", errors.New("unable to secure HLS publisher directory")
	}
	return directory, nil
}

func remoteHLSPublisherDiskBudget(directory string) (int64, error) {
	available, err := remoteHLSPublisherAvailableBytes(directory)
	if err != nil {
		return 0, errRemoteHLSPublisherOutput
	}
	return remoteHLSPublisherBudgetFromAvailable(available)
}

func remoteHLSPublisherBudgetFromAvailable(available int64) (int64, error) {
	budget := available - remoteHLSPublisherDiskReserve - remoteHLSPublisherDiskSlack
	if budget < remoteHLSPublisherMinBudget {
		return 0, errors.New("insufficient disk space for HLS publisher")
	}
	if budget > remoteHLSPublisherMaxBytes {
		budget = remoteHLSPublisherMaxBytes
	}
	return budget, nil
}

func remoteHLSPublisherAvailableBytes(directory string) (int64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(directory, &stats); err != nil || stats.Bsize <= 0 {
		return 0, errRemoteHLSPublisherOutput
	}
	blocks := uint64(stats.Bavail)
	blockSize := uint64(stats.Bsize)
	maxInt64 := uint64(^uint64(0) >> 1)
	if blocks > maxInt64/blockSize {
		return int64(maxInt64), nil
	}
	return int64(blocks * blockSize), nil
}

func safePublisherError(err error) string {
	if err == nil {
		return ""
	}
	var failure *ConversionFailure
	if errors.As(err, &failure) {
		return failure.Error()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context deadline exceeded"
	case errors.Is(err, ErrBusy):
		return ErrBusy.Error()
	case errors.Is(err, errRemoteHLSPublisherOutput):
		return errRemoteHLSPublisherOutput.Error()
	case err.Error() == "HLS publisher did not cover the requested source duration":
		return err.Error()
	default:
		return "HLS publisher failed"
	}
}

func withoutRemoteHLSEndList(data []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) != "#EXT-X-ENDLIST" {
			filtered = append(filtered, line)
		}
	}
	return []byte(strings.Join(filtered, "\n"))
}

func stripRemoteHLSEndList(path string) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > remoteHLSPublisherMaxPlaylist {
		return
	}
	updated := withoutRemoteHLSEndList(data)
	if string(updated) == string(data) {
		return
	}
	temporary := path + ".incomplete"
	if os.WriteFile(temporary, updated, 0600) != nil {
		_ = os.Remove(temporary)
		return
	}
	if os.Rename(temporary, path) != nil {
		_ = os.Remove(temporary)
	}
}
