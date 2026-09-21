package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"zombiebox.local/gateway/internal/catalog"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

func (s *Server) browsePage(w http.ResponseWriter, r *http.Request, d domain.Device) {
	query := r.URL.Query()
	provider, parent, search := query.Get("provider"), query.Get("parent"), query.Get("q")
	offset := 0
	if raw := query.Get("offset"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 10000 {
			fail(w, 400, "invalid_offset")
			return
		}
		offset = value
	}
	if len(parent) > 100 || len(search) > 200 {
		fail(w, 400, "invalid_browse_request")
		return
	}
	if s.deps.Browse == nil || (provider != "plex" && provider != "jellyfin" && provider != "stremio" && provider != "youtube") {
		fail(w, 404, "browse_unavailable")
		return
	}
	s.mu.Lock()
	config, revision := s.config(r.Context(), provider), s.configRevision[provider]
	s.mu.Unlock()
	if !config.Enabled {
		fail(w, 409, "provider_disabled")
		return
	}
	page, err := s.browse.Page(r.Context(), d.ID, provider, providers.Titles[provider], revision, config, parent, search, offset)
	if err != nil {
		if errors.Is(err, catalog.ErrExpired) {
			fail(w, 410, "browse_expired")
		} else {
			fail(w, 502, "browse_unavailable")
		}
		return
	}
	s.mu.Lock()
	unchanged := revision == s.configRevision[provider]
	s.mu.Unlock()
	if !unchanged {
		fail(w, 409, "provider_changed")
		return
	}
	respond(w, 200, page)
}

func (s *Server) browseSource(ctx context.Context, device, id string) *domain.Source {
	provider := s.browse.Provider(ctx, device, id)
	if provider == "" {
		return nil
	}
	s.mu.Lock()
	config, revision := s.config(ctx, provider), s.configRevision[provider]
	s.mu.Unlock()
	if !config.Enabled {
		return nil
	}
	entry, ok := s.browse.Resolve(ctx, device, id, config, revision)
	if !ok {
		return nil
	}
	s.mu.Lock()
	unchanged := revision == s.configRevision[provider]
	s.mu.Unlock()
	if !unchanged {
		return nil
	}
	return &entry.Source
}
