package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

const (
	hlsEventDuration       = "28"
	hlsEventSegmentSeconds = 4
	hlsEventMaxSegments    = 32
	hlsEventMaxBytes       = 64 << 20
	hlsEventMaxPlaylist    = 64 << 10
	hlsEventProcessTimeout = 2 * time.Minute
)

var (
	errHLSEventOutputLimit   = errors.New("HLS EVENT output limit exceeded")
	errHLSEventOutputInvalid = errors.New("invalid HLS EVENT output")
)

// HLSEventProbeSegment describes one committed MPEG-TS segment in a completed
// bounded QA EVENT playlist. Paths stay in the caller-owned output directory.
type HLSEventProbeSegment struct {
	Path  string
	Bytes int64
}

// HLSEventProbeResult identifies a bounded QA playlist and its segments. The
// playlist and committed segments are visible while conversion runs.
type HLSEventProbeResult struct {
	PlaylistPath string
	Segments     []HLSEventProbeSegment
	TotalBytes   int64
}

// ConvertRemoteToHLSEventProbe copies the first 28 seconds of a finite split
// H.264 video and AAC audio source into a bounded MPEG-TS HLS EVENT playlist.
// It is a QA adapter for source validation and incremental segmenter behavior,
// not a full-duration publisher: output is capped at 32 segments and 64 MiB.
// Quality is accepted as semantic metadata but does not alter copy parameters.
// The caller owns outputDir and its contents for the result lifetime.
func (t *RemoteTools) ConvertRemoteToHLSEventProbe(ctx context.Context, source domain.Source, selection domain.MediaSelection, outputDir string) (HLSEventProbeResult, error) {
	if t == nil || t.tools == nil || t.http == nil {
		return HLSEventProbeResult{}, errors.New("remote media unavailable")
	}
	if !ValidQuality(selection.Quality) {
		return HLSEventProbeResult{}, errors.New("invalid media quality")
	}
	if selection.PositionMS != 0 {
		return HLSEventProbeResult{}, errors.New("HLS EVENT probe supports only the initial position")
	}
	if selection.AudioID != nil {
		return HLSEventProbeResult{}, errors.New("HLS EVENT probe does not support an audio selection override")
	}
	if source.Live || source.AudioURL == "" || ManifestKind(source) != "" || !RemoteCandidate(source) {
		return HLSEventProbeResult{}, errors.New("HLS EVENT probe requires a finite split progressive source")
	}

	directory, err := privateEmptyHLSEventDirectory(outputDir)
	if err != nil {
		return HLSEventProbeResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, hlsEventProcessTimeout)
	defer cancel()

	bridge, err := t.bridge(ctx, source)
	if err != nil {
		return HLSEventProbeResult{}, err
	}
	defer bridge.close()

	success := false
	defer func() {
		if !success {
			cleanupHLSEventFiles(directory)
		}
	}()

	videoMetadata, err := t.tools.probe(ctx, bridge.video, true)
	if err != nil {
		return HLSEventProbeResult{}, bridge.failureOr(err)
	}
	if err := bridge.failureOr(nil); err != nil {
		return HLSEventProbeResult{}, err
	}
	if !hasOnlyExpectedCodec(videoMetadata, "video", "h264") {
		return HLSEventProbeResult{}, errors.New("HLS EVENT probe requires split H.264 video and AAC audio")
	}

	audioMetadata, err := t.tools.probe(ctx, bridge.audio, true)
	if err != nil {
		return HLSEventProbeResult{}, bridge.failureOr(err)
	}
	if err := bridge.failureOr(nil); err != nil {
		return HLSEventProbeResult{}, err
	}
	if !hasOnlyExpectedCodec(audioMetadata, "audio", "aac") {
		return HLSEventProbeResult{}, errors.New("HLS EVENT probe requires split H.264 video and AAC audio")
	}

	if err := t.tools.acquireJob(ctx); err != nil {
		return HLSEventProbeResult{}, err
	}
	defer func() { <-t.tools.jobs }()

	playlistPath := filepath.Join(directory, "index.m3u8")
	segmentPattern := filepath.Join(directory, "seg%03d.ts")
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-max_alloc", "67108864", "-threads", "2"}
	args = remoteArguments(args)
	args = append(args, "-i", bridge.video)
	args = append(args, remoteArguments(nil)...)
	args = append(args, "-i", bridge.audio,
		"-map", "0:v:0", "-map", "1:a:0",
		"-sn", "-dn", "-map_metadata", "-1",
		"-c:v", "copy", "-c:a", "copy",
		"-t", hlsEventDuration,
		"-fs", strconv.Itoa(hlsEventMaxBytes),
		"-f", "hls",
		"-hls_time", strconv.Itoa(hlsEventSegmentSeconds),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "temp_file",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", segmentPattern,
		playlistPath,
	)
	if err := runBoundedHLSEvent(ctx, t.tools.runner, t.tools.ffmpeg, args, directory); err != nil {
		return HLSEventProbeResult{}, bridge.failureOr(err)
	}
	if err := bridge.failureOr(nil); err != nil {
		return HLSEventProbeResult{}, err
	}

	result, err := inspectHLSEvent(ctx, t.tools, directory)
	if err != nil {
		return HLSEventProbeResult{}, err
	}
	success = true
	return result, nil
}

func hasOnlyExpectedCodec(metadata Metadata, expectedType, expectedCodec string) bool {
	expected := 0
	for _, stream := range metadata.Streams {
		if stream.Type == expectedType && stream.Codec == expectedCodec {
			expected++
			continue
		}
		if stream.Type == "video" || stream.Type == "audio" {
			return false
		}
	}
	return expected == 1
}

func privateEmptyHLSEventDirectory(path string) (string, error) {
	if path == "" {
		return "", errors.New("private HLS EVENT output directory required")
	}
	directory, err := filepath.Abs(path)
	if err != nil || strings.Contains(directory, "%") {
		return "", errors.New("invalid HLS EVENT output directory")
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return "", errors.New("HLS EVENT output directory must not use symlinks")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("private HLS EVENT output directory required")
	}
	permissions := info.Mode().Perm()
	if permissions&0077 != 0 || permissions&0300 != 0300 {
		return "", errors.New("HLS EVENT output directory must be private and writable")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		return "", errors.New("HLS EVENT output directory must be empty")
	}
	return directory, nil
}

func runBoundedHLSEvent(parent context.Context, runner Runner, executable string, args []string, directory string) error {
	ctx, cancel := context.WithTimeout(parent, hlsEventProcessTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, executable, args, io.Discard)
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
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
			<-done // Runner promises to reap its process before returning.
			return ctx.Err()
		case <-ticker.C:
			if _, err := scanHLSEventFiles(directory); err != nil {
				cancel()
				<-done // Do not clean files or release the job before the child is reaped.
				return err
			}
		}
	}
}

type hlsEventFiles struct {
	totalBytes    int64
	playlistBytes int64
	segments      map[int]struct{}
	temporary     bool
}

func scanHLSEventFiles(directory string) (hlsEventFiles, error) {
	result := hlsEventFiles{segments: make(map[int]struct{})}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return result, errHLSEventOutputInvalid
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return result, errHLSEventOutputInvalid
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return result, errHLSEventOutputInvalid
		}
		if info.Size() < 0 || info.Size() > hlsEventMaxBytes {
			return result, errHLSEventOutputLimit
		}
		switch name {
		case "index.m3u8", "index.m3u8.tmp":
			if name == "index.m3u8.tmp" {
				result.temporary = true
			}
			if info.Size() > hlsEventMaxPlaylist {
				return result, errHLSEventOutputLimit
			}
			if name == "index.m3u8" {
				result.playlistBytes = info.Size()
			}
		default:
			index, temporary, ok := hlsEventSegmentName(name)
			if !ok || index >= hlsEventMaxSegments {
				return result, errHLSEventOutputInvalid
			}
			if temporary {
				result.temporary = true
			}
			result.segments[index] = struct{}{}
		}
		result.totalBytes += info.Size()
		if result.totalBytes > hlsEventMaxBytes {
			return result, errHLSEventOutputLimit
		}
	}
	if len(result.segments) > hlsEventMaxSegments {
		return result, errHLSEventOutputLimit
	}
	return result, nil
}

func hlsEventSegmentName(name string) (int, bool, bool) {
	temporary := strings.HasSuffix(name, ".ts.tmp")
	suffix := ".ts"
	if temporary {
		suffix = ".ts.tmp"
	}
	if !strings.HasPrefix(name, "seg") || !strings.HasSuffix(name, suffix) {
		return 0, false, false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "seg"), suffix)
	if len(digits) != 3 {
		return 0, false, false
	}
	index, err := strconv.Atoi(digits)
	return index, temporary, err == nil
}

func inspectHLSEvent(ctx context.Context, tools *Tools, directory string) (HLSEventProbeResult, error) {
	files, err := scanHLSEventFiles(directory)
	if err != nil || files.temporary || files.playlistBytes == 0 || len(files.segments) == 0 {
		return HLSEventProbeResult{}, errHLSEventOutputInvalid
	}
	playlistPath := filepath.Join(directory, "index.m3u8")
	playlistBytes, err := os.ReadFile(playlistPath)
	if err != nil || len(playlistBytes) > hlsEventMaxPlaylist {
		return HLSEventProbeResult{}, errHLSEventOutputInvalid
	}
	segmentNames, err := parseHLSEventPlaylist(string(playlistBytes))
	if err != nil || len(segmentNames) != len(files.segments) {
		return HLSEventProbeResult{}, errHLSEventOutputInvalid
	}

	result := HLSEventProbeResult{PlaylistPath: playlistPath}
	for expectedIndex, name := range segmentNames {
		index, _, ok := hlsEventSegmentName(name)
		if !ok || index != expectedIndex {
			return HLSEventProbeResult{}, errHLSEventOutputInvalid
		}
		segmentPath := filepath.Join(directory, name)
		info, err := os.Lstat(segmentPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 {
			return HLSEventProbeResult{}, errHLSEventOutputInvalid
		}
		if err := validateMPEGTS(segmentPath, info.Size()); err != nil {
			return HLSEventProbeResult{}, errHLSEventOutputInvalid
		}
		metadata, err := tools.Probe(ctx, segmentPath)
		if err != nil || !hasExpectedAVCodecs(metadata) {
			return HLSEventProbeResult{}, errHLSEventOutputInvalid
		}
		result.Segments = append(result.Segments, HLSEventProbeSegment{Path: segmentPath, Bytes: info.Size()})
		result.TotalBytes += info.Size()
	}
	result.TotalBytes += files.playlistBytes
	if result.TotalBytes > hlsEventMaxBytes {
		return HLSEventProbeResult{}, errHLSEventOutputLimit
	}
	return result, nil
}

func hasExpectedAVCodecs(metadata Metadata) bool {
	video, audio := 0, 0
	for _, stream := range metadata.Streams {
		switch stream.Type {
		case "video":
			if stream.Codec != "h264" {
				return false
			}
			video++
		case "audio":
			if stream.Codec != "aac" {
				return false
			}
			audio++
		}
	}
	return video == 1 && audio == 1
}

func parseHLSEventPlaylist(playlist string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(playlist, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, errHLSEventOutputInvalid
	}
	var names []string
	var durationPending bool
	var sawVersion, sawTargetDuration, sawEvent, sawEndList bool
	var totalDuration, longestSegment float64
	var targetDuration int
	for _, rawLine := range lines[1:] {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if durationPending || sawEndList {
				return nil, errHLSEventOutputInvalid
			}
			value := strings.TrimPrefix(line, "#EXTINF:")
			secondsText, _, _ := strings.Cut(value, ",")
			seconds, err := strconv.ParseFloat(secondsText, 64)
			if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > 30 {
				return nil, errHLSEventOutputInvalid
			}
			totalDuration += seconds
			if totalDuration > 32 {
				return nil, errHLSEventOutputInvalid
			}
			if seconds > longestSegment {
				longestSegment = seconds
			}
			durationPending = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			switch {
			case strings.HasPrefix(line, "#EXT-X-VERSION:"):
				version, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-VERSION:"))
				if err != nil || version < 1 || version > 7 || sawVersion {
					return nil, errHLSEventOutputInvalid
				}
				sawVersion = true
			case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
				seconds, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
				if err != nil || seconds < 1 || seconds > 30 || sawTargetDuration {
					return nil, errHLSEventOutputInvalid
				}
				targetDuration = seconds
				sawTargetDuration = true
			case line == "#EXT-X-MEDIA-SEQUENCE:0":
			case line == "#EXT-X-PLAYLIST-TYPE:EVENT":
				if sawEvent {
					return nil, errHLSEventOutputInvalid
				}
				sawEvent = true
			case line == "#EXT-X-ENDLIST":
				if durationPending || sawEndList {
					return nil, errHLSEventOutputInvalid
				}
				sawEndList = true
			default:
				return nil, errHLSEventOutputInvalid
			}
			continue
		}
		if !durationPending || sawEndList {
			return nil, errHLSEventOutputInvalid
		}
		index, _, ok := hlsEventSegmentName(line)
		if !ok || index != len(names) || index >= hlsEventMaxSegments {
			return nil, errHLSEventOutputInvalid
		}
		names = append(names, line)
		durationPending = false
	}
	if !sawVersion || !sawTargetDuration || float64(targetDuration) < math.Ceil(longestSegment) || !sawEvent || !sawEndList || durationPending || len(names) == 0 {
		return nil, errHLSEventOutputInvalid
	}
	return names, nil
}

func validateMPEGTS(path string, size int64) error {
	if size < 188 || size%188 != 0 {
		return errHLSEventOutputInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return errHLSEventOutputInvalid
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	packet := make([]byte, 188)
	for offset := int64(0); offset < size; offset += int64(len(packet)) {
		if _, err := io.ReadFull(reader, packet); err != nil || packet[0] != 0x47 {
			return errHLSEventOutputInvalid
		}
	}
	return nil
}

func cleanupHLSEventFiles(directory string) {
	_ = os.Remove(filepath.Join(directory, "index.m3u8"))
	_ = os.Remove(filepath.Join(directory, "index.m3u8.tmp"))
	for index := 0; index < hlsEventMaxSegments; index++ {
		base := fmt.Sprintf("seg%03d.ts", index)
		_ = os.Remove(filepath.Join(directory, base))
		_ = os.Remove(filepath.Join(directory, base+".tmp"))
	}
}
