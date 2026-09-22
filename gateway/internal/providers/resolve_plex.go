package providers

import (
	"context"
	"encoding/xml"
	"errors"
	"net/url"
	"strings"
)

// Hub previews may omit playable parts. Resolve those lazily on the gateway.
func (a *Adapters) resolvePlex(ctx context.Context, source Source) (Source, error) {
	body, err := a.request(ctx, source.URL, source.Headers)
	if err != nil {
		return Source{}, err
	}
	var data struct {
		Videos []plexNode `xml:"Video"`
		Tracks []plexNode `xml:"Track"`
	}
	if xml.Unmarshal(body, &data) != nil {
		return Source{}, errors.New("invalid plex metadata")
	}
	base, err := url.Parse(source.URL)
	if err != nil {
		return Source{}, errors.New("invalid plex source")
	}
	for _, node := range append(data.Videos, data.Tracks...) {
		if "plex-"+node.RatingKey != source.Item.ID || len(node.Media) == 0 || len(node.Media[0].Parts) == 0 {
			continue
		}
		path := node.Media[0].Parts[0].Key
		if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
			continue
		}
		part, err := url.Parse(path)
		if err != nil || part.IsAbs() || part.Host != "" || part.Fragment != "" {
			continue
		}
		source.Subtitles = plexSubtitles(source.URL, source.Headers, node.Media[0].Parts[0].Streams)
		source.URL = base.ResolveReference(part).String()
		source.MIME = "video/mp4"
		if node.Type == "track" {
			source.MIME = "audio/mpeg"
		}
		return source, nil
	}
	return Source{}, errors.New("plex media unavailable")
}
