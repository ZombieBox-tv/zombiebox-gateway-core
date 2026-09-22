// Package providers isolates remote metadata and credentials from the client protocol.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

func (a *Adapters) Jellyfin(ctx context.Context, c Config) ([]Source, error) {
	if c.URL == "" || c.Token == "" || c.UserID == "" {
		return nil, errors.New("configuration required")
	}
	base := strings.TrimRight(c.URL, "/")
	headers := http.Header{"X-Emby-Token": []string{c.Token}}
	q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Movie,Episode,Audio"}, "Limit": {"40"}, "Fields": {"Overview,MediaSources"}, "SortBy": {"DateCreated"}, "SortOrder": {"Descending"}}
	body, err := a.request(ctx, base+"/Users/"+url.PathEscape(c.UserID)+"/Items?"+q.Encode(), headers)
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []struct {
			ID           string `json:"Id"`
			Name         string
			Overview     string
			ImageTags    map[string]string
			MediaSources []jellyfinMediaSource
			Type         string
		}
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	out := []Source{}
	for _, x := range result.Items {
		path := "/Videos/"
		mime := "video/mp4"
		kind := "video"
		if x.Type == "Audio" {
			path = "/Audio/"
			mime = "audio/mpeg"
			kind = "track"
		}
		artURL := ""
		if x.ImageTags["Primary"] != "" {
			artURL = base + "/Items/" + url.PathEscape(x.ID) + "/Images/Primary?maxWidth=960&maxHeight=540"
		}
		out = append(out, Source{Subtitles: jellyfinSubtitles(base, x.ID, headers, x.MediaSources), ArtworkURL: artURL, ArtworkHeaders: headers, Item: domain.Item{ID: "jellyfin-" + x.ID, Provider: "jellyfin", Kind: kind, Title: x.Name, Description: x.Overview, Playable: true}, URL: base + path + url.PathEscape(x.ID) + "/stream?static=true", Headers: headers, MIME: mime})
	}
	return out, nil
}
