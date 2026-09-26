package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type pcmStreamRunner struct {
	metadata  string
	ffmpegRun func(context.Context, io.Writer) error

	mu             sync.Mutex
	conversionArgs []string
}

func (r *pcmStreamRunner) Run(ctx context.Context, executable string, args []string, output io.Writer) error {
	if executable == "ffprobe" {
		_, err := io.WriteString(output, r.metadata)
		return err
	}
	r.mu.Lock()
	r.conversionArgs = append([]string(nil), args...)
	r.mu.Unlock()
	if r.ffmpegRun != nil {
		return r.ffmpegRun(ctx, output)
	}
	return nil
}

func (r *pcmStreamRunner) args() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.conversionArgs...)
}

func newPCMStreamFixture(t *testing.T, runner *pcmStreamRunner) (*RemoteTools, domain.Source) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\nsegment.ts\n")
	}))
	t.Cleanup(server.Close)
	tools := NewWithRunner("ffmpeg", "ffprobe", runner)
	return NewRemote(tools, server.Client()), domain.Source{
		URL:  server.URL + "/live.m3u8",
		MIME: "application/vnd.apple.mpegurl",
		Live: true,
	}
}

func TestPCMStreamUsesExactLiveHLSAudioProfile(t *testing.T) {
	runner := &pcmStreamRunner{metadata: `{"streams":[{"index":1,"codec_type":"audio","codec_name":"aac"}]}`}
	remote, source := newPCMStreamFixture(t, runner)
	selectedAudio := 1
	if err := remote.ConvertRemote(t.Context(), source, "PCM_STREAM", domain.MediaSelection{AudioID: &selectedAudio}, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := runner.args()
	inputIndex := -1
	for i, arg := range got {
		if arg == "-i" {
			inputIndex = i
			break
		}
	}
	if inputIndex < 0 || inputIndex+1 >= len(got) || !strings.HasPrefix(got[inputIndex+1], "http://127.0.0.1:") {
		t.Fatalf("FFmpeg input did not stay behind the loopback bridge: %#v", got)
	}
	want := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-max_alloc", "67108864",
		"-threads", "2", "-protocol_whitelist", "http,tcp,pipe",
		"-allowed_extensions", "ALL", "-extension_picky", "0",
		"-format_whitelist", "mov,matroska,webm,mpegts,mp3,aac,flac,ogg,wav,hls,dash",
		"-http_proxy", "", "-i", got[inputIndex+1],
		"-map", "0:1", "-vn", "-sn", "-dn", "-map_metadata", "-1",
		"-c:a", "pcm_s16le", "-ac", "2", "-ar", "44100", "-f", "s16le", "pipe:1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected PCM stream arguments:\n got: %#v\nwant: %#v", got, want)
	}
	if PCMStreamMIME != "audio/x-zombiebox-pcm;format=s16le;rate=44100;channels=2" {
		t.Fatalf("unexpected PCM stream MIME: %q", PCMStreamMIME)
	}
}

func TestPCMStreamIsUnavailableForLocalConversion(t *testing.T) {
	input := t.TempDir() + "/audio.aac"
	if err := os.WriteFile(input, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &pcmStreamRunner{metadata: `{"streams":[{"index":0,"codec_type":"audio"}]}`}
	err := NewWithRunner("ffmpeg", "ffprobe", runner).ConvertSelected(t.Context(), input, "PCM_STREAM", domain.MediaSelection{}, io.Discard)
	if err == nil || err.Error() != "unsupported media mode" {
		t.Fatalf("local PCM conversion error = %v, want unsupported media mode", err)
	}
	if len(runner.args()) != 0 {
		t.Fatal("started FFmpeg for local PCM conversion")
	}
}

func TestPCMStreamRejectsIncompatibleSourcesAndInventories(t *testing.T) {
	tests := []struct {
		name      string
		live      bool
		mime      string
		urlSuffix string
		metadata  string
	}{
		{name: "non-live HLS", live: false, mime: "application/vnd.apple.mpegurl", metadata: `{"streams":[{"index":0,"codec_type":"audio"}]}`},
		{name: "non-HLS live", live: true, mime: "audio/aac", urlSuffix: ".aac", metadata: `{"streams":[{"index":0,"codec_type":"audio"}]}`},
		{name: "missing audio", live: true, mime: "application/vnd.apple.mpegurl", metadata: `{"streams":[]}`},
		{name: "video-bearing HLS", live: true, mime: "application/vnd.apple.mpegurl", metadata: `{"streams":[{"index":0,"codec_type":"audio"},{"index":1,"codec_type":"video"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &pcmStreamRunner{metadata: test.metadata}
			remote, source := newPCMStreamFixture(t, runner)
			source.Live = test.live
			source.MIME = test.mime
			if test.urlSuffix != "" {
				source.URL = strings.TrimSuffix(source.URL, "live.m3u8") + "live" + test.urlSuffix
			}
			if err := remote.ConvertRemote(t.Context(), source, "PCM_STREAM", domain.MediaSelection{}, io.Discard); err == nil {
				t.Fatal("accepted incompatible PCM source")
			}
			if len(runner.args()) != 0 {
				t.Fatal("started FFmpeg for an incompatible source")
			}
		})
	}
}

func TestPCMStreamCancellationAndOutputFailure(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		started := make(chan struct{})
		runner := &pcmStreamRunner{
			metadata: `{"streams":[{"index":0,"codec_type":"audio"}]}`,
			ffmpegRun: func(ctx context.Context, _ io.Writer) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			},
		}
		remote, source := newPCMStreamFixture(t, runner)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- remote.ConvertRemote(ctx, source, "PCM_STREAM", domain.MediaSelection{}, io.Discard)
		}()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("PCM conversion did not start")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("conversion error = %v, want context cancellation", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("PCM conversion did not stop after cancellation")
		}
		if len(remote.tools.jobs) != 0 {
			t.Fatal("PCM cancellation leaked a conversion job")
		}
	})

	t.Run("output error", func(t *testing.T) {
		outputErr := errors.New("sink failed")
		runner := &pcmStreamRunner{
			metadata: `{"streams":[{"index":0,"codec_type":"audio"}]}`,
			ffmpegRun: func(_ context.Context, output io.Writer) error {
				_, err := output.Write([]byte("pcm"))
				return err
			},
		}
		remote, source := newPCMStreamFixture(t, runner)
		if err := remote.ConvertRemote(t.Context(), source, "PCM_STREAM", domain.MediaSelection{}, writerFunc(func([]byte) (int, error) {
			return 0, outputErr
		})); err == nil || err.Error() != "conversion_failed" {
			t.Fatalf("conversion error = %v, want conversion_failed", err)
		}
	})
}

func TestLiveHLSRemuxStillUsesADTS(t *testing.T) {
	runner := &pcmStreamRunner{metadata: `{"streams":[{"index":0,"codec_type":"audio","codec_name":"aac"}]}`}
	remote, source := newPCMStreamFixture(t, runner)
	if err := remote.ConvertRemote(t.Context(), source, "REMUX", domain.MediaSelection{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(runner.args(), " ")
	for _, expected := range []string{"-map 0:v:0?", "-map 0:a:0?", "-c copy", "-f adts pipe:1"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("existing ADTS remux option %q missing from %q", expected, got)
		}
	}
	for _, forbidden := range []string{"pcm_s16le", "-f s16le"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("REMUX unexpectedly selected PCM output: %q", got)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
