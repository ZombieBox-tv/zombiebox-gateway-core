package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

func (a *Adapters) browseJellyfin(ctx context.Context, c Config, parent, query string, offset int) (domain.BrowseResult, error) {
	if c.URL == "" || c.Token == "" || c.UserID == "" {
		return domain.BrowseResult{}, errors.New("configuration required")
	}
	base := strings.TrimRight(c.URL, "/")
	path := "/Users/" + url.PathEscape(c.UserID) + "/Views"
	q := url.Values{"StartIndex": {strconv.Itoa(offset)}, "Limit": {"40"}, "Fields": {"Overview"}}
	if parent != "" || query != "" {
		path = "/Users/" + url.PathEscape(c.UserID) + "/Items"
		q.Set("ParentId", parent)
		q.Set("Recursive", strconv.FormatBool(query != ""))
		q.Set("SortBy", "SortName")
		if query != "" {
			q.Set("SearchTerm", query)
		}
	}
	if parent == "" && query == "" {
		q = url.Values{}
	}
	headers := http.Header{"X-Emby-Token": {c.Token}}
	body, err := a.request(ctx, base+path+"?"+q.Encode(), headers)
	if err != nil {
		return domain.BrowseResult{}, err
	}
	var data struct {
		Total int `json:"TotalRecordCount"`
		Items []struct {
			ID                   string `json:"Id"`
			Name, Type, Overview string
			IsFolder             bool
			RunTimeTicks         int64
			ImageTags            map[string]string
		}
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return domain.BrowseResult{}, err
	}
	sources := []Source{}
	for _, item := range data.Items {
		source := Source{
			Headers:        headers,
			ArtworkHeaders: headers,
			Item: domain.Item{
				ID:          "jellyfin-" + item.ID,
				Provider:    "jellyfin",
				Kind:        strings.ToLower(item.Type),
				Title:       item.Name,
				Description: item.Overview,
				DurationMS:  item.RunTimeTicks / 10000,
			},
		}
		if item.ImageTags["Primary"] != "" {
			source.ArtworkURL = base + "/Items/" + url.PathEscape(item.ID) + "/Images/Primary?maxWidth=960&maxHeight=540"
		}
		if item.IsFolder || item.Type == "CollectionFolder" || item.Type == "Series" || item.Type == "Season" || item.Type == "BoxSet" {
			source.BrowsePath = item.ID
		} else if item.Type == "Movie" || item.Type == "Episode" || item.Type == "Video" || item.Type == "Audio" {
			source.Item.Playable = true
			path, mime := "/Videos/", "video/mp4"
			if item.Type == "Audio" {
				path, mime = "/Audio/", "audio/mpeg"
			}
			source.URL = base + path + url.PathEscape(item.ID) + "/stream?static=true"
			source.MIME = mime
		} else {
			continue
		}
		sources = append(sources, source)
	}
	// Views is not paginated by every server; page its bounded metadata locally.
	if parent == "" && query == "" {
		return sourcePage(sources, offset), nil
	}
	result := domain.BrowseResult{Sources: sources, NextOffset: -1}
	if len(result.Sources) > 40 {
		result.Sources = result.Sources[:40]
	}
	if len(data.Items) > 0 && offset+len(data.Items) < data.Total {
		result.NextOffset = offset + len(data.Items)
	}
	return result, nil
}
