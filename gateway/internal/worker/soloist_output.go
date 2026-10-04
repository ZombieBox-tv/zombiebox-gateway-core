package worker

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"time"
)

const soloistPCMFormat = "s16le"
const soloistPCMFrameBytes = 4     // stereo signed 16-bit little-endian, 44100 Hz
const soloistPCMProbeFrames = 4410 // at least 100 ms of advancing non-silent PCM
const soloistPCMOutputAge = time.Second

// SoloistPCMHub drains one private sink monitor continuously. There is no
// replay buffer, file, cached track, or persistent audio. A slow listener is
// disconnected rather than blocking capture or accumulating an export.
type SoloistPCMHub struct {
	mu           sync.Mutex
	reader       io.ReadCloser
	listeners    map[*soloistPCMListener]struct{}
	done         chan struct{}
	closeOnce    sync.Once
	cancel       context.CancelFunc
	now          func() time.Time
	signalFrames uint64
	lastSignal   time.Time
	ended        bool
}

type soloistPCMListener struct {
	hub       *SoloistPCMHub
	chunks    chan []byte
	pending   []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func NewSoloistPCMHub(ctx context.Context, reader io.ReadCloser) (*SoloistPCMHub, error) {
	if ctx == nil || reader == nil {
		return nil, ErrSoloistPCMUnavailable
	}
	lifetime, cancel := context.WithCancel(ctx)
	hub := &SoloistPCMHub{reader: reader, listeners: make(map[*soloistPCMListener]struct{}), done: make(chan struct{}), cancel: cancel, now: time.Now}
	go hub.capture()
	go func() {
		select {
		case <-lifetime.Done():
			_ = hub.Close()
		case <-hub.done:
		}
	}()
	return hub, nil
}

func (h *SoloistPCMHub) capture() {
	defer close(h.done)
	defer func() {
		h.mu.Lock()
		h.ended = true
		h.signalFrames = 0
		h.lastSignal = time.Time{}
		for listener := range h.listeners {
			listener.stopLocked()
			delete(h.listeners, listener)
		}
		h.mu.Unlock()
		_ = h.reader.Close()
	}()
	for {
		data := make([]byte, 4096)
		n, err := io.ReadFull(h.reader, data)
		// Truncated frames never reach the declared-format relay.
		data = data[:n-n%soloistPCMFrameBytes]
		if len(data) > 0 {
			h.publish(data)
		}
		if err != nil {
			return
		}
	}
}

func (h *SoloistPCMHub) publish(data []byte) {
	nonSilent := false
	for offset := 0; offset+2 <= len(data); offset += 2 {
		if binary.LittleEndian.Uint16(data[offset:]) != 0 {
			nonSilent = true
			break
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if !nonSilent {
		h.signalFrames = 0
		h.lastSignal = time.Time{}
	} else {
		if now.Sub(h.lastSignal) > soloistPCMOutputAge {
			h.signalFrames = 0
		}
		h.signalFrames = min(uint64(soloistPCMProbeFrames), h.signalFrames+uint64(len(data)/soloistPCMFrameBytes))
		h.lastSignal = now
	}
	for listener := range h.listeners {
		select {
		case listener.chunks <- data:
		default:
			listener.stopLocked()
			delete(h.listeners, listener)
		}
	}
}

// Invalidate fences reconnect/authentication changes and closes listeners so
// old queued samples cannot qualify or leak into a new Connect session.
func (h *SoloistPCMHub) Invalidate() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.signalFrames = 0
	h.lastSignal = time.Time{}
	for listener := range h.listeners {
		listener.stopLocked()
		delete(h.listeners, listener)
	}
}

func (h *SoloistPCMHub) Active() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.ended && h.signalFrames >= soloistPCMProbeFrames && !h.lastSignal.IsZero() && h.now().Sub(h.lastSignal) <= soloistPCMOutputAge
}

// WaitActive is the bounded readiness probe. Time alone cannot satisfy it.
func (h *SoloistPCMHub) WaitActive(ctx context.Context) error {
	if h == nil || ctx == nil {
		return ErrSoloistReadinessUnverifiable
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if h.Active() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.done:
			return ErrSoloistReadinessUnverifiable
		case <-ticker.C:
		}
	}
}

func (h *SoloistPCMHub) Stream(ctx context.Context, session string) (*SoloistPCMStream, error) {
	if !h.Active() {
		return nil, ErrSoloistPCMUnavailable
	}
	return NewSoloistPCMStream(ctx, session, "zombiebox_soloist", func(context.Context, string) (io.ReadCloser, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.ended || len(h.listeners) >= 1 {
			return nil, ErrSoloistPCMUnavailable
		}
		listener := &soloistPCMListener{hub: h, chunks: make(chan []byte, 8), closed: make(chan struct{})}
		h.listeners[listener] = struct{}{}
		return listener, nil
	})
}

func (h *SoloistPCMHub) Close() error {
	if h == nil {
		return nil
	}
	h.closeOnce.Do(func() { h.cancel(); _ = h.reader.Close() })
	<-h.done
	return nil
}

func (l *soloistPCMListener) stopLocked() { l.closeOnce.Do(func() { close(l.closed) }) }
func (l *soloistPCMListener) Close() error {
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	l.stopLocked()
	delete(l.hub.listeners, l)
	return nil
}
func (l *soloistPCMListener) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	select {
	case <-l.closed:
		return 0, io.EOF
	default:
	}
	if len(l.pending) == 0 {
		select {
		case <-l.closed:
			return 0, io.EOF
		case l.pending = <-l.chunks:
		}
		select {
		case <-l.closed:
			return 0, io.EOF
		default:
		}
	}
	n := copy(data, l.pending)
	l.pending = l.pending[n:]
	return n, nil
}

var _ io.ReadCloser = (*soloistPCMListener)(nil)
