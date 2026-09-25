package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestLiveAudioHLSRemuxLatencyAndOutput(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}

	dir := t.TempDir()

	// Generate 4 AAC 44.1kHz stereo TS segments (~0.975s each, like AirPlay worker)
	segNames := []string{"audio0.ts", "audio1.ts", "audio2.ts", "audio3.ts"}
	for i, seg := range segNames {
		segPath := filepath.Join(dir, seg)
		args := []string{
			"-y", "-nostdin", "-v", "error",
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=44100", 440+i*110),
			"-t", "0.975",
			"-c:a", "aac", "-b:a", "128k", "-ac", "2",
			"-f", "mpegts", segPath,
		}
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("failed to generate segment %s: %v %s", seg, err, out)
		}
	}

	// Live playlist (no EXT-X-ENDLIST)
	manifestContent := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:1\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:0.975,\n" +
		"audio0.ts\n" +
		"#EXTINF:0.975,\n" +
		"audio1.ts\n" +
		"#EXTINF:0.975,\n" +
		"audio2.ts\n" +
		"#EXTINF:0.975,\n" +
		"audio3.ts\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Upstream request: %s %s", r.Method, r.URL.Path)
		if r.URL.Path == "/audio.m3u8" {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(200)
			w.Write([]byte(manifestContent))
			return
		}
		segFile := filepath.Join(dir, filepath.Base(r.URL.Path))
		if data, err := os.ReadFile(segFile); err == nil {
			w.Header().Set("Content-Type", "video/mp2t")
			w.WriteHeader(200)
			w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	tools := New(ffmpeg, ffprobe)
	remote := NewRemote(tools, upstream.Client())
	source := domain.Source{
		URL:  upstream.URL + "/audio.m3u8",
		MIME: "application/vnd.apple.mpegurl",
		Live: true,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	pr, pw := io.Pipe()
	defer pr.Close()

	start := time.Now()
	convertErrChan := make(chan error, 1)
	go func() {
		err := remote.ConvertRemote(ctx, source, "REMUX", domain.MediaSelection{}, pw)
		pw.CloseWithError(err)
		convertErrChan <- err
	}()

	// Measure first byte
	buf := make([]byte, 1024)
	n, readErr := pr.Read(buf)
	firstByteLatency := time.Since(start)
	t.Logf("First read returned %d bytes after %v (err=%v)", n, firstByteLatency, readErr)
	if n > 0 {
		t.Logf("First %d bytes: %q", n, buf[:min(n, 64)])
	}

	// Collect data for 3 seconds
	totalBytes := n
	deadline := time.Now().Add(3 * time.Second)
	var allBytes bytes.Buffer
	if n > 0 {
		allBytes.Write(buf[:n])
	}

	for time.Now().Before(deadline) {
		b := make([]byte, 4096)
		readDone := make(chan int, 1)
		readErrChan := make(chan error, 1)
		go func() {
			num, e := pr.Read(b)
			if e != nil {
				readErrChan <- e
			} else {
				readDone <- num
			}
		}()

		select {
		case num := <-readDone:
			totalBytes += num
			allBytes.Write(b[:num])
			t.Logf("Read %d bytes, total=%d at %v", num, totalBytes, time.Since(start))
		case e := <-readErrChan:
			t.Logf("Read error at %v: %v (total=%d)", time.Since(start), e, totalBytes)
			break
		case <-time.After(500 * time.Millisecond):
			t.Logf("No data for 500ms at %v (total=%d)", time.Since(start), totalBytes)
		}
	}

	cancel()
	t.Logf("Finished collection: total bytes = %d in %v", totalBytes, time.Since(start))
	if totalBytes < 4096 {
		t.Fatalf("expected at least 4KB of playable ADTS audio, got %d bytes", totalBytes)
	}
	outPath := filepath.Join(dir, "output.aac")
	if err := os.WriteFile(outPath, allBytes.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()
	meta, err := tools.Probe(probeCtx, outPath)
	if err != nil {
		t.Fatalf("failed to probe remuxed live ADTS: %v", err)
	}
	if len(meta.Streams) != 1 || meta.Streams[0].Type != "audio" || meta.Streams[0].Codec != "aac" {
		t.Fatalf("unexpected stream metadata: %+v", meta)
	}
	t.Logf("Probe successful! Stream codec=%s type=%s", meta.Streams[0].Codec, meta.Streams[0].Type)
}

func TestLiveAudioHLSRemuxCancellationReleasesJob(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}

	dir := t.TempDir()
	segPath := filepath.Join(dir, "audio0.ts")
	args := []string{
		"-y", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100",
		"-t", "0.975",
		"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-f", "mpegts", segPath,
	}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("failed to generate segment: %v %s", err, out)
	}

	manifestContent := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:0.975,\naudio0.ts\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/audio.m3u8" {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(200)
			w.Write([]byte(manifestContent))
			return
		}
		data, err := os.ReadFile(segPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(200)
		w.Write(data)
	}))
	defer upstream.Close()

	tools := New(ffmpeg, ffprobe)
	remote := NewRemote(tools, upstream.Client())
	source := domain.Source{
		URL:  upstream.URL + "/audio.m3u8",
		MIME: "application/vnd.apple.mpegurl",
		Live: true,
	}

	ctx, cancel := context.WithCancel(t.Context())
	pr, pw := io.Pipe()

	done := make(chan error, 1)
	go func() {
		err := remote.ConvertRemote(ctx, source, "REMUX", domain.MediaSelection{}, pw)
		pw.CloseWithError(err)
		done <- err
	}()

	// Wait until conversion starts
	buf := make([]byte, 1024)
	if _, err := pr.Read(buf); err != nil {
		t.Fatalf("read failed: %v", err)
	}

	// Cancel context and verify ConvertRemote terminates promptly
	cancel()
	// The HTTP response writer stops accepting bytes on disconnect. Close the
	// pipe here too, since ADTS can already be writing another AAC frame.
	pr.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ConvertRemote did not terminate after context cancellation")
	}

	// Verify job slot was freed by starting a probe/job
	jobCtx, jobCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer jobCancel()
	select {
	case tools.jobs <- struct{}{}:
		<-tools.jobs
	case <-jobCtx.Done():
		t.Fatal("job slot was not freed after cancellation")
	}
}

func TestLiveAudioHLSRemuxUpstreamFailure(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	tools := New(ffmpeg, ffprobe)
	remote := NewRemote(tools, upstream.Client())
	source := domain.Source{
		URL:  upstream.URL + "/audio.m3u8",
		MIME: "application/vnd.apple.mpegurl",
		Live: true,
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err = remote.ConvertRemote(ctx, source, "REMUX", domain.MediaSelection{}, io.Discard)
	if err == nil {
		t.Fatal("expected error on upstream 500, got nil")
	}
}
