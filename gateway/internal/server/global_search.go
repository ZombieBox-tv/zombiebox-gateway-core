package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

type searchSection struct {
	Provider string        `json:"provider"`
	State    string        `json:"state"`
	Items    []domain.Item `json:"items"`
	More     bool          `json:"more"`
}

func (s *Server) globalSearch(w http.ResponseWriter, r *http.Request, d domain.Device) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) < 2 || len(query) > 200 {
		fail(w, 400, "invalid_search_query")
		return
	}
	s.recordRecentYouTubeContext(d.ID, "", query, "")
	select {
	case s.searchJobs <- struct{}{}:
		defer func() { <-s.searchJobs }()
	default:
		fail(w, 429, "search_busy")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	ids := []string{"youtube", "plex", "jellyfin", "stremio", "iptv", "local"}
	sections := make([]searchSection, len(ids))
	budget := 20
	if d.Registration.Memory.ClassMB <= 64 {
		budget = 8
	}
	jobs := make(chan int, len(ids))
	for i := range ids {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for n := 0; n < 2; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				sections[index] = s.searchProvider(ctx, d.ID, ids[index], query, budget)
			}
		}()
	}
	workers.Wait()
	w.Header().Set("Cache-Control", "private, no-store")
	respond(w, 200, map[string]any{"query": query, "sections": sections})
}

func (s *Server) searchProvider(ctx context.Context, device, provider, query string, budget int) searchSection {
	out := searchSection{Provider: provider, State: "UNAVAILABLE", Items: []domain.Item{}}
	if ctx.Err() != nil {
		return out
	}
	s.mu.Lock()
	config, revision := s.config(ctx, provider), s.configRevision[provider]
	s.mu.Unlock()
	if provider != "local" && !config.Enabled {
		out.State = "DISABLED"
		return out
	}
	out.State = "UNAVAILABLE"
	if ctx.Err() != nil {
		return out
	}
	if provider == "local" || provider == "iptv" {
		if !s.deps.Catalog.HasCatalog(provider) {
			return out
		}
		sources, err := s.deps.Catalog.Fetch(ctx, provider, config, s.opt.MediaDir)
		if err != nil {
			return out
		}
		decorateArtwork(sources)
		for _, source := range sources {
			if strings.Contains(strings.ToLower(source.Item.Title), strings.ToLower(query)) {
				if len(out.Items) == budget {
					out.More = true
					break
				}
				out.Items = append(out.Items, source.Item)
			}
		}
	} else {
		if s.deps.Browse == nil {
			return out
		}
		page, err := s.browse.Page(ctx, device, provider, providers.Titles[provider], revision, config, "", query, 0)
		if err != nil {
			return out
		}
		out.More = page.NextOffset >= 0 || len(page.Items) > budget
		out.Items = page.Items[:min(budget, len(page.Items))]
	}
	s.mu.Lock()
	unchanged := revision == s.configRevision[provider]
	s.mu.Unlock()
	if !unchanged {
		out.Items = []domain.Item{}
		out.More = false
		return out
	}
	for index := range out.Items {
		item := &out.Items[index]
		item.Title = previewText(item.Title, 300)
		item.Subtitle = previewText(item.Subtitle, 300)
		item.Description = previewText(item.Description, 2000)
		item.Programmes = nil
	}
	out.State = "READY"
	return out
}

func previewText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}
