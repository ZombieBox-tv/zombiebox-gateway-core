package worker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const soloistRuntimeUID = 65531

// SoloistRuntime is a composition boundary for the optional dedicated Full
// container. The proprietary binary and key are supplied only at runtime.
// It starts lazily on primary-backend failure, never on Gateway startup.
type SoloistRuntime struct {
	ctx       context.Context
	cancel    context.CancelFunc
	token     string
	mu        sync.Mutex
	attempted bool
	failed    bool
	backend   *SoloistBackend
	output    *SoloistPCMHub
	processes []*soloistRuntimeProcess
}

type soloistRuntimeProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func NewSoloistRuntime(ctx context.Context, token string) (*SoloistRuntime, error) {
	if ctx == nil || len(token) < minimumSoloistBackendTokenBytes {
		return nil, ErrSoloistBackendConfig
	}
	lifetime, cancel := context.WithCancel(ctx)
	return &SoloistRuntime{ctx: lifetime, cancel: cancel, token: token}, nil
}

// Container admission checks supplement (never replace) the Compose/inspect
// gate: PID 1, dedicated UID/GID, no capabilities, NNP and seccomp. A host PID
// namespace, privileged launch, root process or exposed device fails closed.
func verifySoloistContainer() error {
	if os.Getpid() != 1 || os.Getuid() != soloistRuntimeUID || os.Getgid() != soloistRuntimeUID {
		return ErrSoloistIsolationUnverifiable
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return ErrSoloistIsolationUnverifiable
	}
	for _, field := range []string{"CapInh:", "CapPrm:", "CapEff:", "CapBnd:", "CapAmb:", "NoNewPrivs:", "Seccomp:"} {
		want := "0000000000000000"
		if field == "NoNewPrivs:" {
			want = "1"
		}
		if field == "Seccomp:" {
			want = "2"
		}
		found := false
		for _, line := range strings.Split(string(status), "\n") {
			values := strings.Fields(line)
			if len(values) == 2 && values[0] == field && values[1] == want {
				found = true
			}
		}
		if !found {
			return ErrSoloistIsolationUnverifiable
		}
	}
	if _, err := os.Stat("/dev/snd"); !os.IsNotExist(err) {
		return ErrSoloistIsolationUnverifiable
	}
	return nil
}

func verifySoloistRuntimeMaterial(binaryPath, keyPath, checksumPath string) error {
	info, err := os.Lstat(keyPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() == 0 || info.Size() > 4096 {
		return ErrSoloistIsolationUnverifiable
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != soloistRuntimeUID {
		return ErrSoloistIsolationUnverifiable
	}
	info, err = os.Lstat(binaryPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return ErrSoloistBinaryMissing
	}
	digest, err := os.ReadFile(checksumPath)
	if err != nil || len(digest) > 128 {
		return ErrSoloistBinaryMissing
	}
	wanted, err := hex.DecodeString(strings.TrimSpace(string(digest)))
	if err != nil || len(wanted) != sha256.Size {
		return ErrSoloistBinaryMissing
	}
	file, err := os.Open(binaryPath)
	if err != nil {
		return ErrSoloistBinaryMissing
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err = io.Copy(hasher, file); err != nil || subtle.ConstantTimeCompare(wanted, hasher.Sum(nil)) != 1 {
		return ErrSoloistBinaryMissing
	}
	return nil
}

func (r *SoloistRuntime) startProcess(name string, args ...string) (*soloistRuntimeProcess, error) {
	cmd := exec.CommandContext(r.ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Child output is discarded, including API keys, pairing details and
	// upstream diagnostics. Error messages never include cmd.Args.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return nil, errors.New("Soloist runtime process unavailable")
	}
	process := &soloistRuntimeProcess{cmd: cmd, done: make(chan struct{})}
	r.processes = append(r.processes, process)
	go func() { _ = cmd.Wait(); close(process.done) }()
	return process, nil
}

func (r *SoloistRuntime) activate(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attempted {
		if r.failed || r.backend == nil {
			return ErrSoloistBackendUnavailable
		}
		return nil
	}
	r.attempted = true
	r.failed = true
	if err := verifySoloistContainer(); err != nil {
		return err
	}
	if err := verifySoloistRuntimeMaterial("/runtime/soloist", "/run/secrets/soloist-api-key", "/runtime/soloist.sha256"); err != nil {
		return err
	}
	for _, dir := range []string{"/tmp/runtime", "/tmp/playback-cache", "/state/session"} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return ErrSoloistBackendUnavailable
		}
	}
	if err := r.startPrivateAudio(ctx); err != nil {
		return err
	}
	if _, err := r.startProcess("/usr/local/libexec/soloist-launch", "/run/secrets/soloist-api-key"); err != nil {
		return err
	}
	backend, err := NewSoloistBackend(r.ctx, "ws://127.0.0.1:8096", r.token)
	if err != nil {
		return err
	}
	reader, err := newSoloistPCMReader(r.ctx, "zombiebox_soloist")
	if err != nil {
		_ = backend.Close()
		return err
	}
	output, err := NewSoloistPCMHub(r.ctx, reader)
	if err != nil {
		_ = reader.Close()
		_ = backend.Close()
		return err
	}
	backend.AttachOutput(output)
	r.backend, r.output = backend, output
	r.failed = false
	go func() {
		select {
		case <-r.ctx.Done():
		case <-output.done:
		}
		_ = r.Close()
	}()
	// Any supervised dependency exit revokes readiness and tears down all
	// children. Restarting the optional container permits a fresh attempt.
	for _, process := range r.processes {
		go func(process *soloistRuntimeProcess) {
			select {
			case <-r.ctx.Done():
			case <-process.done:
			}
			_ = r.Close()
		}(process)
	}
	return nil
}

func (r *SoloistRuntime) startPrivateAudio(ctx context.Context) error {
	if err := os.MkdirAll("/tmp/runtime", 0700); err != nil {
		return err
	}
	if err := os.Setenv("XDG_RUNTIME_DIR", "/tmp/runtime"); err != nil {
		return err
	}
	if err := os.Setenv("PIPEWIRE_REMOTE", "zombie-soloist"); err != nil {
		return err
	}
	pipewire, err := r.startProcess("pipewire", "-c", "/usr/local/share/zombiebox/soloist-pipewire.conf")
	if err != nil {
		return err
	}
	ready, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Stat(filepath.Join("/tmp/runtime", "zombie-soloist")); err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}
		select {
		case <-ready.Done():
			return ErrSoloistBackendUnavailable
		case <-pipewire.done:
			return ErrSoloistBackendUnavailable
		case <-ticker.C:
		}
	}
	if _, err = r.startProcess("dbus-run-session", "--", "wireplumber"); err != nil {
		return err
	}
	return nil
}

func (r *SoloistRuntime) Done() <-chan struct{} { return r.ctx.Done() }

func (r *SoloistRuntime) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	candidate := sha256.Sum256([]byte(request.Header.Get("Authorization")))
	expected := sha256.Sum256([]byte("Bearer " + r.token))
	if len(request.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(candidate[:], expected[:]) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if request.URL.Path == "/activate" {
		if request.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if err := r.activate(request.Context()); err != nil {
			_ = r.Close()
			http.Error(w, "Soloist runtime unavailable", 503)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	r.mu.Lock()
	backend, failed := r.backend, r.failed
	r.mu.Unlock()
	if backend == nil || failed || r.ctx.Err() != nil {
		http.Error(w, "Soloist inactive", 503)
		return
	}
	backend.ServeHTTP(w, request)
}

func (r *SoloistRuntime) Close() error {
	r.cancel()
	r.mu.Lock()
	processes := append([]*soloistRuntimeProcess(nil), r.processes...)
	output, backend := r.output, r.backend
	r.failed = true
	r.mu.Unlock()
	if backend != nil {
		_ = backend.Close()
	}
	if output != nil {
		_ = output.Close()
	}
	for _, process := range processes {
		select {
		case <-process.done:
		default:
			_ = syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL)
			_ = process.cmd.Process.Kill()
			<-process.done
		}
	}
	return nil
}
