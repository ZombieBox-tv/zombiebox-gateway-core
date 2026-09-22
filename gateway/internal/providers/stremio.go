// Package providers isolates remote metadata and credentials from the client protocol.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

func (a *Adapters) Stremio(ctx context.Context, c Config) ([]Source, error) {
	if c.URL == "" || c.CatalogID == "" {
		return nil, errors.New("configuration required")
	}
	base := strings.TrimSuffix(strings.TrimRight(c.URL, "/"), "/manifest.json")
	kind := c.MediaType
	if kind == "" {
		kind = "movie"
	}
	body, err := a.request(ctx, base+"/catalog/"+url.PathEscape(kind)+"/"+url.PathEscape(c.CatalogID)+".json", nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Metas []struct {
			ID          string
			Name        string
			Description string
			Poster      string
		}
	}
	if err = json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	out := []Source{}
	for _, m := range data.Metas {
		if len(out) >= 40 {
			break
		}
		out = append(out, Source{ArtworkURL: m.Poster, Item: domain.Item{ID: "stremio-" + m.ID, Provider: "stremio", Kind: kind, Title: m.Name, Description: m.Description, Playable: true}, URL: base + "/stream/" + url.PathEscape(kind) + "/" + url.PathEscape(m.ID) + ".json", MIME: "application/x-zombie-stremio"})
	}
	return out, nil
}
func (a *Adapters) Resolve(ctx context.Context, source Source) (Source, error) {
	if source.MIME == "application/x-zombie-plex" {
		return a.resolvePlex(ctx, source)
	}
	if source.MIME == "application/x-zombie-youtube" {
		return a.resolveYouTube(ctx, source)
	}
	if source.MIME != "application/x-zombie-stremio" {
		return source, nil
	}
	body, err := a.request(ctx, source.URL, nil)
	if err != nil {
		return source, err
	}
	var data struct {
		Streams []struct {
			URL       string
			Subtitles []stremioSubtitle
		}
	}
	if err = json.Unmarshal(body, &data); err != nil {
		return source, err
	}
	for _, stream := range data.Streams {
		u, e := url.Parse(stream.URL)
		if e == nil && (u.Scheme == "http" || u.Scheme == "https") {
			source.Subtitles = stremioSubtitles(stream.Subtitles)
			source.URL = stream.URL
			source.MIME = "video/mp4"
			return source, nil
		}
	}
	return source, errors.New("no HTTP stream; torrents are unsupported")
}
