// Package media supervises optional FFmpeg tools without shell command construction.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

var ErrBusy = errors.New("media worker busy")

type Stream = domain.Stream
type Metadata = domain.Metadata
type Tools struct {
	ffmpeg, ffprobe string
	probes, jobs    chan struct{}
	runner          Runner
}

func New(ffmpeg, ffprobe string) *Tools {
	return NewWithRunner(ffmpeg, ffprobe, ExecRunner{})
}

func NewWithRunner(ffmpeg, ffprobe string, runner Runner) *Tools {
	if runner == nil {
		panic("media: runner is required")
	}
	return &Tools{ffmpeg, ffprobe, make(chan struct{}, 2), make(chan struct{}, 1), runner}
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
	input, err := localInput(path)
	if err != nil {
		return Metadata{}, err
	}
	metadata, err := t.probe(ctx, input, false)
	if err != nil {
		return metadata, err
	}
	for _, candidate := range sidecars(input) {
		metadata.Streams = append(metadata.Streams, candidate.stream)
	}
	return metadata, nil
}

func (t *Tools) probe(ctx context.Context, input string, remote bool, manifestKind ...string) (Metadata, error) {
	var result Metadata
	select {
	case t.probes <- struct{}{}:
		defer func() { <-t.probes }()
	default:
		return result, ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := []string{"-v", "error", "-max_alloc", "67108864", "-protocol_whitelist", "file,pipe", "-probesize", "8388608", "-analyzeduration", "5000000", "-show_entries", "stream=index,codec_type,codec_name,profile,level,width,height,pix_fmt,r_frame_rate,avg_frame_rate,codec_tag_string,color_transfer,color_primaries:stream_tags=language,title:stream_disposition=default,forced:format=format_name,duration,bit_rate", "-of", "json"}
	if remote {
		args = remoteArguments(args, manifestKind...)
	} else {
		args = localFormats(args)
	}
	args = append(args, input)
	output := &boundedBuffer{limit: 1 << 20}
	if err := t.runner.Run(ctx, t.ffprobe, args, output); err != nil {
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
	return t.ConvertSelected(ctx, path, mode, domain.MediaSelection{}, output)
}

func (t *Tools) ConvertSelected(ctx context.Context, path, mode string, selection domain.MediaSelection, output io.Writer) error {
	input, err := localInput(path)
	if err != nil {
		return err
	}
	return t.convert(ctx, input, "", false, false, mode, selection, output)
}

func (t *Tools) convert(ctx context.Context, input, audioInput string, remote, adtsAAC bool, mode string, selection domain.MediaSelection, output io.Writer, manifestKind ...string) error {
	if !ValidQuality(selection.Quality) {
		return errors.New("invalid media quality")
	}
	if selection.PositionMS < 0 || selection.PositionMS > 7*24*60*60*1000 || (selection.AudioID != nil && *selection.AudioID < 0) {
		return errors.New("invalid media selection")
	}
	// A replacement stream may arrive while cancellation reaps the previous
	// FFmpeg process. Give it a bounded grace period without adding capacity.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case t.jobs <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrBusy
	}
	defer func() { <-t.jobs }()
	if mode != "REMUX" && mode != "TRANSCODE" {
		return errors.New("unsupported media mode")
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-max_alloc", "67108864", "-threads", "2", "-protocol_whitelist", "file,pipe"}
	if remote {
		args = remoteArguments(args, manifestKind...)
	} else {
		args = localFormats(args)
	}
	if mode == "REMUX" && selection.PositionMS > 0 {
		return errors.New("accurate resume requires transcoding")
	}
	if selection.PositionMS > 0 {
		args = append(args, "-ss", strconv.FormatFloat(float64(selection.PositionMS)/1000, 'f', 3, 64))
	}
	args = append(args, "-i", input)
	audio := "0:a:0?"
	if audioInput != "" {
		second := remoteArguments([]string{})
		if selection.PositionMS > 0 {
			second = append(second, "-ss", strconv.FormatFloat(float64(selection.PositionMS)/1000, 'f', 3, 64))
		}
		args = append(args, second...)
		args = append(args, "-i", audioInput)
		audio = "1:a:0"
	}
	if selection.AudioID != nil {
		audio = "0:" + strconv.Itoa(*selection.AudioID)
	}
	args = append(args, "-map", "0:v:0?", "-map", audio, "-sn", "-dn", "-map_metadata", "-1")
	if mode == "REMUX" {
		args = append(args, "-c", "copy")
		if adtsAAC {
			args = append(args, "-bsf:a", "aac_adtstoasc")
		}
	} else {
		args = append(args, videoEncoding(selection.Quality)...)
	}
	args = append(args, "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	if err := t.runner.Run(ctx, t.ffmpeg, args, output); err != nil {
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

// Restrict remote demuxers as well as protocols. Playlists, concat and nested
// network references require the rewritten manifest graph; opaque input keeps finite routes.
func remoteArguments(args []string, kind ...string) []string {
	formats := "mov,matroska,webm,mpegts,mp3,aac,flac,ogg,wav"
	if len(kind) > 0 && kind[0] != "" {
		formats += ",hls,dash"
		if kind[0] == "hls" {
			args = append(args, "-allowed_extensions", "ALL", "-extension_picky", "0")
		}
	}

	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-protocol_whitelist" {
			args[i+1] = "http,tcp,pipe"
			return append(args, "-format_whitelist", formats, "-http_proxy", "")
		}
	}
	return append(args, "-protocol_whitelist", "http,tcp,pipe", "-format_whitelist", formats, "-http_proxy", "")
}

// Local media is a self-contained container. Playlists/concat files must never
// turn an uploaded file into requests for other gateway paths or remote URLs.
// Manifest adaptation has its own bounded graph in RemoteTools.
func localFormats(args []string) []string {
	return append(args, "-format_whitelist", "mov,matroska,webm,mp3,wav,flac,ogg,avi,mpeg,mpegts,aac,asf,flv,srt,webvtt,ass")
}
