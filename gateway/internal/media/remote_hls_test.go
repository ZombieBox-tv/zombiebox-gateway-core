package media

import (
	"context"
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

func TestRemoteHLSEventMapsSplitInputsAndPublishesBeforeRunnerReturns(t *testing.T) {
	runner := &hlsEventFakeRunner{
		videoCodec: "h264",
		audioCodec: "aac",
	}
	published := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	runner.onFFmpeg = func(ctx context.Context, args []string) error {
		directory, err := hlsEventOutputDir(args)
		if err != nil {
			return err
		}
		if err := writeHLSEventFixtureSegment(directory); err != nil {
			return err
		}
		if err := writeHLSEventFixturePlaylist(directory, false); err != nil {
			return err
		}
		close(published)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return writeHLSEventFixturePlaylist(directory, true)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fixture")
	}))
	defer server.Close()
	tools := NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner)
	remote := NewRemote(tools, server.Client())
	directory := privateTempDirectory(t)
	resultChannel := make(chan struct {
		result HLSEventProbeResult
		err    error
	}, 1)
	go func() {
		result, err := remote.ConvertRemoteToHLSEventProbe(t.Context(), privateSplitSource(server.URL), domain.MediaSelection{Quality: "720p"}, directory)
		resultChannel <- struct {
			result HLSEventProbeResult
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-published:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not publish its first complete segment")
	}
	playlistPath := filepath.Join(directory, "index.m3u8")
	if _, err := os.Stat(filepath.Join(directory, "seg000.ts")); err != nil {
		t.Fatalf("committed segment was not visible while Runner was active: %v", err)
	}
	playlist, err := os.ReadFile(playlistPath)
	if err != nil || !strings.Contains(string(playlist), "seg000.ts") || strings.Contains(string(playlist), "#EXT-X-ENDLIST") {
		t.Fatalf("initial EVENT playlist = %q, error = %v", playlist, err)
	}
	select {
	case outcome := <-resultChannel:
		t.Fatalf("adapter returned before Runner exited: %+v", outcome)
	default:
	}
	close(release)
	select {
	case outcome := <-resultChannel:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if len(outcome.result.Segments) != 1 || outcome.result.TotalBytes <= 0 {
			t.Fatalf("result = %+v", outcome.result)
		}
		if outcome.result.PlaylistPath != playlistPath {
			t.Fatalf("playlist path = %q", outcome.result.PlaylistPath)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adapter did not return after Runner completed")
	}

	ffmpegArgs, probeArgs := runner.snapshot()
	if len(ffmpegArgs) == 0 || len(probeArgs) != 3 {
		t.Fatalf("captured FFmpeg=%d ffprobe=%d calls", len(ffmpegArgs), len(probeArgs))
	}
	inputs := argumentValues(ffmpegArgs, "-i")
	if len(inputs) != 2 || !strings.HasSuffix(inputs[0], "/video") || !strings.HasSuffix(inputs[1], "/audio") {
		t.Fatalf("FFmpeg inputs = %q", inputs)
	}
	if !hasArgumentPair(ffmpegArgs, "-map", "0:v:0") || !hasArgumentPair(ffmpegArgs, "-map", "1:a:0") {
		t.Fatalf("split input maps missing: %q", ffmpegArgs)
	}
	if !hasArgumentPair(ffmpegArgs, "-c:v", "copy") || !hasArgumentPair(ffmpegArgs, "-c:a", "copy") {
		t.Fatalf("copy codecs missing: %q", ffmpegArgs)
	}
	if !hasArgumentPair(ffmpegArgs, "-hls_playlist_type", "event") || !hasArgumentPair(ffmpegArgs, "-hls_flags", "temp_file") || !hasArgumentPair(ffmpegArgs, "-hls_segment_type", "mpegts") {
		t.Fatalf("bounded EVENT output flags missing: %q", ffmpegArgs)
	}
	if !hasArgumentPair(ffmpegArgs, "-t", "28") || argumentValue(ffmpegArgs, "-fs") != strconv.Itoa(hlsEventMaxBytes) || hlsEventMaxSegments != 32 || hlsEventMaxBytes != 64<<20 {
		t.Fatalf("QA probe bounds changed: args=%q segments=%d bytes=%d", ffmpegArgs, hlsEventMaxSegments, hlsEventMaxBytes)
	}
	joined := strings.Join(append(append([]string(nil), ffmpegArgs...), flattenArguments(probeArgs)...), " ")
	for _, secret := range []string{"video-signature-private", "audio-signature-private", "Bearer private-header"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("process arguments exposed upstream secret %q: %q", secret, joined)
		}
	}
	if strings.Contains(joined, "libx264") || hasArgument(ffmpegArgs, "-vf") || hasArgument(ffmpegArgs, "-b:v") {
		t.Fatalf("valid Quality metadata altered copy-only output: %q", ffmpegArgs)
	}
}

func TestRemoteHLSEventRejectsUnsupportedSelectionsAndDirectories(t *testing.T) {
	runner := &hlsEventFakeRunner{videoCodec: "h264", audioCodec: "aac"}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	remote := NewRemote(NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner), server.Client())
	base := privateSplitSource(server.URL)
	tests := []struct {
		name      string
		source    domain.Source
		selection domain.MediaSelection
	}{
		{name: "live", source: markLive(base)},
		{name: "not split", source: domain.Source{URL: base.URL, MIME: base.MIME}},
		{name: "manifest", source: domain.Source{URL: server.URL + "/index.m3u8", MIME: "application/vnd.apple.mpegurl", AudioURL: base.AudioURL}},
		{name: "resume", source: base, selection: domain.MediaSelection{PositionMS: 1}},
		{name: "audio override", source: base, selection: domain.MediaSelection{AudioID: intPointer(1)}},
		{name: "invalid quality", source: base, selection: domain.MediaSelection{Quality: "source-secret-option"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := remote.ConvertRemoteToHLSEventProbe(t.Context(), test.source, test.selection, privateTempDirectory(t)); err == nil {
				t.Fatal("unsupported request was accepted")
			}
		})
	}

	actual := privateTempDirectory(t)
	symlink := filepath.Join(t.TempDir(), "linked-output")
	if err := os.Symlink(actual, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.ConvertRemoteToHLSEventProbe(t.Context(), base, domain.MediaSelection{}, symlink); err == nil {
		t.Fatal("symlink output directory was accepted")
	}
	public := t.TempDir()
	if err := os.Chmod(public, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.ConvertRemoteToHLSEventProbe(t.Context(), base, domain.MediaSelection{}, public); err == nil {
		t.Fatal("non-private output directory was accepted")
	}
	if len(runner.snapshotArgs()) != 0 {
		t.Fatal("unsupported request reached ffprobe or ffmpeg")
	}
}

func TestRemoteHLSEventRejectsUnvalidatedCodecs(t *testing.T) {
	runner := &hlsEventFakeRunner{videoCodec: "hevc", audioCodec: "aac"}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	remote := NewRemote(NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner), server.Client())
	if _, err := remote.ConvertRemoteToHLSEventProbe(t.Context(), privateSplitSource(server.URL), domain.MediaSelection{}, privateTempDirectory(t)); err == nil || !strings.Contains(err.Error(), "H.264") {
		t.Fatalf("unsupported video codec error = %v", err)
	}
	if len(runner.snapshotArgs()) != 1 {
		t.Fatal("conversion ran after invalid video probe")
	}
}

func TestRemoteHLSEventCancellationReapsAndReleasesJob(t *testing.T) {
	upstreamEntered := make(chan struct{})
	upstreamCancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/video" {
			http.NotFound(w, r)
			return
		}
		close(upstreamEntered)
		<-r.Context().Done()
		close(upstreamCancelled)
	}))
	defer server.Close()
	runner := &hlsEventFakeRunner{videoCodec: "h264", audioCodec: "aac"}
	runner.onFFmpeg = func(ctx context.Context, args []string) error {
		input := argumentValues(args, "-i")[0]
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, input, nil)
		if err != nil {
			return err
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		return ctx.Err()
	}
	tools := NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner)
	remote := NewRemote(tools, server.Client())
	ctx, cancel := context.WithCancel(t.Context())
	resultChannel := make(chan error, 1)
	directory := privateTempDirectory(t)
	go func() {
		_, err := remote.ConvertRemoteToHLSEventProbe(ctx, privateSplitSource(server.URL), domain.MediaSelection{}, directory)
		resultChannel <- err
	}()
	select {
	case <-upstreamEntered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("FFmpeg runner did not request the bridged source")
	}
	cancel()
	select {
	case <-upstreamCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not reach upstream request")
	}
	select {
	case err := <-resultChannel:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("conversion cancellation = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("conversion did not return after reaping the runner")
	}
	if len(tools.jobs) != 0 {
		t.Fatal("conversion slot remained occupied after cancellation")
	}
}

func TestRemoteHLSEventQuotaCancelsAndCleansPartialOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	quotaFile := make(chan string, 1)
	runner := &hlsEventFakeRunner{videoCodec: "h264", audioCodec: "aac"}
	runner.onFFmpeg = func(ctx context.Context, args []string) error {
		pattern := argumentValue(args, "-hls_segment_filename")
		path := strings.Replace(pattern, "%03d", "000", 1) + ".tmp"
		file, err := os.Create(path)
		if err != nil {
			return err
		}
		if err := file.Truncate(hlsEventMaxBytes + 1); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		quotaFile <- path
		<-ctx.Done()
		return ctx.Err()
	}
	tools := NewWithRunner("fixture-ffmpeg", "fixture-ffprobe", runner)
	remote := NewRemote(tools, server.Client())
	directory := privateTempDirectory(t)
	_, err := remote.ConvertRemoteToHLSEventProbe(t.Context(), privateSplitSource(server.URL), domain.MediaSelection{}, directory)
	if err != errHLSEventOutputLimit {
		t.Fatalf("quota error = %v", err)
	}
	path := <-quotaFile
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("partial output was not cleaned: %v", err)
	}
	if len(tools.jobs) != 0 {
		t.Fatal("conversion slot remained occupied after quota cancellation")
	}
}

func TestRemoteHLSEventRealFFmpegPublishesWhileInputIsStillOpen(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	fixtures := t.TempDir()
	videoPath := filepath.Join(fixtures, "video.mp4")
	audioPath := filepath.Join(fixtures, "audio.m4a")
	for _, args := range [][]string{
		{"-y", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25", "-t", "12", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-g", "50", "-keyint_min", "50", "-sc_threshold", "0", "-movflags", "+faststart", videoPath},
		{"-y", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=660:sample_rate=44100", "-t", "12", "-vn", "-c:a", "aac", "-b:a", "96k", "-ac", "2", "-movflags", "+faststart", audioPath},
	} {
		if output, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("generate test source: %v: %s", err, output)
		}
	}

	ffmpegActive := &atomic.Bool{}
	barrierEntered := make(chan struct{})
	releaseInput := make(chan struct{})
	var barrierOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var path string
		switch r.URL.Path {
		case "/video":
			path = videoPath
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
		contentType := "video/mp4"
		if r.URL.Path == "/audio" {
			contentType = "audio/mp4"
		}
		serveRangedFixture(w, r, file, info.Size(), contentType, r.URL.Path == "/video" && ffmpegActive.Load(), barrierEntered, releaseInput, &barrierOnce)
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-releaseInput:
		default:
			close(releaseInput)
		}
	}()
	runner := &realHLSEventRunner{active: ffmpegActive, runner: ExecRunner{}}
	remote := NewRemote(NewWithRunner(ffmpeg, ffprobe, runner), upstream.Client())
	directory := privateTempDirectory(t)
	resultChannel := make(chan struct {
		result HLSEventProbeResult
		err    error
	}, 1)
	go func() {
		result, err := remote.ConvertRemoteToHLSEventProbe(ctx, domain.Source{
			URL:          upstream.URL + "/video?signature=private-video",
			AudioURL:     upstream.URL + "/audio?signature=private-audio",
			MIME:         "video/mp4",
			Headers:      http.Header{"Authorization": {"Bearer fixture-video"}},
			AudioHeaders: http.Header{"Authorization": {"Bearer fixture-audio"}},
		}, domain.MediaSelection{Quality: "720p"}, directory)
		resultChannel <- struct {
			result HLSEventProbeResult
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-barrierEntered:
	case <-ctx.Done():
		t.Fatalf("FFmpeg did not reach the source read barrier: %v", ctx.Err())
	}
	deadline := time.NewTimer(10 * time.Second)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(directory, "seg000.ts")); err == nil {
			playlist, readErr := os.ReadFile(filepath.Join(directory, "index.m3u8"))
			if readErr == nil && strings.Contains(string(playlist), "seg000.ts") {
				select {
				case outcome := <-resultChannel:
					t.Fatalf("conversion returned before the held source response finished: %v", outcome.err)
				default:
				}
				break
			}
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			entries, _ := os.ReadDir(directory)
			var states []string
			for _, entry := range entries {
				info, statErr := entry.Info()
				if statErr == nil {
					states = append(states, fmt.Sprintf("%s:%d", entry.Name(), info.Size()))
				}
			}
			t.Fatalf("FFmpeg did not publish an HLS segment while the source response was held open; files=%v", states)
		case outcome := <-resultChannel:
			t.Fatalf("conversion ended before first segment was observable: %v", outcome.err)
		}
	}
	close(releaseInput)
	select {
	case outcome := <-resultChannel:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		if len(outcome.result.Segments) < 2 {
			t.Fatalf("real EVENT output has too few segments: %+v", outcome.result)
		}
		for _, segment := range outcome.result.Segments {
			metadata, err := New(ffmpeg, ffprobe).Probe(ctx, segment.Path)
			if err != nil || !hasExpectedAVCodecs(metadata) || !has720pH264(metadata) {
				t.Fatalf("real segment did not validate as H.264/AAC: %+v %v", metadata, err)
			}
		}
	case <-ctx.Done():
		t.Fatalf("conversion did not complete after releasing source: %v", ctx.Err())
	}
}

type hlsEventFakeRunner struct {
	mu         sync.Mutex
	videoCodec string
	audioCodec string
	onFFmpeg   func(context.Context, []string) error
	ffmpegArgs []string
	probeArgs  [][]string
}

func (r *hlsEventFakeRunner) Run(ctx context.Context, executable string, args []string, output io.Writer) error {
	r.mu.Lock()
	if strings.Contains(filepath.Base(executable), "ffprobe") {
		r.probeArgs = append(r.probeArgs, append([]string(nil), args...))
		r.mu.Unlock()
		input := args[len(args)-1]
		switch {
		case strings.HasSuffix(input, "/video"):
			return writeProbeFixture(output, "video", r.videoCodec)
		case strings.HasSuffix(input, "/audio"):
			return writeProbeFixture(output, "audio", r.audioCodec)
		default:
			return writeProbeFixture(output, "both", "")
		}
	}
	r.ffmpegArgs = append([]string(nil), args...)
	onFFmpeg := r.onFFmpeg
	r.mu.Unlock()
	if onFFmpeg == nil {
		return nil
	}
	return onFFmpeg(ctx, args)
}

func (r *hlsEventFakeRunner) snapshot() ([]string, [][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ffmpegArgs...), cloneArgumentLists(r.probeArgs)
}

func (r *hlsEventFakeRunner) snapshotArgs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	args := cloneArgumentLists(r.probeArgs)
	if len(r.ffmpegArgs) > 0 {
		args = append(args, append([]string(nil), r.ffmpegArgs...))
	}
	return args
}

type realHLSEventRunner struct {
	active *atomic.Bool
	runner Runner
}

func (r *realHLSEventRunner) Run(ctx context.Context, executable string, args []string, output io.Writer) error {
	if strings.Contains(filepath.Base(executable), "ffmpeg") && !strings.Contains(filepath.Base(executable), "ffprobe") {
		r.active.Store(true)
		defer r.active.Store(false)
	}
	return r.runner.Run(ctx, executable, args, output)
}

func writeProbeFixture(output io.Writer, kind, codec string) error {
	var body string
	switch kind {
	case "video":
		body = fmt.Sprintf(`{"streams":[{"index":0,"codec_type":"video","codec_name":%q,"width":1280,"height":720}]}`, codec)
	case "audio":
		body = fmt.Sprintf(`{"streams":[{"index":0,"codec_type":"audio","codec_name":%q}]}`, codec)
	default:
		body = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"},{"index":1,"codec_type":"audio","codec_name":"aac"}]}`
	}
	_, err := io.WriteString(output, body)
	return err
}

func privateSplitSource(origin string) domain.Source {
	return domain.Source{
		URL:          origin + "/video?signature=video-signature-private",
		AudioURL:     origin + "/audio?signature=audio-signature-private",
		MIME:         "video/mp4",
		Headers:      http.Header{"Authorization": {"Bearer private-header"}},
		AudioHeaders: http.Header{"Authorization": {"Bearer private-header"}},
	}
}

func privateTempDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func markLive(source domain.Source) domain.Source {
	source.Live = true
	return source
}

func hlsEventOutputDir(args []string) (string, error) {
	pattern := argumentValue(args, "-hls_segment_filename")
	if pattern == "" || !strings.HasSuffix(pattern, "seg%03d.ts") {
		return "", fmt.Errorf("missing HLS output pattern")
	}
	return strings.TrimSuffix(pattern, "seg%03d.ts"), nil
}

func writeHLSEventFixtureSegment(directory string) error {
	data := make([]byte, 188*2)
	for offset := 0; offset < len(data); offset += 188 {
		data[offset] = 0x47
	}
	return os.WriteFile(filepath.Join(directory, "seg000.ts"), data, 0600)
}

func writeHLSEventFixturePlaylist(directory string, complete bool) error {
	content := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXTINF:4.000000,\nseg000.ts\n"
	if complete {
		content += "#EXT-X-ENDLIST\n"
	}
	temporary := filepath.Join(directory, "index.m3u8.tmp")
	if err := os.WriteFile(temporary, []byte(content), 0600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(directory, "index.m3u8"))
}

func argumentValue(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func argumentValues(args []string, name string) []string {
	var values []string
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			values = append(values, args[index+1])
		}
	}
	return values
}

func hasArgument(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func hasArgumentPair(args []string, name, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name && args[index+1] == value {
			return true
		}
	}
	return false
}

func flattenArguments(lists [][]string) []string {
	var flattened []string
	for _, list := range lists {
		flattened = append(flattened, list...)
	}
	return flattened
}

func cloneArgumentLists(lists [][]string) [][]string {
	cloned := make([][]string, 0, len(lists))
	for _, list := range lists {
		cloned = append(cloned, append([]string(nil), list...))
	}
	return cloned
}

func has720pH264(metadata Metadata) bool {
	for _, stream := range metadata.Streams {
		if stream.Type == "video" && stream.Codec == "h264" && stream.Width == 1280 && stream.Height == 720 {
			return true
		}
	}
	return false
}

func intPointer(value int) *int { return &value }

func serveRangedFixture(w http.ResponseWriter, r *http.Request, file *os.File, size int64, contentType string, hold bool, entered, release chan struct{}, once *sync.Once) {
	start, end, partial, valid := fixtureRange(r.Header.Get("Range"), size)
	if !valid {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	length := end - start + 1
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	}
	if r.Method == http.MethodHead {
		return
	}
	reader := io.NewSectionReader(file, start, length)
	buffer := make([]byte, 8192)
	bytesWritten := int64(0)
	barrierAt := length * 3 / 4
	if barrierAt < 1 {
		barrierAt = 1
	}
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			if hold && bytesWritten < barrierAt && bytesWritten+int64(count) >= barrierAt {
				first := int(barrierAt - bytesWritten)
				_, _ = w.Write(buffer[:first])
				bytesWritten += int64(first)
				_ = http.NewResponseController(w).Flush()
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = w.Write(buffer[first:count])
				bytesWritten += int64(count - first)
			} else {
				_, _ = w.Write(buffer[:count])
				bytesWritten += int64(count)
			}
			_ = http.NewResponseController(w).Flush()
		}
		if err != nil {
			return
		}
	}
}

func fixtureRange(value string, size int64) (int64, int64, bool, bool) {
	if value == "" {
		return 0, size - 1, false, size > 0
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, false, false
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, false
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, false
		}
		start := size - suffix
		if start < 0 {
			start = 0
		}
		return start, size - 1, true, size > 0
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, false
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, true
}
