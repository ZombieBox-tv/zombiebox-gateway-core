package media

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestFFmpegFailureStageDoesNotExposeDiagnosticText(t *testing.T) {
	stderr := []byte("https://media.example/stream?token=private\nCould not write header for output file #0 (incorrect codec parameters ?): Invalid argument\n")
	if got := ffmpegFailureStage(stderr); got != "output_header" {
		t.Fatalf("stage = %q", got)
	}
	if strings.Contains(ffmpegFailureStage(stderr), "private") {
		t.Fatal("classified stage exposed upstream URL")
	}
	var buffer boundedStderr
	data := []byte(strings.Repeat("x", 70*1024))
	if n, err := buffer.Write(data); err != nil || n != len(data) || len(buffer.value) != 64*1024 {
		t.Fatalf("bounded stderr size=%d accepted=%d err=%v", len(buffer.value), n, err)
	}
}

func TestExecRunnerClassifiesWithoutReturningStderr(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("shell unavailable")
	}
	err := (ExecRunner{}).Run(context.Background(), "sh", []string{"-c", "echo 'Could not write header token=private' >&2; exit 234"}, io.Discard)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe process error: %v", err)
	}
	failure := conversionProcessFailure(err)
	if failure.class != ConversionFailureFFmpegExit || failure.exitCode != 234 || failure.stage != "output_header" {
		t.Fatalf("failure = %#v", failure)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal("exit status was lost")
	}
}
