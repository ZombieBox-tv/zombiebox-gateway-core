package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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
	case <-time.After(3 * time.Second):
		t.Fatal("child not reaped")
	}
	if len(tools.jobs) != 0 {
		t.Fatal("capacity leaked")
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
