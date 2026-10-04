package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxSubscribers = 8

// SoloistLaunchConfig describes the runtime-provided material required to
// start an officially obtained Soloist binary inside a verified isolation
// boundary. Nothing here is baked into the image: the binary and the API key
// are supplied at container runtime only, and the launcher fails closed if
// any required material or isolation primitive is missing.
type SoloistLaunchConfig struct {
	// BinaryPath is a runtime bind-mounted path to a user-provided Soloist
	// binary. It is never vendored or redistributed by this repository.
	BinaryPath string
	// APIKeyFile is a runtime-mounted secret file (e.g. a Docker/Compose
	// secret) holding the Soloist --api-key value. It is read once, passed
	// only as argv to the isolated child, and never logged.
	APIKeyFile string
	// PipewireDevice selects Soloist's verified official output contract
	// (--pipewire-device) rather than assuming PulseAudio compatibility.
	PipewireDevice string
	// IsolatedUID is the dedicated non-root UID the child must run as. It
	// must differ from the worker process UID so the child has no shared
	// identity with the parent.
	IsolatedUID int
	// IsolatedGID is the dedicated non-root GID paired with IsolatedUID.
	IsolatedGID int
}

// ErrSoloistIsolationUnverifiable is returned instead of launching Soloist
// whenever the runtime cannot establish the required isolation invariants
// (dedicated non-root user, private PID/mount namespace, no shared secrets,
// no logging, restricted capabilities, no sibling visibility). The caller
// must fail closed rather than fall back to an unverified launch.
var ErrSoloistIsolationUnverifiable = errors.New("Soloist isolation boundary could not be verified")

// ErrSoloistBinaryMissing indicates no runtime-provided Soloist binary was
// mounted. Soloist is never baked into or redistributed with this image.
var ErrSoloistBinaryMissing = errors.New("Soloist runtime binary not provided")

// ErrSoloistReadinessUnverifiable indicates the runtime has no trustworthy
// readiness signal for the isolated child; the launcher must fail closed rather
// than report a successful start from a timer alone.
var ErrSoloistReadinessUnverifiable = errors.New("Soloist readiness signal unavailable")

// soloistIsolationTools are the host primitives required to construct the
// verified isolation boundary. All must be present; their absence is a
// fail-closed condition, not a degraded-but-working path.
var soloistIsolationTools = []string{"unshare", "setpriv"}

var execCommandContext = exec.CommandContext

// verifySoloistIsolationPrereqs checks, without launching anything, that the
// primitives needed to build a dedicated non-root user plus private PID and
// mount namespaces are present. It does not itself prove the running child
// achieved isolation; runtime verification of the started process is done by
// verifySoloistChildIsolation after start.
func verifySoloistIsolationPrereqs(cfg SoloistLaunchConfig) error {
	if strings.TrimSpace(cfg.BinaryPath) == "" {
		return ErrSoloistBinaryMissing
	}
	if info, err := os.Stat(cfg.BinaryPath); err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return ErrSoloistBinaryMissing
	}
	if strings.TrimSpace(cfg.APIKeyFile) == "" {
		return ErrSoloistIsolationUnverifiable
	}
	if info, err := os.Stat(cfg.APIKeyFile); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return ErrSoloistIsolationUnverifiable
	}
	if cfg.IsolatedUID <= 0 || cfg.IsolatedGID <= 0 || cfg.IsolatedUID == os.Getuid() {
		return ErrSoloistIsolationUnverifiable
	}
	for _, tool := range soloistIsolationTools {
		if _, err := exec.LookPath(tool); err != nil {
			return ErrSoloistIsolationUnverifiable
		}
	}
	if strings.TrimSpace(cfg.PipewireDevice) == "" {
		return ErrSoloistIsolationUnverifiable
	}
	return nil
}

// SoloistLauncher supervises one isolated Soloist child process for the
// lifetime of a single qualified fallback attempt. It never persists the
// API key to disk, logs, or argv visible outside the isolated namespace: the
// key is read from APIKeyFile and appended to the child's argv only, inside
// the unshared PID/mount namespace, immediately before exec.
type soloistProcessRunner func(context.Context, string, ...string) *exec.Cmd

type soloistIsolationVerifier func(SoloistLaunchConfig, *exec.Cmd) error

type soloistReadinessProbe func(context.Context, *exec.Cmd) error

type SoloistLauncher struct {
	cfg SoloistLaunchConfig
	cmd *exec.Cmd

	processRunner     soloistProcessRunner
	isolationVerifier soloistIsolationVerifier
	readinessProbe    soloistReadinessProbe

	mu         sync.Mutex
	started    bool
	ready      bool
	helperPath string
	waitDone   chan struct{}
}

// NewSoloistLauncher validates the isolation prerequisites and returns a
// launcher, or a fail-closed error if any invariant cannot be established.
// It does not start the child; call Start for that.
func NewSoloistLauncher(cfg SoloistLaunchConfig) (*SoloistLauncher, error) {
	if err := verifySoloistIsolationPrereqs(cfg); err != nil {
		return nil, err
	}
	return &SoloistLauncher{
		cfg:           cfg,
		processRunner: execCommandContext,
		isolationVerifier: func(cfg SoloistLaunchConfig, cmd *exec.Cmd) error {
			return verifySoloistChildIsolation(cfg, cmd)
		},
		readinessProbe: func(ctx context.Context, cmd *exec.Cmd) error {
			if cmd == nil || cmd.Process == nil {
				return errors.New("Soloist child process not started")
			}
			return errors.New("Soloist readiness output probe not configured")
		},
	}, nil
}

// Start launches Soloist under unshare (private PID + mount namespaces) and
// setpriv (dedicated non-root UID/GID, no-new-privs, capability drop). The
// API key is read from disk once and passed as argv only to that isolated
// child; it is never written to a log, an env var visible to siblings, or a
// reusable file. Start fails closed if the child cannot be confirmed running
// under the intended UID.
func (l *SoloistLauncher) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return errors.New("Soloist launcher already started")
	}

	// The parent process must not read or copy the secret before it crosses the
	// namespace boundary. The runtime-mounted key file stays on disk and is read
	// only by the child helper script after unshare/setpriv has created the
	// isolated execution context.
	helperscript, err := os.CreateTemp("", "soloist-launcher-*.sh")
	if err != nil {
		l.mu.Unlock()
		return ErrSoloistIsolationUnverifiable
	}
	if _, err := helperscript.WriteString(`#!/bin/sh
exec setpriv --reuid "$1" --regid "$2" --clear-groups --no-new-privs --bounding-set=-all --inh-caps=-all --ambient-caps=-all -- /bin/sh -c 'exec "$1" --api-key "$(tr -d "\r\n" < "$2")" --pipewire-device "$3"' soloist "$3" "$4" "$5"
`); err != nil {
		_ = helperscript.Close()
		_ = os.Remove(helperscript.Name())
		l.mu.Unlock()
		return ErrSoloistIsolationUnverifiable
	}
	if err := helperscript.Chmod(0700); err != nil {
		_ = helperscript.Close()
		_ = os.Remove(helperscript.Name())
		l.mu.Unlock()
		return ErrSoloistIsolationUnverifiable
	}
	if err := helperscript.Close(); err != nil {
		_ = os.Remove(helperscript.Name())
		l.mu.Unlock()
		return ErrSoloistIsolationUnverifiable
	}
	l.helperPath = helperscript.Name()
	l.waitDone = make(chan struct{})

	args := []string{
		"--pid", "--mount", "--mount-proc", "--fork", "--kill-child",
		"--",
		helperscript.Name(),
		strconv.Itoa(l.cfg.IsolatedUID),
		strconv.Itoa(l.cfg.IsolatedGID),
		l.cfg.BinaryPath,
		l.cfg.APIKeyFile,
		l.cfg.PipewireDevice,
	}
	cmd := l.processRunner(ctx, "unshare", args...)
	if cmd == nil {
		_ = os.Remove(l.helperPath)
		l.helperPath = ""
		l.waitDone = nil
		l.mu.Unlock()
		return ErrSoloistIsolationUnverifiable
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = os.Remove(helperscript.Name())
		l.helperPath = ""
		l.waitDone = nil
		l.mu.Unlock()
		return err
	}
	l.cmd = cmd
	l.started = true
	l.mu.Unlock()

	done := l.waitDone
	go func() {
		_ = cmd.Wait()
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.cmd == cmd {
			_ = os.Remove(l.helperPath)
			l.helperPath = ""
			l.cmd = nil
			l.started = false
			l.ready = false
		}
		close(done)
	}()
	if err := l.verifyReady(ctx, cmd); err != nil {
		l.Stop()
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.started || ctx.Err() != nil {
		return ErrSoloistReadinessUnverifiable
	}
	l.ready = true
	return nil
}

func (l *SoloistLauncher) verifyReady(ctx context.Context, cmd *exec.Cmd) error {
	if l.processRunner == nil {
		l.processRunner = execCommandContext
	}
	if l.readinessProbe == nil {
		l.readinessProbe = func(ctx context.Context, cmd *exec.Cmd) error {
			return ErrSoloistReadinessUnverifiable
		}
	}
	if l.isolationVerifier == nil {
		l.isolationVerifier = func(cfg SoloistLaunchConfig, cmd *exec.Cmd) error {
			return verifySoloistChildIsolation(cfg, cmd)
		}
	}
	if err := l.isolationVerifier(l.cfg, cmd); err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := l.readinessProbe(readyCtx, cmd); err != nil {
		return err
	}
	return nil
}

func verifySoloistChildIsolation(cfg SoloistLaunchConfig, cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("Soloist child process not started")
	}
	// unshare --fork is a supervisor in the parent's namespace. Inspect its
	// descendant, retrying only the bounded fork/setpriv transition.
	pid := 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		children, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", cmd.Process.Pid, cmd.Process.Pid))
		if err != nil {
			return ErrSoloistIsolationUnverifiable
		}
		for _, child := range strings.Fields(string(children)) {
			candidate, err := strconv.Atoi(child)
			if err != nil {
				continue
			}
			status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", candidate))
			if err != nil {
				continue
			}
			uid, err := soloistChildUIDFromStatus(string(status))
			if err == nil && uid == cfg.IsolatedUID {
				pid = candidate
				break
			}
		}
		if pid > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 0 {
		return ErrSoloistIsolationUnverifiable
	}
	pidNS, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", "pid"))
	if err != nil {
		return err
	}
	selfPIDNS, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(os.Getpid()), "ns", "pid"))
	if err != nil {
		return err
	}
	if pidNS == selfPIDNS {
		return errors.New("Soloist child did not start in a private PID namespace")
	}
	statusPath := filepath.Join("/proc", strconv.Itoa(pid), "status")
	status, err := os.ReadFile(statusPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", statusPath, err)
	}
	uid, err := soloistChildUIDFromStatus(string(status))
	if err != nil {
		return err
	}
	if uid == 0 {
		return errors.New("Soloist child is still running as root")
	}
	if cfg.IsolatedUID > 0 && uid != cfg.IsolatedUID {
		return fmt.Errorf("Soloist child UID mismatch: expected %d, got %d", cfg.IsolatedUID, uid)
	}
	return nil
}

func soloistChildUIDFromStatus(status string) (int, error) {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) != 5 {
				return 0, fmt.Errorf("malformed Uid line: %q", line)
			}
			uid, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0, fmt.Errorf("invalid child UID %q: %w", fields[1], err)
			}
			for _, value := range fields[2:] {
				other, err := strconv.Atoi(value)
				if err != nil || other != uid {
					return 0, ErrSoloistIsolationUnverifiable
				}
			}
			return uid, nil
		}
	}
	return 0, errors.New("Soloist child UID missing from /proc/<pid>/status")
}

// Stop terminates the isolated Soloist child. Killing the unshare supervisor
// process tears down its private PID namespace, which in turn reaps the
// Soloist child; no orphan process or namespace is left behind.
func (l *SoloistLauncher) Stop() {
	l.mu.Lock()
	cmd := l.cmd
	waitDone := l.waitDone
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
	l.mu.Unlock()
	if waitDone != nil {
		<-waitDone
	}
	l.mu.Lock()
	l.started = false
	l.ready = false
	if l.helperPath != "" {
		_ = os.Remove(l.helperPath)
		l.helperPath = ""
	}
	l.cmd = nil
	l.waitDone = nil
	l.mu.Unlock()
}

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
	encodedBytes    uint64
	lastEncoded     time.Time
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
	// Saturating counters expose only transport activity, never PCM, track,
	// account or stream identity. They help separate an idle decoder from a
	// downstream player failure during a supervised Connect attempt.
	if size := uint64(len(chunk)); b.encodedBytes <= ^uint64(0)-size {
		b.encodedBytes += size
	} else {
		b.encodedBytes = ^uint64(0)
	}
	b.lastEncoded = time.Now()
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

type spotifyAudioDiagnostic struct {
	Available        bool   `json:"available"`
	EncodedBytes     uint64 `json:"encodedBytes"`
	LastEncodedAgeMs int64  `json:"lastEncodedAgeMs"`
	Active           bool   `json:"active"`
}

func (b *spotifyBridge) diagnostic() spotifyAudioDiagnostic {
	if b == nil {
		return spotifyAudioDiagnostic{LastEncodedAgeMs: -1}
	}
	b.mu.Lock()
	d := spotifyAudioDiagnostic{
		Available:        !b.closed,
		EncodedBytes:     b.encodedBytes,
		LastEncodedAgeMs: -1,
	}
	last := b.lastEncoded
	b.mu.Unlock()
	if !last.IsZero() {
		age := time.Since(last)
		if age < 0 {
			age = 0
		}
		d.Active = d.Available && age < 5*time.Second
		if age > time.Minute {
			age = time.Minute
		}
		d.LastEncodedAgeMs = age.Milliseconds()
	}
	return d
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
	// Playback status may become active before the decoder writes its first PCM
	// frame. Publish the live response now so the gateway's bounded response-
	// header timeout does not turn that normal startup delay into a failed stream.
	w.WriteHeader(http.StatusOK)
	if hasFlush {
		flusher.Flush()
	}
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
