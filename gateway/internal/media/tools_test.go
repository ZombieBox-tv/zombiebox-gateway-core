package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestMain(m *testing.M) {
	if os.Getenv("ZOMBIE_MEDIA_TEST_HELPER") == "large" {
		_, _ = os.Stdout.Write(append([]byte(`{"streams":[]}`), bytes.Repeat([]byte(" "), 2<<20)...))
		os.Exit(0)
	}
	if os.Getenv("ZOMBIE_MEDIA_TEST_HELPER") == "1" {
		_, _ = os.Stdout.Write([]byte("ready"))
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if os.Getenv("ZOMBIE_MEDIA_TEST_HELPER") == "exit" {
		os.Exit(37)
	}
	os.Exit(m.Run())
}

type readyWriter struct {
	ready   chan struct{}
	written bool
}

func (w *readyWriter) Write(p []byte) (int, error) {
	if !w.written {
		close(w.ready)
		w.written = true
	}
	return len(p), nil
}
func TestCancellationReapsJobAndRejectsConcurrentWork(t *testing.T) {
	executable, _ := os.Executable()
	t.Setenv("ZOMBIE_MEDIA_TEST_HELPER", "1")
	tools := New(executable, executable)
	input := filepath.Join(t.TempDir(), "input.mp4")
	if err := os.WriteFile(input, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &readyWriter{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- tools.Convert(ctx, input, "REMUX", output) }()
	select {
	case <-output.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not start")
	}
	if err := tools.Convert(ctx, input, "REMUX", io.Discard); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrency: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if ConversionFailureClass(err) != "" {
			t.Fatalf("cancellation was converted into a media failure: %q", ConversionFailureClass(err))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("child not reaped")
	}
	if len(tools.jobs) != 0 {
		t.Fatal("capacity leaked")
	}
}

func TestConversionFailurePreservesSafeFFmpegExitCode(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOMBIE_MEDIA_TEST_HELPER", "exit")
	input := filepath.Join(t.TempDir(), "input.mp4")
	if err := os.WriteFile(input, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}

	err = NewWithRunner(executable, executable, ExecRunner{}).Convert(t.Context(), input, "REMUX", io.Discard)
	if ConversionFailureClass(err) != ConversionFailureFFmpegExit {
		t.Fatalf("failure class = %q, err = %v", ConversionFailureClass(err), err)
	}
	if code, ok := ConversionFailureExitCode(err); !ok || code != 37 {
		t.Fatalf("exit code = %d, present = %v", code, ok)
	}
	if status, ok := ConversionFailureHTTPStatus(err); ok || status != 0 {
		t.Fatalf("unexpected upstream status: %d, present = %v", status, ok)
	}
	if err.Error() != "conversion_failed" {
		t.Fatalf("failure text exposed detail: %q", err.Error())
	}
}

func TestConversionFailureSanitizesInjectedRunnerError(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input.mp4")
	if err := os.WriteFile(input, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := runFunc(func(context.Context, string, []string, io.Writer) error {
		return errors.New("failed https://media.example/input?signature=private Bearer super-secret")
	})
	err := NewWithRunner("ffmpeg", "ffprobe", runner).Convert(t.Context(), input, "REMUX", io.Discard)
	if ConversionFailureClass(err) != ConversionFailureFFmpeg {
		t.Fatalf("failure class = %q, err = %v", ConversionFailureClass(err), err)
	}
	if strings.Contains(err.Error(), "media.example") || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("failure text leaked runner details: %q", err.Error())
	}
}

func TestRealFFmpegProbeRemuxAndTranscode(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required for media integration gate")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required for media integration gate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	input := filepath.Join(t.TempDir(), "synthetic.mkv")
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=green:s=320x180:r=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "0.5", "-c:v", "libx264", "-threads", "1", "-c:a", "aac", input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v %s", err, output)
	}
	tools := New(ffmpeg, ffprobe)
	source, err := tools.Probe(ctx, input)
	if err != nil || len(source.Streams) != 2 {
		t.Fatalf("probe: %v %+v", err, source)
	}
	packets := func(path string) []byte {
		t.Helper()
		data, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v", "-show_packets", "-show_data_hash", "sha256", "-show_entries", "packet=data_hash", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, mode := range []string{"REMUX", "TRANSCODE"} {
		output := filepath.Join(t.TempDir(), "output.mp4")
		file, err := os.Create(output)
		if err != nil {
			t.Fatal(err)
		}
		err = tools.Convert(ctx, input, mode, file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := tools.Probe(ctx, output)
		if err != nil || len(metadata.Streams) != 2 || metadata.Streams[0].Codec != "h264" || metadata.Streams[1].Codec != "aac" {
			t.Fatalf("%s output: %v %+v", mode, err, metadata)
		}
		if mode == "REMUX" && !bytes.Equal(packets(input), packets(output)) {
			t.Fatal("remux changed compressed video packets")
		}
		if mode == "TRANSCODE" && (metadata.Streams[0].Width > 640 || metadata.Streams[0].Height > 360 || metadata.Streams[0].Level != 30) {
			t.Fatalf("unexpected transcode profile: %+v", metadata)
		}
	}
}

func TestRealFFmpegHybridPreservesCompressedVideo(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required for media integration gate")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required for media integration gate")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	input := filepath.Join(t.TempDir(), "source.mkv")
	output := filepath.Join(t.TempDir(), "hybrid.mp4")
	cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=green:s=320x180:r=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "0.5", "-c:v", "libx264", "-threads", "1", "-c:a", "ac3", input)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v %s", err, data)
	}
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	err = New(ffmpeg, ffprobe).Convert(ctx, input, "HYBRID", file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	metadata, err := New(ffmpeg, ffprobe).Probe(ctx, output)
	if err != nil || len(metadata.Streams) != 2 || metadata.Streams[0].Codec != "h264" || metadata.Streams[1].Codec != "aac" {
		t.Fatalf("hybrid output: %v %+v", err, metadata)
	}
	packets := func(path string) []byte {
		t.Helper()
		data, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v", "-show_packets", "-show_data_hash", "sha256", "-show_entries", "packet=data_hash", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if !bytes.Equal(packets(input), packets(output)) {
		t.Fatal("hybrid conversion changed compressed video packets")
	}
}

func TestProbeRejectsOversizedToolOutput(t *testing.T) {
	executable, _ := os.Executable()
	t.Setenv("ZOMBIE_MEDIA_TEST_HELPER", "large")
	input := filepath.Join(t.TempDir(), "input.mp4")
	if err := os.WriteFile(input, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := New(executable, executable).Probe(context.Background(), input); err == nil {
		t.Fatal("accepted oversized tool output")
	}
}

func TestHybridArgumentsCopyVideoAndBoundAudioEncode(t *testing.T) {
	input := filepath.Join(t.TempDir(), "input.mp4")
	if err := os.WriteFile(input, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var got []string
	runner := runFunc(func(_ context.Context, _ string, args []string, _ io.Writer) error {
		got = append([]string(nil), args...)
		return nil
	})
	tools := NewWithRunner("ffmpeg", "ffprobe", runner)
	if err := tools.ConvertSelected(context.Background(), input, "HYBRID", domain.MediaSelection{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-max_alloc", "67108864",
		"-threads", "2", "-protocol_whitelist", "file,pipe",
		"-format_whitelist", "mov,matroska,webm,mp3,wav,flac,ogg,avi,mpeg,mpegts,aac,asf,flv,srt,webvtt,ass",
		"-i", input, "-map", "0:v:0?", "-map", "0:a:0?", "-sn", "-dn", "-map_metadata", "-1",
		"-c:v", "copy", "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "44100",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected hybrid arguments:\n got: %#v\nwant: %#v", got, want)
	}
	for i, arg := range got {
		if arg == "libx264" || arg == "-vf" || arg == "-r" {
			t.Fatalf("hybrid argument %d re-encodes video or changes frame rate: %q", i, arg)
		}
	}
}
