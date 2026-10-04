package worker

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

var (
	ErrSoloistPCMInvalidSession = errors.New("invalid Soloist PCM session")
	ErrSoloistPCMUnavailable    = errors.New("Soloist PCM stream unavailable")
)

type SoloistPCMReaderFactory func(context.Context, string) (io.ReadCloser, error)

type SoloistPCMStream struct {
	ctx     context.Context
	cancel  context.CancelFunc
	session string
	source  string
	reader  io.ReadCloser

	mu        sync.Mutex
	closed    bool
	closeErr  error
	closeOnce sync.Once
}

func NewSoloistPCMOutput(ctx context.Context, session string) (*SoloistPCMStream, error) {
	return NewSoloistPCMStream(ctx, session, "zombiebox_soloist", newSoloistPCMReader)
}

func NewSoloistPCMStream(ctx context.Context, session, source string, factory SoloistPCMReaderFactory) (*SoloistPCMStream, error) {
	if ctx == nil || strings.TrimSpace(session) == "" || strings.TrimSpace(source) == "" {
		return nil, ErrSoloistPCMInvalidSession
	}
	if factory == nil {
		return nil, ErrSoloistPCMUnavailable
	}
	streamCtx, cancel := context.WithCancel(ctx)
	reader, err := factory(streamCtx, source)
	if err != nil {
		cancel()
		return nil, err
	}
	stream := &SoloistPCMStream{
		ctx:     streamCtx,
		cancel:  cancel,
		session: session,
		source:  source,
		reader:  reader,
	}
	context.AfterFunc(streamCtx, func() { _ = stream.Close() })
	return stream, nil
}

func (s *SoloistPCMStream) Session() string {
	if s == nil {
		return ""
	}
	return s.session
}

func (s *SoloistPCMStream) Source() string {
	if s == nil {
		return ""
	}
	return s.source
}

func (s *SoloistPCMStream) Read(p []byte) (int, error) {
	if s == nil {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return 0, io.EOF
	}
	if s.reader == nil {
		_ = s.Close()
		return 0, io.EOF
	}
	n, err := s.reader.Read(p)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_ = s.Close()
			return n, err
		}
		_ = s.Close()
		return n, err
	}
	if n == 0 && s.ctx != nil && s.ctx.Err() != nil {
		_ = s.Close()
		return 0, s.ctx.Err()
	}
	return n, nil
}

func (s *SoloistPCMStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
		if s.reader != nil {
			if err := s.reader.Close(); err != nil && s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}

func newSoloistPCMReader(ctx context.Context, source string) (io.ReadCloser, error) {
	if strings.TrimSpace(source) == "" {
		return nil, ErrSoloistPCMUnavailable
	}
	// This remote belongs to the container only; no default host audio server.
	cmd := exec.CommandContext(ctx, "pw-cat", "--record", "--raw",
		"--remote", "zombie-soloist", "--target", source,
		"--rate", "44100", "--channels", "2", "--format", "s16",
		"--properties", `{ "stream.capture.sink": true, "node.name": "zombiebox_capture" }`, "-")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		return nil, err
	}
	return &soloistPCMProcessReader{cmd: cmd, stream: stdout}, nil
}

type soloistPCMProcessReader struct {
	cmd       *exec.Cmd
	stream    io.ReadCloser
	closeOnce sync.Once
}

func (r *soloistPCMProcessReader) Read(p []byte) (int, error) {
	if r == nil || r.stream == nil {
		return 0, io.EOF
	}
	return r.stream.Read(p)
}

func (r *soloistPCMProcessReader) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.cmd != nil && r.cmd.Process != nil {
			_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
			_ = r.cmd.Process.Kill()
		}
		if r.stream != nil {
			_ = r.stream.Close()
		}
		if r.cmd != nil {
			_ = r.cmd.Wait()
		}
	})
	return nil
}
