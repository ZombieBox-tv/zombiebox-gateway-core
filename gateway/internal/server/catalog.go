package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/store"
)

type catalogEntry struct {
	sources []providers.Source
	err     string
	expires time.Time
	loading bool
}

var implemented = map[string]bool{"local": true, "iptv": true, "plex": true, "jellyfin": true, "stremio": true}

func (s *Server) config(ctx context.Context, id string) providers.Config {
	var c providers.Config
	_ = s.db.Get(ctx, "providers", id, &c)
	return c
}
func (s *Server) providers(w http.ResponseWriter, r *http.Request, d domain.Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, id := range providers.Order {
		if id == "local" {
			continue
		}
		c := s.config(r.Context(), id)
		out = append(out, map[string]any{"id": id, "title": providers.Titles[id], "enabled": c.Enabled, "configured": c.URL != "" || c.PlaylistPath != "", "hasToken": c.Token != "", "implemented": implemented[id], "managedByServer": s.managed[id]})
	}
	respond(w, 200, map[string]any{"providers": out})
}
func (s *Server) configureProvider(w http.ResponseWriter, r *http.Request, d domain.Device) {
	// Pairing token proves identity; the current operator code authorizes secret changes.
	if !s.admin(w, r) {
		return
	}
	id := r.PathValue("provider")
	if _, ok := providers.Titles[id]; !ok || id == "local" {
		fail(w, 404, "unknown_provider")
		return
	}
	var patch map[string]json.RawMessage
	if !decode(w, r, &patch) {
		return
	}
	// A client cannot make the server open arbitrary local files. Set playlistPath in the local config file.
	allowed := map[string]bool{"enabled": true, "url": true, "token": true, "userId": true, "epgUrl": true, "catalogId": true, "mediaType": true}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.managed[id] {
		fail(w, 409, "provider_managed_by_server")
		return
	}
	c := s.config(r.Context(), id)
	raw, _ := json.Marshal(c)
	var current map[string]json.RawMessage
	_ = json.Unmarshal(raw, &current)
	for key, value := range patch {
		if !allowed[key] {
			fail(w, 400, "unknown_config_field")
			return
		}
		current[key] = value
	}
	raw, _ = json.Marshal(current)
	if json.Unmarshal(raw, &c) != nil || providers.Validate(c) != nil {
		fail(w, 400, "invalid_provider_config")
		return
	}
	if s.db.Put(r.Context(), "providers", id, c) != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.configRevision[id]++
	delete(s.catalogCache, id)
	s.events.publish("", "modules.changed", map[string]string{"provider": id})
	respond(w, 200, map[string]any{"id": id, "enabled": c.Enabled, "configured": c.URL != "" || c.PlaylistPath != "", "hasToken": c.Token != ""})
}
func (s *Server) SeedProviders(ctx context.Context, configs map[string]providers.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := []store.Record{}
	for id, c := range configs {
		if _, ok := providers.Titles[id]; !ok || id == "local" {
			return errors.New("unknown provider")
		}
		if err := providers.Validate(c); err != nil {
			return err
		}
		records = append(records, store.Record{Bucket: "providers", ID: id, Value: c})
	}
	if err := s.db.PutMany(ctx, records...); err != nil {
		return err
	}
	for id := range configs {
		s.managed[id] = true
		s.configRevision[id]++
		delete(s.catalogCache, id)
	}
	return nil
}
func (s *Server) catalog(ctx context.Context) []providers.Source {
	requestContext := ctx
	ctx, cancel := context.WithTimeout(ctx, s.opt.CatalogWait)
	defer cancel()
	var wg sync.WaitGroup
	for _, id := range providers.Order {
		if !implemented[id] {
			continue
		}
		s.mu.Lock()
		c := s.config(ctx, id)
		revision := s.configRevision[id]
		if id != "local" && !c.Enabled {
			s.mu.Unlock()
			continue
		}
		entry := s.catalogCache[id]
		if entry.loading || time.Now().Before(entry.expires) {
			s.mu.Unlock()
			continue
		}
		entry.loading = true
		s.catalogCache[id] = entry
		s.mu.Unlock()
		wg.Add(1)
		go func(id string, c providers.Config, revision uint64) {
			defer wg.Done()
			sources, err := providers.Fetch(ctx, id, c, s.opt.MediaDir)
			message := ""
			if err != nil {
				message = "Service unavailable. Check its configuration."
			}
			s.mu.Lock()
			if s.configRevision[id] == revision {
				s.catalogCache[id] = catalogEntry{sources: sources, err: message, expires: time.Now().Add(30 * time.Second)}
			}
			s.mu.Unlock()
		}(id, c, revision)
	}
	wg.Wait()
	out := []providers.Source{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range providers.Order {
		if id == "local" || s.config(requestContext, id).Enabled {
			out = append(out, s.catalogCache[id].sources...)
		}
	}
	return out
}
func (s *Server) moduleList(ctx context.Context) []domain.Module {
	out := []domain.Module{}
	for _, id := range providers.Order {
		c := s.config(ctx, id)
		m := domain.Module{ID: id, Title: providers.Titles[id], State: "DISABLED", Features: []string{}, Message: "Configure this service in Settings"}
		if id == "local" || c.Enabled {
			if implemented[id] {
				m.State = "HEALTHY"
				m.Features = []string{"catalog", "playback"}
				m.Message = ""
				s.mu.Lock()
				entry := s.catalogCache[id]
				s.mu.Unlock()
				if entry.expires.IsZero() {
					m.State = "STARTING"
					m.Message = "Waiting for the first catalog check"
				}
				if entry.err != "" {
					m.State = "DEGRADED"
					m.Message = entry.err
				}
			} else {
				m.State = "DEGRADED"
				m.Message = "Adapter implementation pending"
			}
		}
		out = append(out, m)
	}
	return out
}
func (s *Server) modules(w http.ResponseWriter, r *http.Request, d domain.Device) {
	respond(w, 200, map[string]any{"apiVersion": 1, "modules": s.moduleList(r.Context())})
}
func (s *Server) home(w http.ResponseWriter, r *http.Request, d domain.Device) {
	sources := s.catalog(r.Context())
	screen := domain.Screen{APIVersion: 1, UIVersion: 1, Screen: "home", Sections: []domain.Section{}}
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	scope := r.URL.Query().Get("provider")
	rows := map[string][]domain.Item{}
	available := map[string]domain.Item{}
	for _, src := range sources {
		available[src.Item.ID] = src.Item
		if scope != "" && scope != src.Item.Provider {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(src.Item.Title), search) {
			continue
		}
		rows[src.Item.Provider] = append(rows[src.Item.Provider], src.Item)
	}
	if search == "" && scope == "" {
		stored, _ := s.db.List(r.Context(), "progress:"+d.ID)
		progress := []domain.Progress{}
		for _, raw := range stored {
			var p domain.Progress
			if json.Unmarshal(raw, &p) == nil && p.PositionMS > 0 && p.State != "ENDED" {
				if item, ok := available[p.Item.ID]; ok {
					p.Item = item
					p.Item.PositionMS = p.PositionMS
					progress = append(progress, p)
				}
			}
		}
		sort.Slice(progress, func(i, j int) bool { return progress[i].UpdatedAt > progress[j].UpdatedAt })
		items := []domain.Item{}
		for _, p := range progress {
			if len(items) >= 10 {
				break
			}
			items = append(items, p.Item)
		}
		if len(items) > 0 {
			screen.Sections = append(screen.Sections, domain.Section{ID: "continue", Type: "continue_watching", Title: "Continue Watching", Items: items})
		}
	}
	for _, id := range providers.Order {
		items := rows[id]
		if len(items) == 0 {
			continue
		}
		if len(items) > 10 {
			items = items[:10]
		}
		kind := "landscape_row"
		if id == "iptv" {
			kind = "channel_row"
		}
		screen.Sections = append(screen.Sections, domain.Section{ID: id, Type: kind, Title: providers.Titles[id], Items: items})
		if screen.Hero == nil {
			screen.Hero = &domain.Hero{Item: items[0], Description: items[0].Description}
		}
	}
	respond(w, 200, screen)
}
