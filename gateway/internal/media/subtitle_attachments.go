package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/subtitles"
)

func (t *RemoteTools) attachmentSubtitles(ctx context.Context, source domain.Source, index int) ([]domain.SubtitleCue, error) {
	if index < 0 || index >= len(source.Subtitles) || index >= 32 {
		return nil, errors.New("subtitle unavailable")
	}
	attachment := source.Subtitles[index]
	u, err := url.Parse(attachment.URL)
	codec := strings.ToLower(attachment.Codec)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || !subtitles.TextFormat(codec) {
		return nil, errors.New("subtitle unavailable")
	}
	// Reuse the media job bound for download as well as decoding.
	select {
	case t.tools.probes <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	data, err := func() ([]byte, error) {
		defer func() { <-t.tools.probes }()
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, errors.New("subtitle unavailable")
		}
		req.Header = attachment.Headers.Clone()
		response, err := t.http.Do(req)
		if err != nil {
			return nil, errors.New("subtitle unavailable")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK || response.ContentLength > 2<<20 {
			return nil, errors.New("subtitle unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
		if err != nil || len(data) > 2<<20 {
			return nil, errors.New("subtitle unavailable")
		}
		return data, nil
	}()
	if err != nil {
		return nil, err
	}
	if codec != "ass" && codec != "ssa" {
		return subtitles.ParseSubtitles(data)
	}
	// Decode downloaded, bounded bytes only; FFmpeg never sees a provider URL/token.
	directory, err := os.MkdirTemp("", "zombie-subtitle-")
	if err != nil {
		return nil, errors.New("subtitle unavailable")
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "subtitle.ass")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return nil, errors.New("subtitle unavailable")
	}
	return t.tools.subtitles(ctx, path, 0, false)
}
