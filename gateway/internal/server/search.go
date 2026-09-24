package server

import (
	"context"
	"errors"
	"time"

	"zombiebox.local/gateway/internal/providers"
)

type searchResult struct {
	query     string
	revision  uint64
	fetched   time.Time
	sources   []providers.Source
	connected bool
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

// The anonymous YouTube home feed chooses content in strict order of availability:
// 1. Signed-in profile feed (from connected OAuth account subscriptions/playlists)
// 2. Recent search/view context (from recent local interactions)
// 3. Fallback: explicitly pinned catalog if configured by operator, otherwise generic popular fallback
// Privacy is preserved and recommendations are never manufactured from a disconnected account.
func (s *Server) youtubeHomeSources(ctx context.Context, device string) []providers.Source {
	s.mu.Lock()
	config, revision := s.config(ctx, "youtube"), s.configRevision["youtube"]
	entry := s.youtubeHomeFeeds[device]
	s.mu.Unlock()
	if !config.Enabled {
		return nil
	}
	if s.deps.Browse == nil {
		var out []providers.Source
		for _, source := range s.catalog(ctx) {
			if source.Item.Provider == "youtube" && source.Item.Playable {
				out = append(out, source)
			}
		}
		return out
	}
	connected := false
	if s.youtubeAccount != nil && s.youtubeAccount.Configured() {
		status := s.youtubeAccount.Status(ctx)
		connected = status.Connected
	}
	if entry.revision == revision && entry.connected == connected && time.Since(entry.fetched) < 2*time.Minute {
		return append([]providers.Source(nil), entry.sources...)
	}
	feedCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var sources []providers.Source

	// 1. Signed-in profile feed (where technically available and connected).
	if s.youtubeAccount != nil && s.youtubeAccount.Configured() {
		status := s.youtubeAccount.Status(feedCtx)
		if status.Connected {
			subs, err := s.youtubeAccount.List(feedCtx, "subscriptions", "")
			if err == nil && len(subs.Items) > 0 {
				for _, sub := range subs.Items {
					if sub.BrowseID == "" {
						continue
					}
					result, err := s.deps.Browse.Browse(feedCtx, "youtube", config, sub.BrowseID, "", 0)
					if err == nil && len(result.Sources) > 0 {
						chanPage := s.browse.Present(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, sub.BrowseID, "", 0, result)
						for _, item := range chanPage.Items {
							if item.Kind == "video" && item.Playable {
								sources = append(sources, providers.Source{Item: item})
								if len(sources) >= 40 {
									break
								}
							}
						}
					}
					if len(sources) >= 20 {
						break
					}
				}
			}
			if len(sources) == 0 {
				pls, err := s.youtubeAccount.List(feedCtx, "playlists", "")
				if err == nil && len(pls.Items) > 0 {
					for _, pl := range pls.Items {
						if pl.BrowseID == "" {
							continue
						}
						result, err := s.deps.Browse.Browse(feedCtx, "youtube", config, pl.BrowseID, "", 0)
						if err == nil && len(result.Sources) > 0 {
							plPage := s.browse.Present(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, pl.BrowseID, "", 0, result)
							for _, item := range plPage.Items {
								if item.Kind == "video" && item.Playable {
									sources = append(sources, providers.Source{Item: item})
									if len(sources) >= 40 {
										break
									}
								}
							}
						}
						if len(sources) >= 20 {
							break
						}
					}
				}
			}
		}
	}

	// 2. Recent search/view context (if available and richer feed not empty).
	if len(sources) == 0 {
		contextQuery := s.getRecentYouTubeContext(feedCtx, device)
		if contextQuery != "" {
			page, err := s.browse.Page(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, "", contextQuery, 0)
			if err == nil && len(page.Items) > 0 {
				for _, item := range page.Items {
					if item.Kind == "video" && item.Playable {
						sources = append(sources, providers.Source{Item: item})
						if len(sources) >= 40 {
							break
						}
					}
				}
			}
		}
	}

	// 3. Fallback: explicitly pinned catalog if configured by operator, otherwise generic popular fallback.
	if len(sources) == 0 {
		fallbackQuery := config.CatalogID
		if fallbackQuery != "" {
			page, err := s.browse.Page(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, "", fallbackQuery, 0)
			if (err != nil || len(page.Items) == 0) && feedCtx.Err() == nil {
				page, err = s.browse.Page(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, "", fallbackQuery, 0)
			}
			if err == nil {
				for _, item := range page.Items {
					if item.Kind == "video" && item.Playable {
						sources = append(sources, providers.Source{Item: item})
						if len(sources) >= 40 {
							break
						}
					}
				}
			}
			if len(sources) == 0 {
				for _, source := range s.catalog(ctx) {
					if source.Item.Provider == "youtube" && source.Item.Playable {
						sources = append(sources, source)
						if len(sources) >= 40 {
							break
						}
					}
				}
			}
		} else {
			fallbackQuery = "popular"
			page, err := s.browse.Page(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, "", fallbackQuery, 0)
			if (err != nil || len(page.Items) == 0) && feedCtx.Err() == nil {
				page, err = s.browse.Page(feedCtx, device, "youtube", providers.Titles["youtube"], revision, config, "", fallbackQuery, 0)
			}
			if err == nil {
				for _, item := range page.Items {
					if item.Kind == "video" && item.Playable {
						sources = append(sources, providers.Source{Item: item})
						if len(sources) >= 40 {
							break
						}
					}
				}
			}
			if len(sources) == 0 {
				for _, source := range s.catalog(ctx) {
					if source.Item.Provider == "youtube" && source.Item.Playable {
						sources = append(sources, source)
						if len(sources) >= 40 {
							break
						}
					}
				}
			}
		}
	}

	s.mu.Lock()
	if s.configRevision["youtube"] == revision && len(sources) > 0 {
		s.youtubeHomeFeeds[device] = searchResult{revision: revision, connected: connected, fetched: time.Now(), sources: sources}
	} else {
		sources = nil
	}
	s.mu.Unlock()
	return append([]providers.Source(nil), sources...)
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
