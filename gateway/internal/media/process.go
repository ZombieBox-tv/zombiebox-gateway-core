package media

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	ConversionFailureFFmpegExit        = "ffmpeg_exit"
	ConversionFailureFFmpegStart       = "ffmpeg_start"
	ConversionFailureFFmpeg            = "ffmpeg_failure"
	ConversionFailureUpstreamHTTP      = "upstream_http"
	ConversionFailureUpstreamTimeout   = "upstream_timeout"
	ConversionFailureUpstreamTransport = "upstream_transport"
	ConversionFailureUpstreamRange     = "upstream_range"
)

// ConversionFailure carries only a bounded diagnostic class and optional
// numeric process/HTTP status details. Its Error text deliberately omits the
// underlying process error, URL, headers, response body, and command arguments.
type ConversionFailure struct {
	class      string
	exitCode   int
	httpStatus int
	stage      string
}

func (e *ConversionFailure) Error() string { return "conversion_failed" }

// ConversionFailureClass returns a safe allowlisted class, or an empty string
// when err is not a typed conversion failure. Classes are ffmpeg_exit,
// ffmpeg_start, ffmpeg_failure, upstream_http, upstream_timeout,
// upstream_transport, and upstream_range.
func ConversionFailureClass(err error) string {
	var failure *ConversionFailure
	if !errors.As(err, &failure) || failure == nil {
		return ""
	}
	return failure.class
}

// ConversionFailureExitCode returns the FFmpeg exit code when the failure
// came from a process that started and exited.
func ConversionFailureExitCode(err error) (int, bool) {
	var failure *ConversionFailure
	if !errors.As(err, &failure) || failure == nil || failure.class != ConversionFailureFFmpegExit {
		return 0, false
	}
	return failure.exitCode, true
}

// ConversionFailureHTTPStatus returns an upstream HTTP status when one was
// received and rejected by the remote media bridge.
func ConversionFailureHTTPStatus(err error) (int, bool) {
	var failure *ConversionFailure
	if !errors.As(err, &failure) || failure == nil || failure.class != ConversionFailureUpstreamHTTP {
		return 0, false
	}
	return failure.httpStatus, true
}

// ConversionFailureStage is an allowlisted FFmpeg diagnostic. Raw stderr may
// contain signed upstream URLs, so callers must never log the original text.
func ConversionFailureStage(err error) string {
	var failure *ConversionFailure
	if errors.As(err, &failure) && failure != nil {
		return failure.stage
	}
	return ""
}

func newConversionFailure(class string, exitCode, httpStatus int) *ConversionFailure {
	if exitCode < -1 || exitCode > 255 {
		exitCode = -1
	}
	if httpStatus < 100 || httpStatus > 599 {
		httpStatus = 0
	}
	return &ConversionFailure{class: class, exitCode: exitCode, httpStatus: httpStatus}
}

func conversionProcessFailure(err error) *ConversionFailure {
	stage := "unknown"
	var processError *processRunError
	if errors.As(err, &processError) {
		stage = processError.stage
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		failure := newConversionFailure(ConversionFailureFFmpegExit, exitError.ExitCode(), 0)
		failure.stage = stage
		return failure
	}
	var startError *exec.Error
	if errors.As(err, &startError) {
		return newConversionFailure(ConversionFailureFFmpegStart, 0, 0)
	}
	return newConversionFailure(ConversionFailureFFmpeg, 0, 0)
}

type processRunError struct {
	cause error
	stage string
}

func (e *processRunError) Error() string { return "media process failed" }
func (e *processRunError) Unwrap() error { return e.cause }

type boundedStderr struct{ value []byte }

func (b *boundedStderr) Write(data []byte) (int, error) {
	const limit = 64 * 1024
	if len(b.value) < limit {
		remaining := limit - len(b.value)
		b.value = append(b.value, data[:min(len(data), remaining)]...)
	}
	return len(data), nil
}

func ffmpegFailureStage(stderr []byte) string {
	value := strings.ToLower(string(stderr))
	switch {
	case strings.Contains(value, "error opening input"), strings.Contains(value, "error opening input file"):
		return "input_open"
	case strings.Contains(value, "could not write header"), strings.Contains(value, "error writing header"):
		return "output_header"
	case strings.Contains(value, "error submitting a packet to the muxer"), strings.Contains(value, "error muxing a packet"):
		return "output_packet"
	case strings.Contains(value, "stream map '1:a:0' matches no streams"):
		return "split_audio_missing"
	case strings.Contains(value, "stream map '0:") && strings.Contains(value, "matches no streams"):
		return "selected_stream_missing"
	case strings.Contains(value, "stream map"), strings.Contains(value, "no streams to mux"):
		return "stream_map"
	case strings.Contains(value, "option not found"), strings.Contains(value, "unrecognized option"):
		return "unsupported_option"
	case strings.Contains(value, "invalid argument"):
		return "invalid_argument"
	case strings.Contains(value, "error while decoding"):
		return "decode"
	case strings.Contains(value, "could not find codec parameters"):
		return "input_probe"
	case strings.Contains(value, "error during demuxing"):
		return "input_read"
	default:
		return "unknown"
	}
}

// Runner must honor cancellation and reap its child before returning. Arguments
// are generated by Tools, never shell commands or client-supplied FFmpeg options.
type Runner interface {
	Run(context.Context, string, []string, io.Writer) error
}
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, executable string, args []string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, executable, args...)
	var stderr boundedStderr
	cmd.Stdout, cmd.Stderr = output, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return &processRunError{cause: err, stage: ffmpegFailureStage(stderr.value)}
	}
	return nil
}
