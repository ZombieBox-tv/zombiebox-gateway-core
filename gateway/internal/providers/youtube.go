package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

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
			ID, Title, Subtitle, Description string
			DurationMS                       int64 `json:"durationMs"`
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
		out = append(out, Source{ArtworkURL: "https://i.ytimg.com/vi/" + item.ID + "/hqdefault.jpg", Item: domain.Item{ID: "youtube-" + item.ID, Provider: "youtube", Kind: "video", Title: item.Title, Subtitle: item.Subtitle, Description: truncate(item.Description, 2000), DurationMS: item.DurationMS, Playable: true},
			URL: strings.TrimRight(c.URL, "/") + "/resolve/" + item.ID, Headers: headers, MIME: "application/x-zombie-youtube"})
		if len(out) == 40 {
			break
		}
	}
	return out, nil
}

func (a *Adapters) resolveYouTube(ctx context.Context, source Source) (Source, error) {
	// The bounded worker may need more than the metadata client's five seconds
	// to resolve both H.264 video and AAC audio. The private client retains the
	// wrapper's longer timeout and redirect restrictions.
	resolveURL := source.URL
	resolveHeaders := source.Headers.Clone()
	if source.ResolveURL != "" {
		resolveURL = source.ResolveURL
		resolveHeaders = source.ResolveHeaders.Clone()
	}
	if source.ResolveQuality != "" && source.ResolveQuality != "auto" {
		separator := "?"
		if strings.Contains(resolveURL, "?") {
			separator = "&"
		}
		resolveURL += separator + "quality=" + url.QueryEscape(source.ResolveQuality)
	}

	var body []byte
	var err error
	// The single-flight worker can report busy briefly while a completed worker
	// releases its resources. Retry that one transient status within the caller's
	// deadline; provider failures and invalid streams remain terminal.
	for attempt := 0; attempt < 6; attempt++ {
		body, err = requestWithClient(ctx, a.privateHTTP, resolveURL, resolveHeaders)
		if !errors.Is(err, errProviderBusy) || attempt == 5 {
			break
		}
		select {
		case <-ctx.Done():
			return source, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	if err != nil {
		return source, err
	}
	var result struct {
		URL      string          `json:"url"`
		AudioURL string          `json:"audioUrl"`
		MIME     string          `json:"mimeType"`
		Variants json.RawMessage `json:"variants"`
	}
	if json.Unmarshal(body, &result) != nil {
		return source, errors.New("invalid wrapper stream")
	}

	if !youtubeStreamURL(result.URL) || (result.AudioURL != "" && !youtubeStreamURL(result.AudioURL)) || result.MIME != "video/mp4" {
		return source, errors.New("invalid YouTube stream origin")
	}
	if source.ResolveURL == "" {
		source.ResolveURL = source.URL
		source.ResolveHeaders = source.Headers.Clone()
	}
	source.URL, source.MIME, source.Headers = result.URL, result.MIME, nil
	source.AudioURL, source.AudioHeaders = result.AudioURL, nil
	if variants := parseVariants(result.Variants); len(variants) > 0 {
		if len(source.Variants) == 0 {
			source.Variants = variants
		} else {
			seen := make(map[string]bool)
			merged := make([]string, 0, len(variants)+len(source.Variants))
			for _, v := range variants {
				if !seen[v] {
					seen[v] = true
					merged = append(merged, v)
				}
			}
			for _, v := range source.Variants {
				if !seen[v] {
					seen[v] = true
					merged = append(merged, v)
				}
			}
			source.Variants = boundVariants(merged)
		}
	}

	return source, nil
}

var allowedVariantTiers = map[string]bool{
	"2160p": true,
	"1440p": true,
	"1080p": true,
	"720p":  true,
	"480p":  true,
	"360p":  true,
	"240p":  true,
	"144p":  true,
}

func parseVariants(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var stringsList []string
	if err := json.Unmarshal(raw, &stringsList); err == nil {
		return boundVariants(stringsList)
	}
	var objects []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &objects); err == nil {
		out := make([]string, 0, len(objects))
		for _, obj := range objects {
			if obj.ID != "" {
				out = append(out, obj.ID)
			}
		}
		return boundVariants(out)
	}
	return nil
}

func boundVariants(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, id := range items {
		id = strings.TrimSpace(id)
		if allowedVariantTiers[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
			if len(out) >= 8 {
				break
			}
		}
	}
	return out
}

func youtubeStreamURL(raw string) bool {
	target, err := url.Parse(raw)
	return err == nil && len(raw) <= 16384 && target.Scheme == "https" && target.User == nil && target.Port() == "" && strings.HasSuffix(strings.ToLower(target.Hostname()), ".googlevideo.com")
}
