package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func writeSyntheticPCMWithDeadline(t *testing.T, fifoPath string, bytesCount int, timeout time.Duration) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifoPath, os.O_WRONLY, 0600)
		if err != nil {
			errCh <- err
			return
		}
		defer f.Close()

		chunk := make([]byte, 4096)
		for written := 0; written < bytesCount; {
			toWrite := min(len(chunk), bytesCount-written)
			n, err := f.Write(chunk[:toWrite])
			if err != nil {
				errCh <- err
				return
			}
			written += n
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("failed to write synthetic PCM: %v", err)
		}
	case <-time.After(timeout):
		t.Fatalf("synthetic PCM write timed out after %v", timeout)
	}
}

func TestBridgeLifecycleAndLateConsumer(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}

	dir := t.TempDir()
	fifo := filepath.Join(dir, "audio.pcm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := newSpotifyBridge(ctx, fifo)
	if b == nil {
		t.Fatal("failed to initialize bridge")
	}
	defer b.Close()

	// Write synthetic PCM before any consumer connects
	// 44100 samples/sec * 2 channels * 2 bytes = 176400 bytes/sec
	writeSyntheticPCMWithDeadline(t, fifo, 176400, 3*time.Second)

	// Wait briefly for ffmpeg to encode into ring buffer
	time.Sleep(300 * time.Millisecond)

	b.mu.Lock()
	buffered := b.bufferBytes
	b.mu.Unlock()
	if buffered == 0 {
		t.Fatal("expected ring buffer to contain encoded MP3 frames before consumer connects")
	}

	// Late consumer connects
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/audio", nil)
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer reqCancel()
	req = req.WithContext(reqCtx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.ServeHTTP(rec, req)
	}()

	// Give consumer time to receive initial buffer
	time.Sleep(200 * time.Millisecond)
	reqCancel() // Disconnect consumer
	wg.Wait()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("expected audio/mpeg MIME, got %s", rec.Header().Get("Content-Type"))
	}
	if rec.Body.Len() == 0 {
		t.Fatal("late consumer received no audio data from buffer")
	}

	// Verify valid MP3
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", "pipe:0", "-f", "null", "-")
	cmd.Stdin = strings.NewReader(rec.Body.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("invalid MP3 output: %s %v", out, err)
	}
}

func TestBridgeConsumerReconnect(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}

	dir := t.TempDir()
	fifo := filepath.Join(dir, "audio.pcm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := newSpotifyBridge(ctx, fifo)
	if b == nil {
		t.Fatal("failed to initialize bridge")
	}
	defer b.Close()

	server := httptest.NewServer(http.HandlerFunc(b.ServeHTTP))
	defer server.Close()

	// Feed PCM continuously in background with bounded lifetime
	stopFeed := make(chan struct{})
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		defer f.Close()
		chunk := make([]byte, 4096)
		for {
			select {
			case <-stopFeed:
				return
			case <-ctx.Done():
				return
			default:
				_, err := f.Write(chunk)
				if err != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()
	defer close(stopFeed)

	// Consumer 1 connects, reads briefly, and disconnects
	req1, _ := http.NewRequest("GET", server.URL, nil)
	c1Ctx, c1Cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	req1 = req1.WithContext(c1Ctx)
	res1, err := http.DefaultClient.Do(req1)
	if err == nil {
		_, _ = io.CopyN(io.Discard, res1.Body, 1024)
		res1.Body.Close()
	}
	c1Cancel()

	// Short pause between consumers
	time.Sleep(100 * time.Millisecond)

	// Consumer 2 connects and should stream without issues
	req2, _ := http.NewRequest("GET", server.URL, nil)
	c2Ctx, c2Cancel := context.WithTimeout(context.Background(), 2*time.Second)
	req2 = req2.WithContext(c2Ctx)
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("consumer 2 failed to connect after reconnect: %v", err)
	}
	defer res2.Body.Close()

	buf := make([]byte, 8192)
	n, err := io.ReadAtLeast(res2.Body, buf, 2048)
	c2Cancel()
	if err != nil && err != context.Canceled && !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("consumer 2 read failed: %v (read %d bytes)", err, n)
	}
	if n < 2048 {
		t.Fatalf("expected at least 2048 bytes for consumer 2, got %d", n)
	}
}

func TestBridgeBackpressureBoundedMemory(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}

	dir := t.TempDir()
	fifo := filepath.Join(dir, "audio.pcm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := newSpotifyBridge(ctx, fifo)
	if b == nil {
		t.Fatal("failed to initialize bridge")
	}
	defer b.Close()

	// Write 500 KB of PCM without any consumer connected; must complete without blocking
	writeSyntheticPCMWithDeadline(t, fifo, 500*1024, 4*time.Second)
	time.Sleep(300 * time.Millisecond)

	b.mu.Lock()
	bufferedLen := b.bufferBytes
	maxBuf := b.maxBuffer
	b.mu.Unlock()

	// Bounded ring buffer must never exceed maxBuffer (128 KB)
	if bufferedLen > maxBuf {
		t.Fatalf("buffer exceeded max capacity: %d > %d", bufferedLen, maxBuf)
	}
	if bufferedLen == 0 {
		t.Fatal("expected buffer to be populated under backpressure")
	}
}

func TestBridgeOnStoppedClearsBuffer(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}

	dir := t.TempDir()
	fifo := filepath.Join(dir, "audio.pcm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := newSpotifyBridge(ctx, fifo)
	if b == nil {
		t.Fatal("failed to initialize bridge")
	}
	defer b.Close()

	writeSyntheticPCMWithDeadline(t, fifo, 100*1024, 3*time.Second)
	time.Sleep(300 * time.Millisecond)

	b.mu.Lock()
	before := b.bufferBytes
	b.mu.Unlock()
	if before == 0 {
		t.Fatal("expected audio to be buffered before OnStopped")
	}

	b.OnStopped()

	b.mu.Lock()
	after := b.bufferBytes
	b.mu.Unlock()
	if after != 0 {
		t.Fatalf("expected empty buffer after OnStopped, got %d bytes", after)
	}
}

func TestBridgeTrackChangeClearsBuffer(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}

	dir := t.TempDir()
	fifo := filepath.Join(dir, "audio.pcm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := newSpotifyBridge(ctx, fifo)
	if b == nil {
		t.Fatal("failed to initialize bridge")
	}
	defer b.Close()

	b.OnTrack("spotify:track:1")
	writeSyntheticPCMWithDeadline(t, fifo, 100*1024, 3*time.Second)
	time.Sleep(300 * time.Millisecond)

	b.mu.Lock()
	before := b.bufferBytes
	b.mu.Unlock()
	if before == 0 {
		t.Fatal("expected audio to be buffered for track 1")
	}

	// Change track: buffer must be cleared to prevent stale audio for late subscriber
	b.OnTrack("spotify:track:2")

	b.mu.Lock()
	after := b.bufferBytes
	b.mu.Unlock()
	if after != 0 {
		t.Fatalf("expected buffer to be cleared on track change, got %d bytes", after)
	}
}

func TestBridgeFIFOCreatedLate(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}

	dir := t.TempDir()
	fifo := filepath.Join(dir, "late", "audio.pcm")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := newSpotifyBridge(ctx, fifo)
	if b == nil {
		t.Fatal("failed to initialize bridge")
	}
	defer b.Close()

	// Wait briefly then create the FIFO directory and FIFO
	time.Sleep(100 * time.Millisecond)
	_ = os.MkdirAll(filepath.Dir(fifo), 0755)
	_ = syscall.Mkfifo(fifo, 0600)

	// Write synthetic PCM
	writeSyntheticPCMWithDeadline(t, fifo, 100*1024, 3*time.Second)
	time.Sleep(300 * time.Millisecond)

	b.mu.Lock()
	buffered := b.bufferBytes
	b.mu.Unlock()
	if buffered == 0 {
		t.Fatal("expected bridge to recover and encode after late FIFO creation")
	}
}

func TestBridgeSlowConsumerIsolation(t *testing.T) {
	b := &spotifyBridge{
		maxBuffer:   128 * 1024,
		subscribers: make(map[*subscriber]struct{}),
	}

	// Normal consumer
	subNormal := &subscriber{ch: make(chan []byte, 64)}
	b.subscribers[subNormal] = struct{}{}

	// Slow consumer with completely full channel
	subSlow := &subscriber{ch: make(chan []byte, 2)}
	subSlow.ch <- []byte("1")
	subSlow.ch <- []byte("2")
	b.subscribers[subSlow] = struct{}{}

	// Broadcast should not block despite slow consumer
	testChunk := []byte("hello")
	done := make(chan struct{})
	go func() {
		b.broadcast(testChunk)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("broadcast blocked by slow consumer")
	}

	// Normal subscriber received the chunk
	select {
	case got := <-subNormal.ch:
		if string(got) != "hello" {
			t.Fatalf("unexpected chunk: %s", got)
		}
	default:
		t.Fatal("normal consumer did not receive broadcast chunk")
	}
}

func TestBridgeCleanCancellation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}

	dir := t.TempDir()
	fifo := filepath.Join(dir, "audio.pcm")
	ctx, cancel := context.WithCancel(context.Background())

	b := newSpotifyBridge(ctx, fifo)
	if b == nil {
		t.Fatal("failed to initialize bridge")
	}

	// Cancel context and close
	cancel()
	b.Close()

	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if !closed {
		t.Fatal("expected bridge to be marked closed")
	}
}
