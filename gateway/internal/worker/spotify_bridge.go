package worker

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const maxSubscribers = 8

type subscriber struct {
	ch chan []byte
}

type spotifyBridge struct {
	ctx       context.Context
	cancel    context.CancelFunc
	fifoPath  string
	keepalive *os.File

	mu              sync.Mutex
	buffer          [][]byte
	bufferBytes     int
	maxBuffer       int
	subscribers     map[*subscriber]struct{}
	currentTrackURI string
	stopped         bool
	finished        bool
	closed          bool
	cmd             *exec.Cmd
}

func newSpotifyBridge(parentCtx context.Context, fifoPath string) *spotifyBridge {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(parentCtx)
	b := &spotifyBridge{
		ctx:         ctx,
		cancel:      cancel,
		fifoPath:    fifoPath,
		maxBuffer:   128 * 1024, // ~8 seconds of 128kbps MP3
		subscribers: make(map[*subscriber]struct{}),
	}

	info, statErr := os.Lstat(fifoPath)
	if os.IsNotExist(statErr) {
		_ = os.MkdirAll(filepath.Dir(fifoPath), 0755)
		_ = syscall.Mkfifo(fifoPath, 0600)
		info, statErr = os.Lstat(fifoPath)
	}
	isFIFO := statErr == nil && info.Mode()&os.ModeNamedPipe != 0
	if isFIFO {
		if f, err := os.OpenFile(fifoPath, os.O_RDWR, 0); err == nil {
			b.keepalive = f
		}
	}

	go b.run(isFIFO)
	return b
}

func (b *spotifyBridge) run(isFIFO bool) {
	for b.ctx.Err() == nil {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()

		b.mu.Lock()
		needsKeepalive := b.keepalive == nil
		b.mu.Unlock()
		if isFIFO && needsKeepalive {
			info, statErr := os.Lstat(b.fifoPath)
			if os.IsNotExist(statErr) {
				_ = os.MkdirAll(filepath.Dir(b.fifoPath), 0755)
				_ = syscall.Mkfifo(b.fifoPath, 0600)
				info, statErr = os.Lstat(b.fifoPath)
			}
			if statErr == nil && info.Mode()&os.ModeNamedPipe != 0 {
				if f, err := os.OpenFile(b.fifoPath, os.O_RDWR, 0); err == nil {
					b.mu.Lock()
					if !b.closed {
						b.keepalive = f
					} else {
						_ = f.Close()
					}
					b.mu.Unlock()
				}
			}
			b.mu.Lock()
			needsKeepalive = b.keepalive == nil
			b.mu.Unlock()
			if needsKeepalive {
				select {
				case <-b.ctx.Done():
					return
				case <-time.After(200 * time.Millisecond):
					continue
				}
			}
		}

		cmd := exec.CommandContext(b.ctx, "ffmpeg",
			"-nostdin",
			"-hide_banner",
			"-loglevel", "error",
			"-threads", "1",
			"-fflags", "+nobuffer",
			"-probesize", "32",
			"-analyzeduration", "0",
			"-f", "s16le",
			"-ar", "44100",
			"-ac", "2",
			"-i", b.fifoPath,
			"-c:a", "libmp3lame",
			"-b:a", "128k",
			"-f", "mp3",
			"-flush_packets", "1",
			"pipe:1",
		)
		cmd.WaitDelay = time.Second
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				return cmd.Process.Kill()
			}
			return nil
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil || cmd.Start() != nil {
			select {
			case <-b.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		b.mu.Lock()
		b.cmd = cmd
		b.mu.Unlock()

		b.consumeStdout(stdout)
		_ = cmd.Wait()

		b.mu.Lock()
		b.cmd = nil
		b.mu.Unlock()

		if !isFIFO {
			b.mu.Lock()
			b.finished = true
			for sub := range b.subscribers {
				close(sub.ch)
				delete(b.subscribers, sub)
			}
			b.mu.Unlock()
			return
		}

		select {
		case <-b.ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (b *spotifyBridge) consumeStdout(stdout io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			b.broadcast(chunk)
		}
		if err != nil {
			break
		}
	}
}

func (b *spotifyBridge) broadcast(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	if b.stopped {
		b.stopped = false
		b.buffer = nil
		b.bufferBytes = 0
	}

	// Explicit bounded rolling buffer: continuously drain without blocking.
	// When no subscriber is connected, chunks roll through the buffer and
	// are evicted from the front once maxBuffer is reached.
	b.buffer = append(b.buffer, chunk)
	b.bufferBytes += len(chunk)
	for b.bufferBytes > b.maxBuffer && len(b.buffer) > 0 {
		evicted := b.buffer[0]
		b.buffer = b.buffer[1:]
		b.bufferBytes -= len(evicted)
	}

	for sub := range b.subscribers {
		select {
		case sub.ch <- chunk:
		default:
			// A missing MP3 chunk corrupts a live stream. Disconnect this slow
			// subscriber so its player can reconnect at a complete frame.
			close(sub.ch)
			delete(b.subscribers, sub)
		}
	}
}

func (b *spotifyBridge) subscribe() (*subscriber, []byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed || len(b.subscribers) >= maxSubscribers {
		return nil, nil, false
	}

	initial := b.getAlignedBufferLocked()
	sub := &subscriber{ch: make(chan []byte, 64)}
	if b.finished {
		close(sub.ch)
		return sub, initial, true
	}

	b.subscribers[sub] = struct{}{}
	return sub, initial, false
}

func (b *spotifyBridge) getAlignedBufferLocked() []byte {
	if len(b.buffer) == 0 {
		return nil
	}
	combined := make([]byte, b.bufferBytes)
	offset := 0
	for _, c := range b.buffer {
		copy(combined[offset:], c)
		offset += len(c)
	}
	syncOffset := findMP3Sync(combined)
	if syncOffset > 0 && syncOffset < len(combined) {
		return combined[syncOffset:]
	}
	return combined
}

func (b *spotifyBridge) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subscribers[sub]; ok {
		delete(b.subscribers, sub)
		close(sub.ch)
	}
}

func (b *spotifyBridge) OnStopped() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.stopped = true
	b.currentTrackURI = ""
	b.buffer = nil
	b.bufferBytes = 0
}

func (b *spotifyBridge) OnTrack(uri string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.currentTrackURI != "" && b.currentTrackURI != uri {
		b.buffer = nil
		b.bufferBytes = 0
	}
	b.currentTrackURI = uri
}

func (b *spotifyBridge) ResetBuffer() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buffer = nil
	b.bufferBytes = 0
}

func (b *spotifyBridge) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.cancel()
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
	for sub := range b.subscribers {
		close(sub.ch)
		delete(b.subscribers, sub)
	}
	if b.keepalive != nil {
		_ = b.keepalive.Close()
		b.keepalive = nil
	}
	b.mu.Unlock()
}

func (b *spotifyBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store, no-cache")
	w.Header().Set("Connection", "keep-alive")
	if r.Method == "HEAD" {
		w.WriteHeader(http.StatusOK)
		return
	}

	sub, initial, finished := b.subscribe()
	if sub == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	defer b.unsubscribe(sub)

	flusher, hasFlush := w.(http.Flusher)
	if len(initial) > 0 {
		if _, err := w.Write(initial); err != nil {
			return
		}
		if hasFlush {
			flusher.Flush()
		}
	}
	if finished {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-b.ctx.Done():
			return
		case chunk, ok := <-sub.ch:
			if !ok {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if hasFlush {
				flusher.Flush()
			}
		}
	}
}

// findMP3Sync scans data for a valid MP3 sync header (or ID3 container).
func findMP3Sync(data []byte) int {
	if len(data) >= 10 && data[0] == 'I' && data[1] == 'D' && data[2] == '3' {
		return 0
	}
	if len(data) < 4 {
		return 0
	}
	searchLimit := len(data) - 4
	if searchLimit > 8192 {
		searchLimit = 8192
	}
	for i := 0; i <= searchLimit; i++ {
		if data[i] != 0xFF {
			continue
		}
		b1 := data[i+1]
		if (b1 & 0xE0) != 0xE0 {
			continue
		}
		layer := (b1 >> 1) & 0x03
		if layer != 0x01 { // Layer III
			continue
		}
		version := int((b1 >> 3) & 0x03)
		if version == 0x01 { // reserved
			continue
		}
		b2 := data[i+2]
		bitrateIdx := int((b2 >> 4) & 0x0F)
		if bitrateIdx == 0 || bitrateIdx == 0x0F {
			continue
		}
		samplerateIdx := int((b2 >> 2) & 0x03)
		if samplerateIdx == 0x03 {
			continue
		}
		padding := int((b2 >> 1) & 0x01)
		frameLen := mp3FrameLength(version, bitrateIdx, samplerateIdx, padding)
		if frameLen <= 0 {
			continue
		}
		if i+frameLen+4 <= len(data) {
			nextB0 := data[i+frameLen]
			nextB1 := data[i+frameLen+1]
			if nextB0 == 0xFF && (nextB1&0xE0) == 0xE0 && ((nextB1>>1)&0x03) == layer {
				return i
			}
		} else {
			return i
		}
	}
	return 0
}

var mpeg1Bitrates = [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
var mpeg1Rates = [...]int{44100, 48000, 32000}
var mpeg2Bitrates = [...]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
var mpeg2Rates = [...]int{22050, 24000, 16000}

func mp3FrameLength(version, bitrateIdx, samplerateIdx, padding int) int {
	if version == 3 { // MPEG-1
		if bitrateIdx >= len(mpeg1Bitrates) || samplerateIdx >= len(mpeg1Rates) {
			return 0
		}
		return 144000*mpeg1Bitrates[bitrateIdx]/mpeg1Rates[samplerateIdx] + padding
	}
	// MPEG-2 or MPEG-2.5
	if bitrateIdx >= len(mpeg2Bitrates) || samplerateIdx >= len(mpeg2Rates) {
		return 0
	}
	rate := mpeg2Rates[samplerateIdx]
	if version == 0 { // MPEG-2.5
		rate /= 2
	}
	return 72000*mpeg2Bitrates[bitrateIdx]/rate + padding
}
