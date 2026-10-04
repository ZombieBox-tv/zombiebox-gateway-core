package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

type stubPCMReader struct {
	mu     sync.Mutex
	chunks [][]byte
	err    error
	closed bool
}

func (r *stubPCMReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.EOF
	}
	if len(r.chunks) == 0 {
		r.closed = true
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	n := copy(p, chunk)
	if len(chunk) == n {
		return n, nil
	}
	return n, nil
}

func (r *stubPCMReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return r.err
}

func readExactPCM(stream *SoloistPCMStream, want string) ([]byte, error) {
	var got []byte
	for len(got) < len(want) {
		chunk := make([]byte, len(want)-len(got))
		n, err := io.ReadFull(stream, chunk)
		got = append(got, chunk[:n]...)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return got, err
		}
	}
	return got, nil
}

func TestSoloistPCMStreamReadsExactBytesAndTracksSession(t *testing.T) {
	reader := &stubPCMReader{chunks: [][]byte{[]byte("abc"), []byte("de")}}
	stream, err := NewSoloistPCMStream(context.Background(), "session-1", "soloist_qa_sink.monitor", func(context.Context, string) (io.ReadCloser, error) {
		return reader, nil
	})
	if err != nil {
		t.Fatalf("stream created: %v", err)
	}
	if got := stream.Session(); got != "session-1" {
		t.Fatalf("session = %q, want %q", got, "session-1")
	}
	if got := stream.Source(); got != "soloist_qa_sink.monitor" {
		t.Fatalf("source = %q, want %q", got, "soloist_qa_sink.monitor")
	}

	got, err := readExactPCM(stream, "abcde")
	if err != nil {
		t.Fatalf("read exact bytes: %v", err)
	}
	if !bytes.Equal(got, []byte("abcde")) {
		t.Fatalf("read exact bytes = %q, want %q", got, "abcde")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close error: %v", err)
	}
}

func TestSoloistPCMStreamRejectsInvalidSessionAndSource(t *testing.T) {
	if _, err := NewSoloistPCMStream(context.Background(), "", "soloist_qa_sink.monitor", func(context.Context, string) (io.ReadCloser, error) { return nil, nil }); !errors.Is(err, ErrSoloistPCMInvalidSession) {
		t.Fatalf("blank session accepted: %v", err)
	}
	if _, err := NewSoloistPCMStream(context.Background(), "session-1", "", func(context.Context, string) (io.ReadCloser, error) { return nil, nil }); !errors.Is(err, ErrSoloistPCMInvalidSession) {
		t.Fatalf("blank source accepted: %v", err)
	}
	if _, err := NewSoloistPCMStream(context.Background(), "session-1", "soloist_qa_sink.monitor", nil); !errors.Is(err, ErrSoloistPCMUnavailable) {
		t.Fatalf("nil factory accepted: %v", err)
	}
}

func TestSoloistPCMStreamClosesOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &stubPCMReader{}
	stream, err := NewSoloistPCMStream(ctx, "session-2", "soloist_qa_sink.monitor", func(context.Context, string) (io.ReadCloser, error) {
		return reader, nil
	})
	if err != nil {
		t.Fatalf("stream created: %v", err)
	}
	cancel()
	if _, err := stream.Read(make([]byte, 8)); err == nil {
		t.Fatalf("cancel did not propagate")
	}
	reader.mu.Lock()
	closed := reader.closed
	reader.mu.Unlock()
	if !closed {
		t.Fatalf("reader did not close on cancellation")
	}
}

func TestSoloistPCMProcessReaderCloseKillsHelperProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	reader := &soloistPCMProcessReader{cmd: cmd, stream: stdout}
	if err := reader.Close(); err != nil {
		t.Fatalf("close helper: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper process remained running after Close")
}

func TestSoloistPCMStreamRouteRejectsWithoutStream(t *testing.T) {
	route := &SoloistPCMRoute{}
	req := httptest.NewRequest(http.MethodGet, "/soloist-audio", nil)
	res := httptest.NewRecorder()
	route.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestSoloistPCMRouteCreatesPerSessionStreamFactoryAndSetsHeaders(t *testing.T) {
	var created int
	route := &SoloistPCMRoute{factory: func(ctx context.Context, session string) (*SoloistPCMStream, error) {
		created++
		if session != "session-1" {
			return nil, ErrSoloistPCMInvalidSession
		}
		return &SoloistPCMStream{ctx: ctx, session: session, source: "soloist_qa_sink.monitor", reader: &stubPCMReader{chunks: [][]byte{[]byte("abc"), []byte("de")}}}, nil
	}}

	req := httptest.NewRequest(http.MethodGet, "/soloist-audio?session=session-1", nil)
	res := httptest.NewRecorder()
	route.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store, no-cache" {
		t.Fatalf("Cache-Control = %q, want %q", got, "no-store, no-cache")
	}
	if got := res.Header().Get("Content-Type"); got != soloistPCMStreamMIME {
		t.Fatalf("Content-Type = %q, want %q", got, soloistPCMStreamMIME)
	}
	if got := res.Body.String(); got != "abcde" {
		t.Fatalf("body = %q, want %q", got, "abcde")
	}
	if created != 1 {
		t.Fatalf("created stream count = %d, want 1", created)
	}
}

func TestSoloistPCMRouteReleasesLeaseAndAcceptsNextSessionAfterClose(t *testing.T) {
	pipeReader, pipeWriter := io.Pipe()
	route := &SoloistPCMRoute{factory: func(ctx context.Context, session string) (*SoloistPCMStream, error) {
		if session == "session-1" {
			return &SoloistPCMStream{ctx: ctx, session: session, source: "soloist_qa_sink.monitor", reader: pipeReader}, nil
		}
		if session == "session-3" {
			return &SoloistPCMStream{ctx: ctx, session: session, source: "soloist_qa_sink.monitor", reader: &stubPCMReader{chunks: [][]byte{[]byte("ok")}}}, nil
		}
		return &SoloistPCMStream{ctx: ctx, session: session, source: "soloist_qa_sink.monitor", reader: &blockingReadCloser{}}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/soloist-audio?session=session-1", nil).WithContext(ctx)
	res := httptest.NewRecorder()
	started := make(chan struct{})
	done1 := make(chan struct{})
	go func() {
		close(started)
		defer close(done1)
		route.ServeHTTP(res, req)
	}()
	<-started

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		route.mu.Lock()
		active := route.active
		route.mu.Unlock()
		if active {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("route did not hold an active stream lease")
		default:
			runtime.Gosched()
		}
	}

	req2 := httptest.NewRequest(http.MethodGet, "/soloist-audio?session=session-2", nil)
	res2 := httptest.NewRecorder()
	route.ServeHTTP(res2, req2)
	if res2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request while active status = %d, want %d", res2.Code, http.StatusServiceUnavailable)
	}

	cancel()
	<-done1
	if route.stream != nil {
		t.Fatalf("route still retained stream after close")
	}

	req3 := httptest.NewRequest(http.MethodGet, "/soloist-audio?session=session-3", nil)
	res3 := httptest.NewRecorder()
	route.ServeHTTP(res3, req3)
	if res3.Code != http.StatusOK {
		t.Fatalf("third request after release status = %d, want %d", res3.Code, http.StatusOK)
	}
	if res3.Body.String() != "ok" {
		t.Fatalf("third request body = %q, want %q", res3.Body.String(), "ok")
	}
	_ = pipeWriter.Close()
}

func TestSoloistPCMRouteRejectsConcurrentReaders(t *testing.T) {
	route := &SoloistPCMRoute{}
	first := &SoloistPCMStream{reader: &blockingReadCloser{}}
	second := &SoloistPCMStream{reader: &blockingReadCloser{}}
	route.SetStream(first)

	req1 := httptest.NewRequest(http.MethodGet, "/soloist-audio?session=session-1", nil)
	res1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		route.ServeHTTP(res1, req1)
	}()

	time.Sleep(20 * time.Millisecond)
	req2 := httptest.NewRequest(http.MethodGet, "/soloist-audio?session=session-2", nil)
	res2 := httptest.NewRecorder()
	route.ServeHTTP(res2, req2)
	if res2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second reader status = %d, want %d", res2.Code, http.StatusServiceUnavailable)
	}
	_ = first.Close()
	<-done
	route.SetStream(second)
	_ = second.Close()
}

func TestSoloistPCMRouteClosesStreamWhenClientDisconnects(t *testing.T) {
	route := &SoloistPCMRoute{}
	stream := &SoloistPCMStream{reader: &blockingReadCloser{}}
	route.SetStream(stream)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/soloist-audio?session=session-1", nil).WithContext(ctx)
	res := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		route.ServeHTTP(res, req)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled route did not release stream")
	}
	if !stream.closed {
		t.Fatalf("stream remained open after client disconnect")
	}
}

type blockingReadCloser struct {
	closed    chan struct{}
	initOnce  sync.Once
	closeOnce sync.Once
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.initOnce.Do(func() { r.closed = make(chan struct{}) })
	<-r.closed
	return 0, io.EOF
}
func (r *blockingReadCloser) Close() error {
	r.initOnce.Do(func() { r.closed = make(chan struct{}) })
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}
