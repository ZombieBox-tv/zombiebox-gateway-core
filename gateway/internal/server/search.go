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
	if provider != "youtube" || query == "" {
		return s.catalog(ctx), nil
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

func (s *Server) searchSource(ctx context.Context, device, id string) *providers.Source {
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
