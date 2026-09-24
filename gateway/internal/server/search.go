package server

import (
	"context"
	"errors"
	"time"

	"zombiebox.local/gateway/internal/providers"
)

type searchResult struct {
	query    string
	revision uint64
	fetched  time.Time
	sources  []providers.Source
}

// At most one 40-item result per registered device (registration itself is capped at 64).
func (s *Server) screenSources(ctx context.Context, device, provider, query string) ([]providers.Source, error) {
	if provider != "youtube" {
		return s.catalog(ctx), nil
	}
	if query == "" {
		return s.youtubeHomeSources(ctx, device), nil
	}
	if len(query) > 200 {
		return nil, errors.New("query too long")
	}
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
	s.searchResults[device] = searchResult{query, revision, time.Now(), sources}
	return sources, nil
}

// The anonymous YouTube home feed is empty without an account. Browse a real
// exploration shelf directly so an empty catalog cannot consume the client's
// request budget before the shelf is loaded. Optional worker failures leave
// Home usable.
func (s *Server) youtubeHomeSources(ctx context.Context, device string) []providers.Source {
	s.mu.Lock()
	config, revision := s.config(ctx, "youtube"), s.configRevision["youtube"]
	entry := s.youtubeHomeFeeds[device]
	s.mu.Unlock()
	if !config.Enabled {
		return nil
	}
	var base []providers.Source
	if config.CatalogID != "" || s.deps.Browse == nil {
		base = s.catalog(ctx)
		for _, source := range base {
			if source.Item.Provider == "youtube" && source.Item.Playable {
				return base
			}
		}
	}
	if s.deps.Browse == nil {
		return base
	}
	if entry.revision == revision && time.Since(entry.fetched) < 2*time.Minute {
		return append(base, entry.sources...)
	}
	feedCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	page, err := s.browse.Page(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, "", "popular", 0)
	if (err != nil || len(page.Items) == 0) && feedCtx.Err() == nil {
		page, err = s.browse.Page(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, "", "popular", 0)
	}
	if err != nil {
		return base
	}
	sources := make([]providers.Source, 0, len(page.Items))
	for _, item := range page.Items {
		if item.Kind == "video" {
			sources = append(sources, providers.Source{Item: item})
		}
	}
	s.mu.Lock()
	if s.configRevision["youtube"] == revision && len(sources) > 0 {
		s.youtubeHomeFeeds[device] = searchResult{revision: revision, fetched: time.Now(), sources: sources}
	} else {
		sources = nil
	}
	s.mu.Unlock()
	return append(base, sources...)
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
