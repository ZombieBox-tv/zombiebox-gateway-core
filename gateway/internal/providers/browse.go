package providers

import (
	"context"
	"errors"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

func (a *Adapters) Browse(ctx context.Context, provider string, config Config, parent, query string, offset int) (domain.BrowseResult, error) {
	if offset < 0 || offset > 10000 {
		return domain.BrowseResult{}, errors.New("invalid offset")
	}
	switch provider {
	case "youtube":
		return a.browseYouTube(ctx, config, parent, query, offset)
	case "plex":
		return a.browsePlex(ctx, config, parent, query, offset)
	case "jellyfin":
		return a.browseJellyfin(ctx, config, parent, query, offset)
	case "stremio":
		return a.browseStremio(ctx, config, parent, query, offset)
	default:
		return domain.BrowseResult{}, errors.New("browse unsupported")
	}
}

func sourcePage(sources []Source, offset int) domain.BrowseResult {
	result := domain.BrowseResult{Sources: []Source{}, NextOffset: -1}
	if offset >= len(sources) {
		return result
	}
	end := offset + 40
	if end < len(sources) {
		result.NextOffset = end
	} else {
		end = len(sources)
	}
	result.Sources = sources[offset:end]
	return result
}

func matchingSources(sources []Source, query string) []Source {
	if query == "" {
		return sources
	}
	matches := []Source{}
	for _, source := range sources {
		if strings.Contains(strings.ToLower(source.Item.Title), strings.ToLower(query)) {
			matches = append(matches, source)
		}
	}
	return matches
}
