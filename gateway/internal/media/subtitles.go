package media

import (
	"context"
	"errors"
	"strconv"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/subtitles"
)

func (t *Tools) Subtitles(ctx context.Context, path string, index int) ([]domain.SubtitleCue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, err := localInput(path)
	if err != nil {
		return nil, err
	}
	if index >= sidecarBase {
		for _, candidate := range sidecars(input) {
			if candidate.stream.Index == index {
				if candidate.stream.Codec == "srt" || candidate.stream.Codec == "vtt" {
					return readSidecar(candidate.path)
				}
				return t.subtitles(ctx, candidate.path, 0, false)
			}
		}
		return nil, errors.New("subtitle unavailable")
	}
	return t.subtitles(ctx, input, index, false)
}

func (t *RemoteTools) SubtitlesRemote(ctx context.Context, source domain.Source, index int) ([]domain.SubtitleCue, error) {
	if source.Live || source.AudioURL != "" {
		return nil, errors.New("remote subtitles unavailable")
	}
	if index >= domain.ExternalSubtitleBase {
		return t.attachmentSubtitles(ctx, source, index-domain.ExternalSubtitleBase)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bridge, err := t.bridge(ctx, source)
	if err != nil {
		return nil, err
	}
	defer bridge.close()
	return t.tools.subtitles(ctx, bridge.video, index, true, bridge.kind)
}

func (t *Tools) subtitles(ctx context.Context, input string, index int, remote bool, kind ...string) ([]domain.SubtitleCue, error) {
	if index < 0 {
		return nil, errors.New("invalid subtitle track")
	}
	select {
	case t.probes <- struct{}{}:
		defer func() { <-t.probes }()
	default:
		return nil, ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"-nostdin", "-v", "error", "-max_alloc", "67108864", "-threads", "1", "-protocol_whitelist", "file,pipe"}
	if remote {
		args = remoteArguments(args, kind...)
	} else {
		args = localFormats(args)
	}
	args = append(args, "-i", input, "-map", "0:"+strconv.Itoa(index), "-c:s", "srt", "-f", "srt", "pipe:1")
	output := &boundedBuffer{limit: 2 << 20}
	if err := t.runner.Run(ctx, t.ffmpeg, args, output); err != nil {
		return nil, toolError(ctx, "subtitle_extraction_failed")
	}
	return subtitles.ParseSubtitles(output.Bytes())
}
