package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestRemoteHLSPublisherPublishesOrderedResumePlaylistBeforeCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fixture")
	}))
	defer server.Close()
	runner := &publisherFakeRunner{videoCodec: "h264", audioCodec: "aac"}
	published := make(chan struct{})
	release := make(chan struct{})
	runner.onFFmpeg = func(_ context.Context, args []string) error {
		directory := filepath.Dir(args[len(args)-1])
		if err := writePublisherSegment(directory, 0); err != nil {
			return err
		}
		if err := writePublisherPlaylist(directory, []float64{4}, false); err != nil {
			return err
		}
		close(published)
		<-release
		if err := writePublisherSegment(directory, 1); err != nil {
			return err
		}
		return writePublisherPlaylist(directory, []float64{4, 2}, true)
	}
	remote := NewRemote(NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner), server.Client())
	root := publisherPrivateDirectory(t)
	source := publisherSource(server.URL, 10_000)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	session, err := remote.StartRemoteHLSPublisher(ctx, source, domain.MediaSelection{Quality: "1080p", PositionMS: 4_000}, root)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	directory := session.directory

	select {
	case <-published:
	case <-time.After(3 * time.Second):
		t.Fatal("publisher did not expose the first segment while FFmpeg was active")
	}
	snapshot, err := session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RemoteHLSPublisherRunning || snapshot.Complete || len(snapshot.Segments) != 1 {
		t.Fatalf("early publisher snapshot = %+v", snapshot)
	}
	if strings.Contains(string(snapshot.Playlist), "#EXT-X-ENDLIST") || !strings.Contains(string(snapshot.Playlist), "segment-00001.ts") {
		t.Fatalf("early playlist = %q", snapshot.Playlist)
	}
	if strings.Contains(string(snapshot.Playlist), "video-signature-private") || strings.Contains(string(snapshot.Playlist), "audio-signature-private") {
		t.Fatalf("playlist exposed an upstream URL: %q", snapshot.Playlist)
	}
	file, size, err := session.OpenSegment(snapshot.Segments[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil || size != snapshot.Segments[0].Bytes {
		t.Fatalf("opened segment size=%d metadata=%d close-error=%v", size, snapshot.Segments[0].Bytes, err)
	}
	select {
	case <-session.done:
		t.Fatalf("FFmpeg finished before the blocked runner was released")
	default:
	}

	close(release)
	if err := session.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err = session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RemoteHLSPublisherComplete || !snapshot.Complete || len(snapshot.Segments) != 2 || !strings.Contains(string(snapshot.Playlist), "#EXT-X-ENDLIST") {
		t.Fatalf("completed publisher snapshot = %+v", snapshot)
	}
	if snapshot.Segments[0].Sequence != 0 || snapshot.Segments[1].Sequence != 1 || snapshot.Duration != 10*time.Second || snapshot.StartPosition != 4*time.Second {
		t.Fatalf("ordered resume metadata = %+v", snapshot)
	}

	ffmpegArgs, probeArgs := runner.snapshot()
	if !containsPair(ffmpegArgs, "-map", "0:v:0") || !containsPair(ffmpegArgs, "-map", "1:a:0") || !containsPair(ffmpegArgs, "-c:v", "copy") || !containsPair(ffmpegArgs, "-c:a", "copy") {
		t.Fatalf("split stream mapping/copy not used: %q", ffmpegArgs)
	}
	if !containsPair(ffmpegArgs, "-hls_playlist_type", "event") || !containsPair(ffmpegArgs, "-hls_list_size", "0") || !containsPair(ffmpegArgs, "-t", "6.000") {
		t.Fatalf("full duration EVENT flags missing: %q", ffmpegArgs)
	}
	if countPair(ffmpegArgs, "-ss", "4.000") != 2 {
		t.Fatalf("resume position was not applied to both split inputs: %q", ffmpegArgs)
	}
	if countPair(ffmpegArgs, "-readrate", "1") != 2 || countPair(ffmpegArgs, "-readrate_initial_burst", "8") != 2 {
		t.Fatalf("split inputs are not paced after an eight-second startup burst: %q", ffmpegArgs)
	}
	if !containsPair(ffmpegArgs, "-fs", strconv.FormatInt(session.maxBytes, 10)) {
		t.Fatalf("FFmpeg was not given the session disk budget: %q", ffmpegArgs)
	}
	joined := strings.Join(append(append([]string(nil), ffmpegArgs...), flattenArguments(probeArgs)...), " ")
	for _, secret := range []string{"video-signature-private", "audio-signature-private", "Bearer publisher-private-header"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("provider secret leaked to process arguments: %q", joined)
		}
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("session output disappeared before Close: %v", err)
	}
}

func TestRemoteHLSPublisherDiskBudgetKeepsReserve(t *testing.T) {
	minAvailable := remoteHLSPublisherDiskReserve + remoteHLSPublisherDiskSlack
	if _, err := remoteHLSPublisherBudgetFromAvailable(minAvailable + remoteHLSPublisherMinBudget - 1); err == nil {
		t.Fatal("publisher admitted less than the minimum bounded output")
	}
	budget, err := remoteHLSPublisherBudgetFromAvailable(minAvailable + 5<<30)
	if err != nil || budget != 5<<30 {
		t.Fatalf("publisher did not retain reserve: budget=%d error=%v", budget, err)
	}
	budget, err = remoteHLSPublisherBudgetFromAvailable(minAvailable + remoteHLSPublisherMaxBytes + 1<<30)
	if err != nil || budget != remoteHLSPublisherMaxBytes {
		t.Fatalf("publisher exceeded absolute cap: budget=%d error=%v", budget, err)
	}
}

func TestRemoteHLSPublisherIncompleteSourceNeverGetsEndList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fixture")
	}))
	defer server.Close()
	runner := &publisherFakeRunner{videoCodec: "h264", audioCodec: "aac"}
	runner.onFFmpeg = func(_ context.Context, args []string) error {
		directory := filepath.Dir(args[len(args)-1])
		if err := writePublisherSegment(directory, 0); err != nil {
			return err
		}
		return writePublisherPlaylist(directory, []float64{3}, true)
	}
	remote := NewRemote(NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner), server.Client())
	session, err := remote.StartRemoteHLSPublisher(t.Context(), publisherSource(server.URL, 10_000), domain.MediaSelection{}, publisherPrivateDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Wait(t.Context()); err == nil || !strings.Contains(err.Error(), "requested source duration") {
		t.Fatalf("truncated input result = %v", err)
	}
	snapshot, err := session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RemoteHLSPublisherFailed || snapshot.Complete || strings.Contains(string(snapshot.Playlist), "#EXT-X-ENDLIST") {
		t.Fatalf("failed publisher claimed completion: %+v", snapshot)
	}
}

func TestRemoteHLSPublisherCancelsReapsAndCleansOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fixture")
	}))
	defer server.Close()
	entered := make(chan struct{})
	stopped := make(chan struct{})
	runner := &publisherFakeRunner{videoCodec: "h264", audioCodec: "aac"}
	runner.onFFmpeg = func(ctx context.Context, args []string) error {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}
	tools := NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner)
	remote := NewRemote(tools, server.Client())
	parent, cancel := context.WithCancel(t.Context())
	root := publisherPrivateDirectory(t)
	session, err := remote.StartRemoteHLSPublisher(parent, publisherSource(server.URL, 10_000), domain.MediaSelection{}, root)
	if err != nil {
		t.Fatal(err)
	}
	directory := session.directory
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("FFmpeg runner did not start")
	}
	cancel()
	if err := session.Wait(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publisher result = %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled process was not reaped")
	}
	session.Close()
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("session directory remained after Close: %v", err)
	}
	if len(tools.jobs) != 0 {
		t.Fatal("publisher job slot remained occupied after cancellation")
	}
}

func TestRemoteHLSPublisherCancelsWhenSegmentExceedsDiskBound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fixture")
	}))
	defer server.Close()
	created := make(chan struct{})
	runner := &publisherFakeRunner{videoCodec: "h264", audioCodec: "aac"}
	runner.onFFmpeg = func(ctx context.Context, args []string) error {
		pattern := argumentAfter(args, "-hls_segment_filename")
		path := strings.Replace(pattern, "%05d.ts", "00001.ts.tmp", 1)
		file, err := os.Create(path)
		if err != nil {
			return err
		}
		if err := file.Truncate(remoteHLSPublisherMaxSegment + 1); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		close(created)
		<-ctx.Done()
		return ctx.Err()
	}
	tools := NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner)
	remote := NewRemote(tools, server.Client())
	session, err := remote.StartRemoteHLSPublisher(t.Context(), publisherSource(server.URL, 10_000), domain.MediaSelection{}, publisherPrivateDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	directory := session.directory
	defer session.Close()
	select {
	case <-created:
	case <-time.After(3 * time.Second):
		t.Fatal("FFmpeg did not create the oversized segment")
	}
	if err := session.Wait(t.Context()); !errors.Is(err, errRemoteHLSPublisherOutput) {
		t.Fatalf("oversized output result = %v", err)
	}
	if len(tools.jobs) != 0 {
		t.Fatal("publisher job slot remained occupied after output limit cancellation")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Lstat(directory); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("over-limit publisher output was not cleaned promptly")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRemoteHLSPublisherRequiresKnownBoundedSplitSourceAndPrivateRoot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	runner := &publisherFakeRunner{videoCodec: "h264", audioCodec: "aac"}
	remote := NewRemote(NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner), server.Client())
	base := publisherSource(server.URL, 10_000)
	cases := []struct {
		name      string
		source    domain.Source
		selection domain.MediaSelection
	}{
		{name: "unknown duration", source: publisherSource(server.URL, 0)},
		{name: "duration over limit", source: publisherSource(server.URL, int64(remoteHLSPublisherMaxDuration/time.Millisecond)+1)},
		{name: "live source", source: publisherMarkLive(base)},
		{name: "combined source", source: domain.Source{URL: base.URL, MIME: base.MIME, Item: base.Item}},
		{name: "manifest source", source: domain.Source{URL: server.URL + "/index.m3u8", AudioURL: base.AudioURL, MIME: "application/vnd.apple.mpegurl", Item: base.Item}},
		{name: "resume at end", source: base, selection: domain.MediaSelection{PositionMS: 10_000}},
		{name: "audio override", source: base, selection: domain.MediaSelection{AudioID: publisherInt(1)}},
		{name: "invalid quality", source: base, selection: domain.MediaSelection{Quality: "source-secret-option"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := remote.StartRemoteHLSPublisher(t.Context(), test.source, test.selection, publisherPrivateDirectory(t)); err == nil {
				t.Fatal("invalid source or selection was accepted")
			}
		})
	}
	symlink := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(publisherPrivateDirectory(t), symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.StartRemoteHLSPublisher(t.Context(), base, domain.MediaSelection{}, symlink); err == nil {
		t.Fatal("symlink output root was accepted")
	}
	if len(runner.snapshotCalls()) != 0 {
		t.Fatal("invalid request reached ffprobe or ffmpeg")
	}
}

func TestRemoteHLSPublisherRealFFmpegPublishesWhileSplitInputIsHeldOpen(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	fixtures := t.TempDir()
	videoPath := filepath.Join(fixtures, "video.mp4")
	audioPath := filepath.Join(fixtures, "audio.m4a")
	for _, args := range [][]string{
		{"-y", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25", "-t", "12", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-g", "50", "-keyint_min", "50", "-sc_threshold", "0", "-movflags", "+faststart", videoPath},
		{"-y", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=660:sample_rate=44100", "-t", "12", "-vn", "-c:a", "aac", "-b:a", "96k", "-ac", "2", "-ar", "44100", "-movflags", "+faststart", audioPath},
	} {
		if output, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("generate split fixture: %v: %s", err, output)
		}
	}

	active := &atomic.Bool{}
	inputPaused := make(chan struct{})
	releaseInput := make(chan struct{})
	var pauseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := videoPath
		switch r.URL.Path {
		case "/video":
		case "/audio":
			path = audioPath
		default:
			http.NotFound(w, r)
			return
		}
		file, err := os.Open(path)
		if err != nil {
			http.Error(w, "fixture unavailable", http.StatusInternalServerError)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			http.Error(w, "fixture unavailable", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/video" && active.Load() {
			writer := &publisherGateWriter{ResponseWriter: w, request: r, entered: inputPaused, release: releaseInput, once: &pauseOnce}
			http.ServeContent(writer, r, filepath.Base(path), info.ModTime(), file)
			return
		}
		http.ServeContent(w, r, filepath.Base(path), info.ModTime(), file)
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-releaseInput:
		default:
			close(releaseInput)
		}
	}()
	runner := &publisherExecRunner{runner: ExecRunner{}, active: active}
	remote := NewRemote(NewWithRunner(ffmpeg, ffprobe, runner), upstream.Client())
	startedAt := time.Now()
	session, err := remote.StartRemoteHLSPublisher(ctx, publisherSource(upstream.URL, 12_000), domain.MediaSelection{Quality: "720p"}, publisherPrivateDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	select {
	case <-inputPaused:
	case <-ctx.Done():
		t.Fatalf("FFmpeg did not reach the slow source barrier: %v", ctx.Err())
	}

	deadline := time.NewTimer(10 * time.Second)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	var first RemoteHLSPublisherSnapshot
	for {
		first, err = session.Snapshot()
		if err == nil && len(first.Segments) > 0 {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			files, _ := os.ReadDir(session.directory)
			var states []string
			for _, file := range files {
				info, statErr := file.Info()
				if statErr == nil {
					states = append(states, fmt.Sprintf("%s:%d", file.Name(), info.Size()))
				}
			}
			playlist, _ := os.ReadFile(filepath.Join(session.directory, "index.m3u8"))
			t.Fatalf("no segment became visible while upstream remained held: %v; files=%v playlist=%q", err, states, playlist)
		case <-ctx.Done():
			t.Fatalf("publisher ended before early segment: %v", ctx.Err())
		}
	}
	t.Logf("first complete HLS segment became available after %s", time.Since(startedAt).Round(time.Millisecond))
	if first.Complete || strings.Contains(string(first.Playlist), "#EXT-X-ENDLIST") {
		t.Fatalf("early full-duration playlist claimed completion: %+v", first)
	}
	if len(first.Segments) == 0 || first.Segments[0].Sequence != 0 {
		t.Fatalf("first visible playlist did not begin at segment zero: %+v", first)
	}
	firstSegment, _, err := session.OpenSegment(first.Segments[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstSegment.Close(); err != nil {
		t.Fatal(err)
	}
	metadata := probePublisherSegment(ctx, ffprobe, filepath.Join(session.directory, first.Segments[0].Name))
	if !hasExpectedAVCodecs(metadata) || !has720pH264(metadata) {
		t.Fatalf("first actual MPEG-TS segment was not H.264/AAC 720p: %+v", metadata)
	}
	if output, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", filepath.Join(session.directory, first.Segments[0].Name), "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("decode first published MPEG-TS segment: %v: %s", err, output)
	}
	select {
	case <-session.done:
		t.Fatalf("publisher completed while source was held")
	default:
	}

	close(releaseInput)
	if err := session.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	completed, err := session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Complete || completed.State != RemoteHLSPublisherComplete || len(completed.Segments) < 2 || !strings.Contains(string(completed.Playlist), "#EXT-X-ENDLIST") {
		t.Fatalf("real full-duration output did not complete: %+v", completed)
	}
	previous := make(map[int]float64, 2)
	for _, segment := range completed.Segments {
		ranges, err := publisherSegmentDTSRanges(ctx, ffprobe, filepath.Join(session.directory, segment.Name))
		if err != nil || len(ranges) != 2 {
			t.Fatalf("segment %s did not contain both timestamped A/V streams: ranges=%v err=%v", segment.Name, ranges, err)
		}
		for stream, packetRange := range ranges {
			if priorEnd, ok := previous[stream]; ok && (packetRange[0] < priorEnd || packetRange[0]-priorEnd > 500*time.Millisecond.Seconds()) {
				t.Fatalf("stream %d timestamps were discontinuous at %s: prior_end=%f next_start=%f", stream, segment.Name, priorEnd, packetRange[0])
			}
			previous[stream] = packetRange[1]
		}
	}
}

type publisherFakeRunner struct {
	mu         sync.Mutex
	videoCodec string
	audioCodec string
	onFFmpeg   func(context.Context, []string) error
	ffmpegArgs []string
	probeArgs  [][]string
}

func (r *publisherFakeRunner) Run(ctx context.Context, executable string, args []string, output io.Writer) error {
	if strings.Contains(filepath.Base(executable), "ffprobe") {
		r.mu.Lock()
		r.probeArgs = append(r.probeArgs, append([]string(nil), args...))
		videoCodec, audioCodec := r.videoCodec, r.audioCodec
		r.mu.Unlock()
		input := args[len(args)-1]
		switch {
		case strings.HasSuffix(input, "/video"):
			_, err := fmt.Fprintf(output, `{"streams":[{"index":0,"codec_type":"video","codec_name":%q,"width":1280,"height":720}]}`, videoCodec)
			return err
		case strings.HasSuffix(input, "/audio"):
			_, err := fmt.Fprintf(output, `{"streams":[{"index":0,"codec_type":"audio","codec_name":%q}]}`, audioCodec)
			return err
		default:
			return errors.New("unexpected probe path")
		}
	}
	r.mu.Lock()
	r.ffmpegArgs = append([]string(nil), args...)
	run := r.onFFmpeg
	r.mu.Unlock()
	if run != nil {
		return run(ctx, args)
	}
	return nil
}

func (r *publisherFakeRunner) snapshot() ([]string, [][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ffmpegArgs...), cloneArgumentLists(r.probeArgs)
}

func (r *publisherFakeRunner) snapshotCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	args := cloneArgumentLists(r.probeArgs)
	if len(r.ffmpegArgs) > 0 {
		args = append(args, append([]string(nil), r.ffmpegArgs...))
	}
	return args
}

type publisherExecRunner struct {
	runner Runner
	active *atomic.Bool
}

func (r *publisherExecRunner) Run(ctx context.Context, executable string, args []string, output io.Writer) error {
	if filepath.Base(executable) == "ffmpeg" {
		r.active.Store(true)
		defer r.active.Store(false)
	}
	return r.runner.Run(ctx, executable, args, output)
}

type publisherGateWriter struct {
	http.ResponseWriter
	request *http.Request
	entered chan struct{}
	release <-chan struct{}
	once    *sync.Once
	written int64
}

func (w *publisherGateWriter) Write(data []byte) (int, error) {
	length, _ := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64)
	barrier := length * 3 / 4
	if barrier < 1 {
		barrier = 1
	}
	if w.written < barrier && w.written+int64(len(data)) >= barrier {
		before := int(barrier - w.written)
		count, err := w.ResponseWriter.Write(data[:before])
		w.written += int64(count)
		if err != nil {
			return count, err
		}
		w.flush()
		w.once.Do(func() { close(w.entered) })
		select {
		case <-w.release:
		case <-w.request.Context().Done():
			return count, w.request.Context().Err()
		}
		remainder, err := w.ResponseWriter.Write(data[before:])
		w.written += int64(remainder)
		return count + remainder, err
	}
	count, err := w.ResponseWriter.Write(data)
	w.written += int64(count)
	return count, err
}

func (w *publisherGateWriter) flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func publisherSource(origin string, durationMS int64) domain.Source {
	return domain.Source{
		URL:          origin + "/video?signature=video-signature-private",
		AudioURL:     origin + "/audio?signature=audio-signature-private",
		MIME:         "video/mp4",
		Headers:      http.Header{"Authorization": {"Bearer publisher-private-header"}},
		AudioHeaders: http.Header{"Authorization": {"Bearer publisher-private-header"}},
		Item:         domain.Item{ID: "publisher-fixture", Provider: "youtube", DurationMS: durationMS},
	}
}

func publisherPrivateDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func publisherMarkLive(source domain.Source) domain.Source {
	source.Live = true
	return source
}

func publisherInt(value int) *int { return &value }

func writePublisherSegment(directory string, sequence int) error {
	path := filepath.Join(directory, fmt.Sprintf("segment-%05d.ts", sequence+1))
	data := make([]byte, 188*2)
	for offset := 0; offset < len(data); offset += 188 {
		data[offset] = 0x47
	}
	return os.WriteFile(path, data, 0600)
}

func writePublisherPlaylist(directory string, durations []float64, complete bool) error {
	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-PLAYLIST-TYPE:EVENT\n")
	for index, duration := range durations {
		fmt.Fprintf(&playlist, "#EXTINF:%.3f,\nsegment-%05d.ts\n", duration, index+1)
	}
	if complete {
		playlist.WriteString("#EXT-X-ENDLIST\n")
	}
	temporary := filepath.Join(directory, "index.m3u8.tmp")
	if err := os.WriteFile(temporary, []byte(playlist.String()), 0600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(directory, "index.m3u8"))
}

func argumentAfter(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func containsPair(args []string, name, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name && args[index+1] == value {
			return true
		}
	}
	return false
}

func countPair(args []string, name, value string) int {
	count := 0
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name && args[index+1] == value {
			count++
		}
	}
	return count
}

func probePublisherSegment(ctx context.Context, ffprobe, path string) Metadata {
	output, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "stream=codec_type,codec_name,width,height", "-of", "json", path).Output()
	if err != nil {
		return Metadata{}
	}
	var metadata Metadata
	if err := json.Unmarshal(output, &metadata); err != nil {
		return Metadata{}
	}
	return metadata
}

func publisherSegmentDTSRanges(ctx context.Context, ffprobe, path string) (map[int][2]float64, error) {
	output, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_packets", "-show_entries", "packet=stream_index,dts_time", "-of", "json", path).Output()
	if err != nil {
		return nil, err
	}
	var result struct {
		Packets []struct {
			StreamIndex int    `json:"stream_index"`
			DTS         string `json:"dts_time"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, err
	}
	ranges := make(map[int][2]float64, 2)
	previous := make(map[int]float64, 2)
	for _, packet := range result.Packets {
		dts, err := strconv.ParseFloat(packet.DTS, 64)
		if err != nil {
			return nil, err
		}
		if prior, ok := previous[packet.StreamIndex]; ok && dts < prior {
			return nil, fmt.Errorf("stream %d DTS regressed from %f to %f", packet.StreamIndex, prior, dts)
		}
		if packetRange, ok := ranges[packet.StreamIndex]; ok {
			ranges[packet.StreamIndex] = [2]float64{packetRange[0], dts}
		} else {
			ranges[packet.StreamIndex] = [2]float64{dts, dts}
		}
		previous[packet.StreamIndex] = dts
	}
	return ranges, nil
}
