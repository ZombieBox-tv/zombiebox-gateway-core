package worker

import (
	"context"
	"errors"
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

func TestSoloistLauncherHidesAPIKeyFromParentArgs(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "soloist")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatalf("write fake Soloist binary: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "soloist-api-key")
	if err := os.WriteFile(keyFile, []byte("super-secret-key\n"), 0600); err != nil {
		t.Fatalf("write Soloist API key file: %v", err)
	}

	orderDir := t.TempDir()
	orderFile := filepath.Join(orderDir, "launch-order.txt")
	binDir := filepath.Join(orderDir, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatalf("mkdir fake bin dir: %v", err)
	}
	unsharePath := filepath.Join(binDir, "unshare")
	setprivPath := filepath.Join(binDir, "setpriv")
	realTrPath, err := exec.LookPath("tr")
	if err != nil {
		t.Fatalf("locate real tr binary: %v", err)
	}
	trPath := filepath.Join(binDir, "tr")
	if err := os.WriteFile(unsharePath, []byte("#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--\" ]; then\n    shift\n    break\n  fi\n  shift\ndone\nprintf 'namespace-entered\\n' >> \"$ORDER_FILE\"\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatalf("write fake unshare binary: %v", err)
	}
	if err := os.WriteFile(setprivPath, []byte("#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--\" ]; then\n    shift\n    break\n  fi\n  shift\ndone\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatalf("write fake setpriv binary: %v", err)
	}
	if err := os.WriteFile(trPath, []byte("#!/bin/sh\nprintf 'child-key-read\\n' >> \"$ORDER_FILE\"\nexec \"$REAL_TR\" \"$@\"\n"), 0700); err != nil {
		t.Fatalf("write fake tr binary: %v", err)
	}
	if err := os.Setenv("REAL_TR", realTrPath); err != nil {
		t.Fatalf("set REAL_TR: %v", err)
	}
	defer func() { _ = os.Unsetenv("REAL_TR") }()

	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", binDir+":"+oldPath); err != nil {
		t.Fatalf("set PATH for fake isolation tools: %v", err)
	}
	defer func() { _ = os.Setenv("PATH", oldPath) }()
	if err := os.Setenv("ORDER_FILE", orderFile); err != nil {
		t.Fatalf("set ORDER_FILE: %v", err)
	}
	defer func() { _ = os.Unsetenv("ORDER_FILE") }()

	prevExecCommandContext := execCommandContext
	defer func() { execCommandContext = prevExecCommandContext }()

	var capturedArgs []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string(nil), args...)
		return exec.CommandContext(ctx, unsharePath, args...)
	}

	launcher, err := NewSoloistLauncher(SoloistLaunchConfig{
		BinaryPath:     binaryPath,
		APIKeyFile:     keyFile,
		PipewireDevice: "pipewire-demo",
		IsolatedUID:    12345,
		IsolatedGID:    12345,
	})
	if err != nil {
		t.Fatalf("NewSoloistLauncher: %v", err)
	}
	launcher.isolationVerifier = func(cfg SoloistLaunchConfig, cmd *exec.Cmd) error { return nil }
	launcher.readinessProbe = func(ctx context.Context, cmd *exec.Cmd) error { return nil }
	if err := launcher.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer launcher.Stop()

	deadline := time.Now().Add(3 * time.Second)
	var orderTrace []string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(orderFile)
		if err == nil {
			orderTrace = strings.FieldsFunc(string(data), func(r rune) bool {
				return r == '\n' || r == '\r'
			})
			if err := validateSoloistLaunchOrder(orderTrace); err == nil {
				if containsMarker(orderTrace, "namespace-entered") && containsMarker(orderTrace, "child-key-read") {
					break
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !containsMarker(orderTrace, "namespace-entered") {
		t.Fatalf("child namespace boundary was never recorded: %v", orderTrace)
	}
	if !containsMarker(orderTrace, "child-key-read") {
		t.Fatalf("child-side API key read never occurred after the namespace boundary: %v", orderTrace)
	}
	if err := validateSoloistLaunchOrder(orderTrace); err != nil {
		t.Fatalf("invalid trace order: %v: %v", err, orderTrace)
	}
	if containsMarker(orderTrace, "parent-key-read") {
		t.Fatalf("parent-side API key read was observed: %v", orderTrace)
	}

	for _, arg := range capturedArgs {
		if arg == "--api-key" || arg == "super-secret-key" {
			t.Fatalf("outer unshare argv exposed API key: %v", capturedArgs)
		}
	}
	if len(capturedArgs) == 0 || capturedArgs[0] != "--pid" {
		t.Fatalf("unexpected unshare argv: %v", capturedArgs)
	}
	joined := strings.Join(capturedArgs, " ")
	if strings.Contains(joined, "super-secret-key") {
		t.Fatalf("outer unshare argv embedded the secret: %v", capturedArgs)
	}
	if strings.Contains(joined, "--api-key") {
		t.Fatalf("outer unshare argv contained inline API key flag: %v", capturedArgs)
	}
	if !strings.Contains(joined, keyFile) {
		t.Fatalf("expected key file path to flow into child after namespace creation: %v", capturedArgs)
	}
}

func TestSoloistLauncherRequiresIsolationAndReadiness(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "soloist")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatalf("write fake Soloist binary: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "soloist-api-key")
	if err := os.WriteFile(keyFile, []byte("super-secret-key\n"), 0600); err != nil {
		t.Fatalf("write Soloist API key file: %v", err)
	}

	launcher, err := NewSoloistLauncher(SoloistLaunchConfig{
		BinaryPath:     binaryPath,
		APIKeyFile:     keyFile,
		PipewireDevice: "pipewire-demo",
		IsolatedUID:    12345,
		IsolatedGID:    12345,
	})
	if err != nil {
		t.Fatalf("NewSoloistLauncher: %v", err)
	}
	launcher.processRunner = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 5")
	}
	launcher.isolationVerifier = func(cfg SoloistLaunchConfig, cmd *exec.Cmd) error {
		if cmd == nil || cmd.Process == nil {
			return errors.New("child process missing")
		}
		return errors.New("namespace verification failed")
	}
	launcher.readinessProbe = func(ctx context.Context, cmd *exec.Cmd) error {
		return nil
	}

	if err := launcher.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "namespace verification failed") {
		t.Fatalf("expected isolation failure before readiness, got err=%v", err)
	}
	launcher.mu.Lock()
	ready := launcher.ready
	started := launcher.started
	launcher.mu.Unlock()
	if ready || started {
		t.Fatalf("launcher remained ready/started after failed isolation verification: ready=%v started=%v", ready, started)
	}
}

func TestSoloistLauncherVerificationRejectsMismatchedOrUnreadableUID(t *testing.T) {
	validate := func(status string, cfg SoloistLaunchConfig) error {
		uid, err := soloistChildUIDFromStatus(status)
		if err != nil {
			return err
		}
		if uid == 0 {
			return errors.New("Soloist child is still running as root")
		}
		if cfg.IsolatedUID > 0 && uid != cfg.IsolatedUID {
			return errors.New("Soloist child UID mismatch")
		}
		return nil
	}

	t.Run("matching configured UID passes", func(t *testing.T) {
		cfg := SoloistLaunchConfig{IsolatedUID: 12345}
		if err := validate("Name:\tsoloist\nUid:\t12345\t12345\t12345\t12345\n", cfg); err != nil {
			t.Fatalf("expected matching UID to pass: %v", err)
		}
	})

	t.Run("mismatch rejects", func(t *testing.T) {
		cfg := SoloistLaunchConfig{IsolatedUID: 54321}
		if err := validate("Name:\tsoloist\nUid:\t12345\t12345\t12345\t12345\n", cfg); err == nil {
			t.Fatal("expected UID mismatch to fail")
		}
	})

	t.Run("root is rejected", func(t *testing.T) {
		cfg := SoloistLaunchConfig{IsolatedUID: 12345}
		if err := validate("Name:\tsoloist\nUid:\t0\t0\t0\t0\n", cfg); err == nil {
			t.Fatal("expected root UID to fail")
		}
	})

	t.Run("missing UID is unreadable status", func(t *testing.T) {
		cfg := SoloistLaunchConfig{IsolatedUID: 12345}
		if err := validate("Name:\tsoloist\nGroups:\t0\t0\t0\t0\n", cfg); err == nil {
			t.Fatal("expected missing UID to fail")
		}
	})

	t.Run("malformed UID is rejected", func(t *testing.T) {
		cfg := SoloistLaunchConfig{IsolatedUID: 12345}
		if err := validate("Name:\tsoloist\nUid:\tnot-a-number\tnot-a-number\tnot-a-number\tnot-a-number\n", cfg); err == nil {
			t.Fatal("expected malformed UID to fail")
		}
	})
}

func TestSoloistLauncherRequiresObservedReadinessSignal(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "soloist")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatalf("write fake Soloist binary: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "soloist-api-key")
	if err := os.WriteFile(keyFile, []byte("super-secret-key\n"), 0600); err != nil {
		t.Fatalf("write Soloist API key file: %v", err)
	}

	launcher, err := NewSoloistLauncher(SoloistLaunchConfig{
		BinaryPath:     binaryPath,
		APIKeyFile:     keyFile,
		PipewireDevice: "pipewire-demo",
		IsolatedUID:    12345,
		IsolatedGID:    12345,
	})
	if err != nil {
		t.Fatalf("NewSoloistLauncher: %v", err)
	}
	launcher.processRunner = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 5")
	}
	launcher.isolationVerifier = func(cfg SoloistLaunchConfig, cmd *exec.Cmd) error { return nil }
	launcher.readinessProbe = func(ctx context.Context, cmd *exec.Cmd) error {
		return ErrSoloistReadinessUnverifiable
	}

	if err := launcher.Start(context.Background()); err == nil || !errors.Is(err, ErrSoloistReadinessUnverifiable) {
		t.Fatalf("expected absent readiness signal error, got err=%v", err)
	}
	launcher.mu.Lock()
	ready := launcher.ready
	started := launcher.started
	launcher.mu.Unlock()
	if ready || started {
		t.Fatalf("launcher stayed ready after missing readiness signal: ready=%v started=%v", ready, started)
	}
}

func TestSoloistLauncherFailsClosedOnReadinessTimeout(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "soloist")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatalf("write fake Soloist binary: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "soloist-api-key")
	if err := os.WriteFile(keyFile, []byte("super-secret-key\n"), 0600); err != nil {
		t.Fatalf("write Soloist API key file: %v", err)
	}

	launcher, err := NewSoloistLauncher(SoloistLaunchConfig{
		BinaryPath:     binaryPath,
		APIKeyFile:     keyFile,
		PipewireDevice: "pipewire-demo",
		IsolatedUID:    12345,
		IsolatedGID:    12345,
	})
	if err != nil {
		t.Fatalf("NewSoloistLauncher: %v", err)
	}
	launcher.processRunner = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 5")
	}
	launcher.isolationVerifier = func(cfg SoloistLaunchConfig, cmd *exec.Cmd) error { return nil }
	launcher.readinessProbe = func(ctx context.Context, cmd *exec.Cmd) error {
		return context.DeadlineExceeded
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := launcher.Start(ctx); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected readiness timeout error, got err=%v", err)
	}
	launcher.mu.Lock()
	ready := launcher.ready
	started := launcher.started
	launcher.mu.Unlock()
	if ready || started {
		t.Fatalf("launcher remained ready/started after failed readiness check: ready=%v started=%v", ready, started)
	}
}

func TestSoloistLauncherSignalsReadyOnlyAfterIsolationAndSignal(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "soloist")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatalf("write fake Soloist binary: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "soloist-api-key")
	if err := os.WriteFile(keyFile, []byte("super-secret-key\n"), 0600); err != nil {
		t.Fatalf("write Soloist API key file: %v", err)
	}

	launcher, err := NewSoloistLauncher(SoloistLaunchConfig{
		BinaryPath:     binaryPath,
		APIKeyFile:     keyFile,
		PipewireDevice: "pipewire-demo",
		IsolatedUID:    12345,
		IsolatedGID:    12345,
	})
	if err != nil {
		t.Fatalf("NewSoloistLauncher: %v", err)
	}
	launcher.processRunner = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 5")
	}
	launcher.isolationVerifier = func(cfg SoloistLaunchConfig, cmd *exec.Cmd) error { return nil }
	launcher.readinessProbe = func(ctx context.Context, cmd *exec.Cmd) error { return nil }
	if err := launcher.Start(context.Background()); err != nil {
		t.Fatalf("expected successful isolation and readiness probe to succeed, got err=%v", err)
	}
	launcher.mu.Lock()
	ready := launcher.ready
	started := launcher.started
	launcher.mu.Unlock()
	if !ready || !started {
		t.Fatalf("ready should only become true after both isolation and signal succeed: ready=%v started=%v", ready, started)
	}
	launcher.Stop()
}

func TestSoloistLauncherKeepsHelperUntilChildExit(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "soloist")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatalf("write fake Soloist binary: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "soloist-api-key")
	if err := os.WriteFile(keyFile, []byte("super-secret-key\n"), 0600); err != nil {
		t.Fatalf("write Soloist API key file: %v", err)
	}

	orderDir := t.TempDir()
	orderFile := filepath.Join(orderDir, "launch-order.txt")
	binDir := filepath.Join(orderDir, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatalf("mkdir fake bin dir: %v", err)
	}
	unsharePath := filepath.Join(binDir, "unshare")
	setprivPath := filepath.Join(binDir, "setpriv")
	trPath := filepath.Join(binDir, "tr")
	realTrPath, err := exec.LookPath("tr")
	if err != nil {
		t.Fatalf("locate real tr binary: %v", err)
	}
	if err := os.WriteFile(unsharePath, []byte("#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--\" ]; then\n    shift\n    break\n  fi\n  shift\ndone\nprintf 'namespace-entered\\n' >> \"$ORDER_FILE\"\n# Stall until the helper is actually executed so Start can return while it still exists.\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatalf("write fake unshare binary: %v", err)
	}
	if err := os.WriteFile(setprivPath, []byte("#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--\" ]; then\n    shift\n    break\n  fi\n  shift\ndone\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatalf("write fake setpriv binary: %v", err)
	}
	if err := os.WriteFile(trPath, []byte("#!/bin/sh\nprintf 'child-key-read\\n' >> \"$ORDER_FILE\"\nexec \"$REAL_TR\" \"$@\"\n"), 0700); err != nil {
		t.Fatalf("write fake tr binary: %v", err)
	}
	if err := os.Setenv("REAL_TR", realTrPath); err != nil {
		t.Fatalf("set REAL_TR: %v", err)
	}
	defer func() { _ = os.Unsetenv("REAL_TR") }()
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", binDir+":"+oldPath); err != nil {
		t.Fatalf("set PATH for fake isolation tools: %v", err)
	}
	defer func() { _ = os.Setenv("PATH", oldPath) }()
	if err := os.Setenv("ORDER_FILE", orderFile); err != nil {
		t.Fatalf("set ORDER_FILE: %v", err)
	}
	defer func() { _ = os.Unsetenv("ORDER_FILE") }()

	prevExecCommandContext := execCommandContext
	defer func() { execCommandContext = prevExecCommandContext }()
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, unsharePath, args...)
	}

	launcher, err := NewSoloistLauncher(SoloistLaunchConfig{
		BinaryPath:     binaryPath,
		APIKeyFile:     keyFile,
		PipewireDevice: "pipewire-demo",
		IsolatedUID:    12345,
		IsolatedGID:    12345,
	})
	if err != nil {
		t.Fatalf("NewSoloistLauncher: %v", err)
	}
	launcher.isolationVerifier = func(cfg SoloistLaunchConfig, cmd *exec.Cmd) error { return nil }
	launcher.readinessProbe = func(ctx context.Context, cmd *exec.Cmd) error { return nil }
	if err := launcher.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	helperPath := ""
	launcher.mu.Lock()
	helperPath = launcher.helperPath
	launcher.mu.Unlock()
	if helperPath == "" {
		t.Fatal("helper script path was not retained after Start returned")
	}
	if _, err := os.Stat(helperPath); err != nil {
		t.Fatalf("helper script missing before child completion: %v", err)
	}
	launcher.Stop()
	if _, err := os.Stat(helperPath); !os.IsNotExist(err) {
		t.Fatalf("helper script was not removed after process completion; path=%q err=%v", helperPath, err)
	}
}

func TestSoloistLaunchOrderValidationRejectsForbiddenPatterns(t *testing.T) {
	tests := []struct {
		name  string
		trace []string
		want  bool
	}{
		{name: "valid", trace: []string{"namespace-entered", "child-key-read"}, want: true},
		{name: "parent-key-read-before-namespace", trace: []string{"parent-key-read", "namespace-entered", "child-key-read"}, want: false},
		{name: "reversed-order", trace: []string{"child-key-read", "namespace-entered"}, want: false},
		{name: "parent-key-read-after-namespace", trace: []string{"namespace-entered", "parent-key-read", "child-key-read"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateSoloistLaunchOrder(tt.trace); (err == nil) != tt.want {
				t.Fatalf("trace %v => err=%v, want valid=%v", tt.trace, err, tt.want)
			}
		})
	}
}

func validateSoloistLaunchOrder(trace []string) error {
	if containsMarker(trace, "parent-key-read") {
		return errors.New("parent-side secret read rejected")
	}
	if !containsMarker(trace, "namespace-entered") {
		return errors.New("namespace boundary missing")
	}
	if !containsMarker(trace, "child-key-read") {
		return errors.New("child key read missing")
	}
	if childKeyIndex(trace) <= namespaceIndex(trace) {
		return errors.New("child key read not after namespace entry")
	}
	return nil
}

func containsMarker(trace []string, marker string) bool {
	for _, item := range trace {
		if item == marker {
			return true
		}
	}
	return false
}

func namespaceIndex(trace []string) int {
	for i, item := range trace {
		if item == "namespace-entered" {
			return i
		}
	}
	return -1
}

func childKeyIndex(trace []string) int {
	for i, item := range trace {
		if item == "child-key-read" {
			return i
		}
	}
	return -1
}

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

func TestBridgeRespondsBeforeFirstPCMFrame(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}

	b := newSpotifyBridge(context.Background(), filepath.Join(t.TempDir(), "audio.pcm"))
	if b == nil {
		t.Fatal("failed to initialize bridge")
	}
	defer b.Close()
	server := httptest.NewServer(http.HandlerFunc(b.ServeHTTP))
	defer server.Close()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	res, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("live stream headers waited for PCM: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("unexpected stream response: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
}

func TestBridgeAudioDiagnosticIsBoundedAndPrivate(t *testing.T) {
	var absent *spotifyBridge
	if d := absent.diagnostic(); d.Available || d.LastEncodedAgeMs != -1 {
		t.Fatalf("unexpected absent bridge diagnostic: %+v", d)
	}

	b := &spotifyBridge{maxBuffer: 128 * 1024, subscribers: make(map[*subscriber]struct{})}
	if d := b.diagnostic(); !d.Available || d.Active || d.EncodedBytes != 0 || d.LastEncodedAgeMs != -1 {
		t.Fatalf("unexpected idle bridge diagnostic: %+v", d)
	}
	b.broadcast([]byte{1, 2, 3, 4})
	if d := b.diagnostic(); !d.Active || d.EncodedBytes != 4 || d.LastEncodedAgeMs < 0 {
		t.Fatalf("unexpected active bridge diagnostic: %+v", d)
	}
	b.mu.Lock()
	b.lastEncoded = time.Now().Add(-2 * time.Minute)
	b.mu.Unlock()
	if d := b.diagnostic(); d.Active || d.LastEncodedAgeMs != time.Minute.Milliseconds() {
		t.Fatalf("unexpected stale bridge diagnostic: %+v", d)
	}
	b.mu.Lock()
	b.encodedBytes = ^uint64(0) - 1
	b.mu.Unlock()
	b.broadcast([]byte{1, 2, 3, 4})
	if d := b.diagnostic(); d.EncodedBytes != ^uint64(0) {
		t.Fatalf("audio counter overflowed instead of saturating: %+v", d)
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
