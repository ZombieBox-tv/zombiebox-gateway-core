package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

var youtubeParent = regexp.MustCompile(`^(channel:UC[A-Za-z0-9_-]{22}|playlist:[A-Za-z0-9_-]{10,100})$`)

func (a *Adapters) browseYouTube(ctx context.Context, config Config, parent, query string, offset int) (domain.BrowseResult, error) {
	result := domain.BrowseResult{Sources: []Source{}, NextOffset: -1}
	if (parent != "" && !youtubeParent.MatchString(parent)) || offset > 360 {
		return result, errors.New("invalid YouTube browse request")
	}
	headers, err := wrapperHeaders(config)
	if err != nil {
		return result, err
	}
	if query == "" && parent == "" {
		query = config.CatalogID
		if query == "" {
			query = "popular"
		}
	}
	values := url.Values{"parent": {parent}, "q": {query}, "offset": {strconv.Itoa(offset)}}
	data, err := a.request(ctx, strings.TrimRight(config.URL, "/")+"/browse?"+values.Encode(), headers)
	if err != nil {
		return result, err
	}
	var response struct {
		Items []struct {
			ID, Kind, Title, Subtitle string
			DurationMS                int64 `json:"durationMs"`
		}
		NextOffset int `json:"nextOffset"`
	}
	if json.Unmarshal(data, &response) != nil {
		return result, errors.New("invalid browse response")
	}
	for _, item := range response.Items {
		if len(result.Sources) >= 40 {
			break
		}
		source := Source{Item: domain.Item{ID: "youtube-" + item.ID, Provider: "youtube", Kind: item.Kind, Title: truncate(item.Title, 500), Subtitle: truncate(item.Subtitle, 500)}}
		if item.Kind == "channel" || item.Kind == "playlist" {
			source.BrowsePath = item.Kind + ":" + item.ID
			if !youtubeParent.MatchString(source.BrowsePath) {
				continue
			}
		} else if item.Kind == "video" && youtubeID.MatchString(item.ID) {
			source.Item.Playable = true
			source.Item.DurationMS = max(0, min(604800000, item.DurationMS))
			source.URL = strings.TrimRight(config.URL, "/") + "/resolve/" + item.ID
			source.Headers = headers
			source.MIME = "application/x-zombie-youtube"
			source.ArtworkURL = "https://i.ytimg.com/vi/" + item.ID + "/hqdefault.jpg"
		} else {
			continue
		}
		result.Sources = append(result.Sources, source)
	}
	if response.NextOffset > offset && response.NextOffset <= 360 {
		result.NextOffset = response.NextOffset
	}
	return result, nil
}
