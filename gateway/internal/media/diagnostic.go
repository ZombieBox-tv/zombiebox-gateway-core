package media

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PipelineReport describes this gateway's software path, never a receiver capability.
type PipelineReport struct {
	Version           int            `json:"reportVersion"`
	Fixture           PipelineResult `json:"fixture"`
	Remux             PipelineResult `json:"remux"`
	Transcode         PipelineResult `json:"transcode"`
	Verified          bool           `json:"gatewayPipelineVerified"`
	ReceiverValidated bool           `json:"receiverValidated"`
	NetworkValidated  bool           `json:"networkValidated"`
}

type PipelineResult struct {
	State       string `json:"state"`
	VideoFrames int    `json:"videoFrames"`
	AudioFrames int    `json:"audioFrames"`
}

// DiagnosePipeline uses only a generated one-second local fixture. It does not
// open user media, configuration, URLs or state, and removes its private files.
func (t *Tools) DiagnosePipeline(parent context.Context) PipelineReport {
	report := PipelineReport{Version: 1, Fixture: PipelineResult{State: "unavailable"}, Remux: PipelineResult{State: "not_run"}, Transcode: PipelineResult{State: "not_run"}}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "zombie-media-diagnostic-")
	if err != nil {
		return report
	}
	defer os.RemoveAll(dir)
	availability := &boundedBuffer{limit: 64 << 10}
	if err := t.runner.Run(ctx, t.ffprobe, []string{"-version"}, availability); err != nil {
		report.Fixture.State = diagnosticFailure(ctx, err)
		return report
	}
	input := filepath.Join(dir, "fixture.mp4")
	fixture := &boundedBuffer{limit: 2 << 20}
	args := []string{"-nostdin", "-v", "error", "-max_alloc", "67108864",
		"-f", "lavfi", "-i", "color=c=green:s=160x90:r=10",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=44100", "-t", "1",
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "libx264", "-threads", "1",
		"-profile:v", "baseline", "-pix_fmt", "yuv420p", "-c:a", "aac", "-ac", "1",
		"-movflags", "+frag_keyframe+empty_moov", "-f", "mp4", "pipe:1"}
	err = t.diagnosticRun(ctx, args, fixture)
	if err == nil {
		err = os.WriteFile(input, fixture.Bytes(), 0600)
	}
	if err != nil {
		report.Fixture.State = diagnosticFailure(ctx, err)
		return report
	}
	report.Fixture = t.diagnosticDecode(ctx, input)
	if report.Fixture.State != "pass" {
		return report
	}
	for _, job := range []struct {
		mode   string
		result *PipelineResult
	}{{"REMUX", &report.Remux}, {"TRANSCODE", &report.Transcode}} {
		output := &boundedBuffer{limit: 4 << 20}
		err := t.Convert(ctx, input, job.mode, output)
		path := filepath.Join(dir, strings.ToLower(job.mode)+".mp4")
		if err == nil {
			err = os.WriteFile(path, output.Bytes(), 0600)
		}
		if err != nil {
			job.result.State = diagnosticFailure(ctx, err)
			continue
		}
		*job.result = t.diagnosticDecode(ctx, path)
	}
	report.Verified = report.Remux.State == "pass" && report.Transcode.State == "pass"
	return report
}

func diagnosticFailure(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "cancelled"
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrBusy) {
		return "unavailable"
	}
	return "failed"
}

func (t *Tools) diagnosticRun(ctx context.Context, args []string, output *boundedBuffer) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case t.jobs <- struct{}{}:
		defer func() { <-t.jobs }()
	default:
		return ErrBusy
	}
	return t.runner.Run(ctx, t.ffmpeg, args, output)
}

func (t *Tools) diagnosticDecode(ctx context.Context, path string) PipelineResult {
	result := PipelineResult{State: "failed"}
	metadata, err := t.Probe(ctx, path)
	if err != nil {
		result.State = diagnosticFailure(ctx, err)
		return result
	}
	video, audio := false, false
	for _, stream := range metadata.Streams {
		video = video || (stream.Type == "video" && stream.Codec == "h264" && stream.Width > 0 && stream.Width <= 640 && stream.Height > 0 && stream.Height <= 360)
		audio = audio || (stream.Type == "audio" && stream.Codec == "aac")
	}
	if !video || !audio {
		return result
	}
	output := &boundedBuffer{limit: 64 << 10}
	args := []string{"-nostdin", "-v", "error", "-max_alloc", "67108864", "-threads", "1",
		"-protocol_whitelist", "file,pipe", "-format_whitelist", "mov", "-i", path,
		"-t", "2", "-map", "0:v:0", "-map", "0:a:0", "-threads", "1", "-f", "framehash", "pipe:1"}
	if err := t.diagnosticRun(ctx, args, output); err != nil {
		result.State = diagnosticFailure(ctx, err)
		return result
	}
	result.VideoFrames, result.AudioFrames, err = decodedFrames(string(output.Bytes()))
	if err == nil {
		result.State = "pass"
	}
	return result
}

func decodedFrames(output string) (int, int, error) {
	var frames [2]int
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 6 {
			return 0, 0, errors.New("invalid frame evidence")
		}
		index, indexErr := strconv.Atoi(strings.TrimSpace(fields[0]))
		size, sizeErr := strconv.Atoi(strings.TrimSpace(fields[4]))
		if indexErr != nil || sizeErr != nil || index < 0 || index > 1 || size <= 0 {
			return 0, 0, errors.New("invalid frame evidence")
		}
		frames[index]++
	}
	if frames[0] < 5 || frames[1] < 5 {
		return 0, 0, errors.New("insufficient decoded audio/video")
	}
	return frames[0], frames[1], nil
}
