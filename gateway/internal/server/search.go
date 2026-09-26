package server

import (
	"context"
	"errors"
	"time"

	"zombiebox.local/gateway/internal/providers"
)

type searchResult struct {
	query      string
	revision   uint64
	fetched    time.Time
	sources    []providers.Source
	nextOffset int
}

// At most one 40-item result per registered device (registration itself is capped at 64).
func (s *Server) screenSources(ctx context.Context, device, provider, query string) ([]providers.Source, error) {
	if provider != "youtube" {
		return s.catalog(ctx), nil
	}
	if query == "" {
		return s.youtubeHomeSources(ctx, device)
	}
	if len(query) > 200 {
		return nil, errors.New("query too long")
	}
	s.recordRecentYouTubeContext(device, "", query, "")
	s.mu.Lock()
	c, revision := s.config(ctx, "youtube"), s.configRevision["youtube"]
	entry := s.searchResults[device]
	s.mu.Unlock()
	if !c.Enabled {
		return []providers.Source{}, nil
	}
	if entry.query == query && entry.revision == revision && time.Since(entry.fetched) < 60*time.Second {
		return entry.sources, nil
	}
	c.CatalogID = query
	sources, err := s.deps.Search.YouTube(ctx, c)
	decorateArtwork(sources)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configRevision["youtube"] != revision {
		return nil, errors.New("provider configuration changed")
	}
	s.searchResults[device] = searchResult{query: query, revision: revision, fetched: time.Now(), sources: sources}
	return sources, nil
}

// YouTube Home is the provider's Home feed. OAuth subscriptions and local watch/search
// history do not expose YouTube's recommendation feed and are not substituted here.
func (s *Server) youtubeHomeSources(ctx context.Context, device string) ([]providers.Source, error) {
	s.mu.Lock()
	config, revision := s.config(ctx, "youtube"), s.configRevision["youtube"]
	entry := s.youtubeHomeFeeds[device]
	s.mu.Unlock()
	if !config.Enabled {
		return nil, nil
	}
	if s.deps.Browse == nil {
		// With no browse backend, /catalog is only the provider Home feed when
		// the operator has not configured an explicit catalog query.
		if config.CatalogID != "" {
			return nil, nil
		}
		var out []providers.Source
		for _, source := range s.catalog(ctx) {
			if source.Item.Provider == "youtube" && source.Item.Playable {
				out = append(out, source)
			}
		}
		return out, nil
	}
	if entry.revision == revision && time.Since(entry.fetched) < 2*time.Minute {
		return append([]providers.Source(nil), entry.sources...), nil
	}
	feedCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	page, err := s.browse.Page(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, "", "", 0)
	if err != nil {
		return nil, err
	}
	sources := make([]providers.Source, 0, len(page.Items))
	for _, item := range page.Items {
		if item.Kind != "video" || !item.Playable {
			continue
		}
		entry, ok := s.browse.Resolve(feedCtx, device, item.ID, config, revision)
		if ok {
			sources = append(sources, entry.Source)
		}
	}

	s.mu.Lock()
	if s.configRevision["youtube"] == revision {
		s.youtubeHomeFeeds[device] = searchResult{revision: revision, fetched: time.Now(), sources: sources, nextOffset: page.NextOffset}
	} else {
		s.mu.Unlock()
		return nil, errors.New("provider configuration changed")
	}
	s.mu.Unlock()
	return append([]providers.Source(nil), sources...), nil
}

func (s *Server) searchSource(ctx context.Context, device, id string) *providers.Source {
	if source := s.browseSource(ctx, device, id); source != nil {
		return source
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.searchResults[device]
	if !s.config(ctx, "youtube").Enabled || entry.revision != s.configRevision["youtube"] || time.Since(entry.fetched) > 10*time.Minute {
		return nil
	}
	for _, source := range entry.sources {
		if source.Item.ID == id {
			copy := source
			return &copy
		}
	}
	return nil
}
