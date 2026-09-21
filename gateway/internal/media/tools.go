// Package media supervises optional FFmpeg tools without shell command construction.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var ErrBusy = errors.New("media worker busy")

type Stream struct {
	Type    string `json:"codec_type"`
	Codec   string `json:"codec_name"`
	Profile string `json:"profile"`
	Level   int    `json:"level"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}
type Metadata struct {
	Streams []Stream `json:"streams"`
	Format  struct {
		Name     string `json:"format_name"`
		Duration string `json:"duration"`
	} `json:"format"`
}
type Tools struct {
	ffmpeg, ffprobe string
	probes, jobs    chan struct{}
}

func New(ffmpeg, ffprobe string) *Tools {
	return &Tools{ffmpeg, ffprobe, make(chan struct{}, 2), make(chan struct{}, 1)}
}

func localInput(path string) (string, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("invalid media path")
	}
	file, err := os.Stat(resolved)
	if err != nil || !file.Mode().IsRegular() {
		return "", errors.New("regular local media file required")
	}
	return resolved, nil
}
func (t *Tools) Probe(ctx context.Context, path string) (Metadata, error) {
	var result Metadata
	select {
	case t.probes <- struct{}{}:
		defer func() { <-t.probes }()
	default:
		return result, ErrBusy
	}
	input, err := localInput(path)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.ffprobe, "-v", "error", "-max_alloc", "67108864", "-protocol_whitelist", "file,pipe", "-probesize", "8388608", "-analyzeduration", "5000000", "-show_entries", "stream=codec_type,codec_name,profile,level,width,height:format=format_name,duration", "-of", "json", input)
	output := &boundedBuffer{limit: 1 << 20}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if err = cmd.Run(); err != nil {
		return result, toolError(ctx, "probe_failed")
	}
	if json.Unmarshal(output.Bytes(), &result) != nil {
		return result, errors.New("invalid probe response")
	}
	return result, nil
}

// Convert writes fragmented MP4 progressively. Remote URLs/credentials and arbitrary options are forbidden.
// Caller owns the output lifetime; cancellation stops and reaps the process before capacity is released.
func (t *Tools) Convert(ctx context.Context, path, mode string, output io.Writer) error {
	select {
	case t.jobs <- struct{}{}:
		defer func() { <-t.jobs }()
	default:
		return ErrBusy
	}
	if mode != "REMUX" && mode != "TRANSCODE" {
		return errors.New("unsupported media mode")
	}
	input, err := localInput(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-max_alloc", "67108864", "-threads", "2", "-protocol_whitelist", "file,pipe", "-i", input, "-map", "0:v:0?", "-map", "0:a:0?", "-sn", "-dn", "-map_metadata", "-1"}
	if mode == "REMUX" {
		args = append(args, "-c", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-threads", "2", "-filter_threads", "1", "-preset", "veryfast", "-profile:v", "baseline", "-level:v", "3.0", "-pix_fmt", "yuv420p", "-vf", "scale=640:360:force_original_aspect_ratio=decrease:force_divisible_by=2", "-r", "30", "-b:v", "1000k", "-maxrate", "1200k", "-bufsize", "2400k", "-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "44100")
	}
	args = append(args, "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	cmd := exec.CommandContext(ctx, t.ffmpeg, args...)
	cmd.Stdout, cmd.Stderr = output, io.Discard
	cmd.WaitDelay = time.Second
	if err = cmd.Run(); err != nil {
		return toolError(ctx, "conversion_failed")
	}
	return nil
}
func toolError(ctx context.Context, message string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New(message)
}

type boundedBuffer struct {
	data  bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if b.data.Len()+len(data) > b.limit {
		return 0, errors.New("tool output too large")
	}
	return b.data.Write(data)
}

func (b *boundedBuffer) Bytes() []byte { return b.data.Bytes() }
