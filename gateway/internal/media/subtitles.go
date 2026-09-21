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
	if index < 0 {
		return nil, errors.New("invalid subtitle track")
	}
	select {
	case t.probes <- struct{}{}:
		defer func() { <-t.probes }()
	default:
		return nil, ErrBusy
	}
	input, err := localInput(path)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"-nostdin", "-v", "error", "-max_alloc", "67108864", "-threads", "1", "-protocol_whitelist", "file,pipe", "-i", input, "-map", "0:" + strconv.Itoa(index), "-c:s", "srt", "-f", "srt", "pipe:1"}
	output := &boundedBuffer{limit: 2 << 20}
	if err := t.runner.Run(ctx, t.ffmpeg, args, output); err != nil {
		return nil, toolError(ctx, "subtitle_extraction_failed")
	}
	return subtitles.ParseSubtitles(output.Bytes())
}
