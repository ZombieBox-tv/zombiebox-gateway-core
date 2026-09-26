package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestRemoteFFmpegMuxPreservesBothTracks(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	video, audio := filepath.Join(dir, "video.mp4"), filepath.Join(dir, "audio.m4a")
	for _, args := range [][]string{
		{"-v", "error", "-f", "lavfi", "-i", "color=c=green:s=320x180:r=10", "-t", "2", "-c:v", "libx264", "-threads", "1", video},
		{"-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "2", "-c:a", "aac", audio},
	} {
		if output, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, output)
		}
	}
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("lost input credentials")
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Range") != "" {
			requests.Add(1)
		}
		switch r.URL.Path {
		case "/video":
			http.ServeFile(w, r, video)
		case "/audio":
			http.ServeFile(w, r, audio)
		default:
			t.Error("unexpected input path")
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	source := domain.Source{URL: upstream.URL + "/video", AudioURL: upstream.URL + "/audio", MIME: "video/mp4", Headers: http.Header{"Authorization": {"Bearer fixture-secret"}}, AudioHeaders: http.Header{"Authorization": {"Bearer fixture-secret"}}}
	local := New(ffmpeg, ffprobe)
	remote := NewRemote(local, upstream.Client())
	metadata, err := remote.ProbeRemote(ctx, source)
	if err != nil || len(metadata.Streams) != 2 {
		t.Fatalf("probe %v %+v", err, metadata)
	}
	for _, mode := range []string{"REMUX", "TRANSCODE"} {
		var output bytes.Buffer
		selection := domain.MediaSelection{}
		if mode == "TRANSCODE" {
			selection.PositionMS = 1000
		}
		if err := remote.ConvertRemote(ctx, source, mode, selection, &output); err != nil {
			t.Fatal(mode, err)
		}
		path := filepath.Join(dir, mode+".mp4")
		if err := os.WriteFile(path, output.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		result, err := local.Probe(ctx, path)
		if err != nil || len(result.Streams) != 2 || result.Streams[0].Codec != "h264" || result.Streams[1].Codec != "aac" {
			t.Fatalf("%s lost tracks: %+v %v", mode, result, err)
		}
	}
	if requests.Load() == 0 {
		t.Fatal("FFmpeg range requests were not relayed")
	}
}

func TestRemoteBridgeOwnershipAndCancellation(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	remote := NewRemote(nil, upstream.Client())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, err := remote.bridge(ctx, domain.Source{URL: upstream.URL, MIME: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	client := &http.Client{Timeout: 3 * time.Second}
	for _, target := range []string{strings.Replace(bridge.video, "/video", "/unknown", 1), strings.Replace(bridge.video, "/video", "/audio", 1), bridge.video + "/extra"} {
		response, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal(response.StatusCode)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, err := client.Get(bridge.video)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("input did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request survived cancellation")
	}
	<-done
}

func TestCopyUpstreamBodyIgnoresDownstreamWriteFailure(t *testing.T) {
	writeErr := errors.New("downstream client closed")
	err := copyUpstreamBody(remoteBodyErrorWriter{err: writeErr}, strings.NewReader("fixture body"))
	if err != nil {
		t.Fatalf("downstream write error was misclassified as upstream read failure: %v", err)
	}
}

func TestCopyUpstreamBodyPreservesOnlySourceTimeoutClassification(t *testing.T) {
	timeout := remoteBodyTimeoutError{}
	err := copyUpstreamBody(io.Discard, &remoteBodyErrorReader{err: timeout})
	if !errors.Is(err, timeout) {
		t.Fatalf("upstream read cause = %v, want timeout", err)
	}
	if class := upstreamErrorClass(err, ConversionFailureUpstreamTransport); class != ConversionFailureUpstreamTimeout {
		t.Fatalf("upstream read class = %q", class)
	}
	failure := newConversionFailure(ConversionFailureUpstreamTimeout, 0, 0)
	if failure.Error() != "conversion_failed" || strings.Contains(failure.Error(), "private") {
		t.Fatalf("failure exposed upstream details: %q", failure.Error())
	}
}

type remoteBodyErrorWriter struct {
	err error
}

func (w remoteBodyErrorWriter) Write([]byte) (int, error) { return 0, w.err }

type remoteBodyErrorReader struct {
	err  error
	done bool
}

func (r *remoteBodyErrorReader) Read(buffer []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(buffer, "partial body"), nil
	}
	return 0, r.err
}

type remoteBodyTimeoutError struct{}

func (remoteBodyTimeoutError) Error() string   { return "private upstream timeout detail" }
func (remoteBodyTimeoutError) Timeout() bool   { return true }
func (remoteBodyTimeoutError) Temporary() bool { return true }

func TestRemoteConversionPreservesBoundedUpstreamHTTPFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-header" {
			t.Error("remote bridge did not forward fixture authorization")
		}
		http.Error(w, "private-body", http.StatusForbidden)
	}))
	defer upstream.Close()

	runner := runFunc(func(ctx context.Context, _ string, args []string, _ io.Writer) error {
		input := bridgeInputFromArgs(args)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, input, nil)
		if err != nil {
			return errors.New("FFmpeg failed for https://media.example/video?signature=private")
		}
		response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		return errors.New("FFmpeg failed for https://media.example/video?signature=private")
	})
	remote := NewRemote(NewWithRunner("ffmpeg", "ffprobe", runner), upstream.Client())
	source := domain.Source{
		URL:     upstream.URL + "/media?signature=private-url",
		MIME:    "video/mp4",
		Headers: http.Header{"Authorization": {"Bearer private-header"}},
	}
	err := remote.ConvertRemote(t.Context(), source, "REMUX", domain.MediaSelection{}, io.Discard)
	if ConversionFailureClass(err) != ConversionFailureUpstreamHTTP {
		t.Fatalf("failure class = %q", ConversionFailureClass(err))
	}
	if status, ok := ConversionFailureHTTPStatus(err); !ok || status != http.StatusForbidden {
		t.Fatalf("upstream status = %d, present = %v", status, ok)
	}
	if _, ok := ConversionFailureExitCode(err); ok {
		t.Fatal("upstream failure was incorrectly reported as an FFmpeg exit")
	}
	if err.Error() != "conversion_failed" || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "signature") {
		t.Fatalf("error exposed private upstream data: %q", err.Error())
	}
}

func TestRemoteConversionClassifiesUpstreamTimeoutWithoutLeakingCause(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(finished)
	}))
	defer upstream.Close()

	runner := runFunc(func(ctx context.Context, _ string, args []string, _ io.Writer) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, bridgeInputFromArgs(args), nil)
		if err == nil {
			response, requestErr := (&http.Client{Timeout: 2 * time.Second}).Do(request)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}
		return errors.New("FFmpeg failed with upstream timeout")
	})
	remote := NewRemote(NewWithRunner("ffmpeg", "ffprobe", runner), &http.Client{Timeout: 100 * time.Millisecond})
	source := domain.Source{URL: upstream.URL + "/media?token=private-token", MIME: "video/mp4"}
	err := remote.ConvertRemote(t.Context(), source, "REMUX", domain.MediaSelection{}, io.Discard)
	if ConversionFailureClass(err) != ConversionFailureUpstreamTimeout {
		t.Fatalf("failure class = %q", ConversionFailureClass(err))
	}
	if _, ok := ConversionFailureHTTPStatus(err); ok {
		t.Fatal("timeout unexpectedly exposed an HTTP status")
	}
	if err.Error() != "conversion_failed" || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("error exposed timeout cause: %q", err.Error())
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out upstream request was not canceled")
	}
}

func bridgeInputFromArgs(args []string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "-i" {
			return args[index+1]
		}
	}
	return ""
}

func TestRemoteProbeRejectsDisguisedManifest(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}
	var nested atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/input" {
			nested.Add(1)
		}
		io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\n/forbidden.ts\n#EXT-X-ENDLIST\n")
	}))
	defer upstream.Close()
	remote := NewRemote(New("ffmpeg", ffprobe), upstream.Client())
	if _, err := remote.ProbeRemote(context.Background(), domain.Source{URL: upstream.URL + "/input", MIME: "video/mp4"}); err == nil {
		t.Fatal("accepted disguised manifest")
	}
	if nested.Load() != 0 {
		t.Fatal("followed nested network reference")
	}
}

func TestRemoteProcessArgumentsContainNoProviderSecrets(t *testing.T) {
	calls := 0
	runner := runFunc(func(_ context.Context, _ string, args []string, out io.Writer) error {
		calls++
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "private.example") || strings.Contains(joined, "secret") || !strings.Contains(joined, "http://127.0.0.1:") {
			t.Fatal("provider input escaped loopback boundary")
		}
		_, err := io.WriteString(out, `{"streams":[{"codec_type":"audio","codec_name":"aac"}]}`)
		return err
	})
	source := domain.Source{URL: "https://private.example/video?token=secret", AudioURL: "https://private.example/audio?token=secret", Headers: http.Header{"Authorization": {"Bearer secret"}}}
	remote := NewRemote(NewWithRunner("ffmpeg", "ffprobe", runner), nil)
	if _, err := remote.ProbeRemote(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if err := remote.ConvertRemote(context.Background(), source, "REMUX", domain.MediaSelection{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}

func TestLiveCandidateAllowsDeclaredManifestsButRejectsSeeking(t *testing.T) {
	source := domain.Source{URL: "https://channel.test/live", MIME: "video/mp2t", Live: true}
	if !RemoteCandidate(source) {
		t.Fatal("continuous TS rejected")
	}
	remote := NewRemote(nil, nil)
	if err := remote.ConvertRemote(t.Context(), source, "REMUX", domain.MediaSelection{PositionMS: 1}, io.Discard); err == nil {
		t.Fatal("accepted live seek")
	}
	for _, mime := range []string{"video/mp4"} {
		source.MIME = mime
		if RemoteCandidate(source) {
			t.Fatal("accepted unbounded live format", mime)
		}
	}
	source.MIME = "video/mp2t"
	source.URL += "/playlist.m3u8"
	if !RemoteCandidate(source) {
		t.Fatal("manifest graph path unavailable")
	}
}

func TestLiveTransportConversionPreservesAudioAndVideo(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	input := filepath.Join(dir, "live.ts")
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "1", "-c:v", "libx264", "-threads", "1", "-c:a", "aac", "-f", "mpegts", input}
	if output, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	fixture, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer live-fixture" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		w.Write(fixture)
	}))
	defer upstream.Close()
	remote := NewRemote(New(ffmpeg, ffprobe), upstream.Client())
	source := domain.Source{URL: upstream.URL + "/live", MIME: "video/mp2t", Live: true, Headers: http.Header{"Authorization": {"Bearer live-fixture"}}}
	for _, mode := range []string{"REMUX", "TRANSCODE"} {
		var output bytes.Buffer
		if err := remote.ConvertRemote(ctx, source, mode, domain.MediaSelection{}, &output); err != nil {
			t.Fatal(mode, err)
		}
		path := filepath.Join(dir, mode+".mp4")
		if err := os.WriteFile(path, output.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		metadata, err := New(ffmpeg, ffprobe).Probe(ctx, path)
		if err != nil || len(metadata.Streams) != 2 || metadata.Streams[0].Codec != "h264" || metadata.Streams[1].Codec != "aac" {
			t.Fatal(mode, metadata, err)
		}
	}
}
