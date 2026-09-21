// Package providers isolates remote metadata and credentials from the client protocol.
package providers

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

func (a *Adapters) Plex(ctx context.Context, c Config) ([]Source, error) {
	if c.URL == "" || c.Token == "" {
		return nil, errors.New("configuration required")
	}
	base := strings.TrimRight(c.URL, "/")
	headers := http.Header{"X-Plex-Token": []string{c.Token}, "X-Plex-Client-Identifier": []string{"zombie-box-tv"}}
	body, err := a.request(ctx, base+"/library/recentlyAdded?X-Plex-Container-Size=40", headers)
	if err != nil {
		return nil, err
	}
	var data struct {
		Videos []struct {
			Key     string `xml:"ratingKey,attr"`
			Title   string `xml:"title,attr"`
			Summary string `xml:"summary,attr"`
			Thumb   string `xml:"thumb,attr"`
			Media   []struct {
				Parts []struct {
					Key string `xml:"key,attr"`
				} `xml:"Part"`
			} `xml:"Media"`
		} `xml:"Video"`
	}
	if err = xml.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	out := []Source{}
	for _, v := range data.Videos {
		if len(v.Media) == 0 || len(v.Media[0].Parts) == 0 {
			continue
		}
		key := v.Media[0].Parts[0].Key
		if !strings.HasPrefix(key, "/") || strings.HasPrefix(key, "//") {
			continue
		}
		artURL := ""
		if strings.HasPrefix(v.Thumb, "/") && !strings.HasPrefix(v.Thumb, "//") {
			artURL = base + v.Thumb
		}
		out = append(out, Source{ArtworkURL: artURL, ArtworkHeaders: headers, Item: domain.Item{ID: "plex-" + v.Key, Provider: "plex", Kind: "video", Title: v.Title, Description: v.Summary, Playable: true}, URL: base + key, Headers: headers, MIME: "video/mp4"})
	}
	return out, nil
}
