package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

var youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

func (a *Adapters) YouTube(ctx context.Context, c Config) ([]Source, error) {
	if c.URL == "" || len(c.Token) < 32 {
		return nil, errors.New("wrapper configuration required")
	}
	headers := http.Header{"Authorization": {"Bearer " + c.Token}}
	body, err := a.request(ctx, strings.TrimRight(c.URL, "/")+"/catalog?q="+url.QueryEscape(c.CatalogID), headers)
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []struct {
			ID, Title, Subtitle string
			DurationMS          int64 `json:"durationMs"`
		}
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return nil, errors.New("invalid wrapper catalog")
	}
	out := []Source{}
	seen := map[string]bool{}
	for _, item := range result.Items {
		if !youtubeID.MatchString(item.ID) || seen[item.ID] || len(item.Title) > 2000 || item.DurationMS < 0 || item.DurationMS > 604800000 {
			continue
		}
		seen[item.ID] = true
		out = append(out, Source{ArtworkURL: "https://i.ytimg.com/vi/" + item.ID + "/hqdefault.jpg", Item: domain.Item{ID: "youtube-" + item.ID, Provider: "youtube", Kind: "video", Title: item.Title, Subtitle: item.Subtitle, DurationMS: item.DurationMS, Playable: true},
			URL: strings.TrimRight(c.URL, "/") + "/resolve/" + item.ID, Headers: headers, MIME: "application/x-zombie-youtube"})
		if len(out) == 40 {
			break
		}
	}
	return out, nil
}

func (a *Adapters) resolveYouTube(ctx context.Context, source Source) (Source, error) {
	body, err := a.request(ctx, source.URL, source.Headers)
	if err != nil {
		return source, err
	}
	var result struct {
		URL      string `json:"url"`
		AudioURL string `json:"audioUrl"`
		MIME     string `json:"mimeType"`
	}
	if json.Unmarshal(body, &result) != nil {
		return source, errors.New("invalid wrapper stream")
	}

	if !youtubeStreamURL(result.URL) || (result.AudioURL != "" && !youtubeStreamURL(result.AudioURL)) || result.MIME != "video/mp4" {
		return source, errors.New("invalid YouTube stream origin")
	}
	source.URL, source.MIME, source.Headers = result.URL, result.MIME, nil
	source.AudioURL, source.AudioHeaders = result.AudioURL, nil

	return source, nil
}

func youtubeStreamURL(raw string) bool {
	target, err := url.Parse(raw)
	return err == nil && len(raw) <= 16384 && target.Scheme == "https" && target.User == nil && target.Port() == "" && strings.HasSuffix(strings.ToLower(target.Hostname()), ".googlevideo.com")
}
