package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

type catalogEntry struct {
	sources []providers.Source
	err     string
	expires time.Time
	loading bool
}

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
		if id == "android_mirror" {
			out = append(out, map[string]any{"id": id, "title": providers.Titles[id], "enabled": s.opt.RelayURL != "", "configured": s.opt.RelayURL != "", "hasToken": false, "implemented": true, "managedByServer": true})
			continue
		}
		out = append(out, map[string]any{"id": id, "title": providers.Titles[id], "enabled": c.Enabled, "configured": c.URL != "" || c.PlaylistPath != "", "hasToken": c.Token != "", "implemented": s.deps.Catalog.HasCatalog(id) || id == "rebrowser", "managedByServer": s.managed[id]})
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
	allowed := map[string]bool{"enabled": true, "url": true, "token": true, "userId": true, "epgUrl": true, "epgMappings": true, "catalogId": true, "mediaType": true}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.managed[id] || id == "android_mirror" {
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
	records := []domain.Record{}
	for id, c := range configs {
		if _, ok := providers.Titles[id]; !ok || id == "local" {
			return errors.New("unknown provider")
		}
		if err := providers.Validate(c); err != nil {
			return err
		}
		records = append(records, domain.Record{Bucket: "providers", ID: id, Value: c})
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
		if !s.deps.Catalog.HasCatalog(id) {
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
			sources, err := s.deps.Catalog.Fetch(ctx, id, c, s.opt.MediaDir)
			decorateArtwork(sources)
			message := ""
			if err != nil {
				if errors.Is(err, providers.ErrPlaylistUnconfigured) {
					message = "Add an M3U playlist in Services"
				} else {
					message = "Service unavailable. Check its configuration."
				}
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
		if id == "rebrowser" {
			if c.Enabled {
				m.State = "STARTING"
				m.Features = []string{"browser"}
				m.Message = "Browser starts when a session opens"
				s.mu.Lock()
				if s.browser != nil && !s.browser.busy && time.Since(s.browser.touched) < 90*time.Second {
					m.State = "HEALTHY"
					m.Message = ""
				}
				s.mu.Unlock()
			}
			out = append(out, m)
			continue
		}
		if id == "android_mirror" {
			if s.opt.RelayURL != "" {
				m.State = "STARTING"
				m.Features = []string{"screen-receiver"}
				m.Message = "Enable receiving on a client; relay is checked when sharing starts"
				s.mu.Lock()
				for _, cast := range s.casts {
					if cast.plan != nil {
						m.State = "HEALTHY"
						m.Message = ""
					}
				}
				s.mu.Unlock()
			}
			out = append(out, m)
			continue
		}
		if id == "local" || c.Enabled {
			if id == "iptv" && c.URL == "" && c.PlaylistPath == "" {
				m.State = "NEEDS_SETUP"
				m.Features = s.deps.Catalog.Features(id)
				m.Message = "Add an M3U playlist in Services"
				out = append(out, m)
				continue
			}
			if s.deps.Catalog.HasCatalog(id) {
				m.State = "HEALTHY"
				m.Features = s.deps.Catalog.Features(id)
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
	scope := r.URL.Query().Get("provider")
	query := r.URL.Query().Get("q")
	search := strings.ToLower(strings.TrimSpace(query))
	activityCursor := r.URL.Query().Get("activityCursor")
	if activityCursor != "" && (scope != "youtube" || search != "") {
		fail(w, 400, "invalid_activity_cursor")
		return
	}
	if scope == "youtube" && search == "" {
		if activityCursor != "" {
			s.youtubeActivityHome(w, r, d, activityCursor)
			return
		}
		activity, err := s.youtubeActivityRecords(r.Context(), d.ID)
		if err != nil {
			fail(w, 500, "storage_error")
			return
		}
		items, next, hasActivity := youtubeActivityPageFromRecords(activity, nil)
		if hasActivity {
			recommendationContext, cancel := context.WithTimeout(r.Context(), youtubeActivityRecommendationTimeout)
			recommendations := s.youtubeActivityRecommendations(recommendationContext, d.ID, activity)
			cancel()
			if r.Context().Err() != nil {
				return
			}
			s.respondYouTubeActivity(w, items, next, recommendations)
			return
		}
	}
	sources, err := s.screenSources(r.Context(), d.ID, scope, query)
	if err != nil {
		fail(w, 502, "search_unavailable")
		return
	}
	favorites, err := s.iptvFavoriteIDs(r.Context())
	if err != nil {
		fail(w, 500, "storage_error")
		return
	}
	screen := domain.Screen{APIVersion: 1, UIVersion: 1, Screen: "home", Sections: []domain.Section{}}
	rows := map[string][]domain.Item{}
	available := map[string]domain.Item{}
	for _, src := range sources {
		if src.Item.Provider == "iptv" {
			src.Item.Favorite = favorites[src.Item.ID]
		}
		available[src.Item.ID] = src.Item
		if scope != "" && scope != src.Item.Provider {
			continue
		}
		if search != "" && scope != "youtube" && !strings.Contains(strings.ToLower(src.Item.Title), search) {
			continue
		}
		rows[src.Item.Provider] = append(rows[src.Item.Provider], src.Item)
	}
	if scope == "youtube" && search == "" {
		s.mu.Lock()
		feed := s.youtubeHomeFeeds[d.ID]
		if feed.revision == s.configRevision["youtube"] {
			screen.NextOffset = feed.nextOffset
		}
		s.mu.Unlock()
	}
	if search == "" && scope == "" {
		stored, _ := s.db.List(r.Context(), "progress:"+d.ID)
		progress := []domain.Progress{}
		for _, raw := range stored {
			var p domain.Progress
			if json.Unmarshal(raw, &p) == nil && p.PositionMS > 0 && p.State != "ENDED" {
				item, ok := available[p.Item.ID]
				if !ok && p.Item.Playable {
					s.mu.Lock()
					config, revision := s.config(r.Context(), p.Item.Provider), s.configRevision[p.Item.Provider]
					s.mu.Unlock()
					if config.Enabled && s.browse.Known(r.Context(), d.ID, p.Item.ID, config, revision) {
						item, ok = p.Item, true
					}
				}
				if ok {
					p.Item = item
					p.Item.PositionMS = p.PositionMS
					if p.DurationMS > 0 {
						p.Item.DurationMS = p.DurationMS
					}
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
		limit := 10
		if id == "youtube" && scope == "youtube" && search == "" {
			limit = 40
		}
		if len(items) > limit {
			items = items[:limit]
		}
		kind := "landscape_row"
		if id == "iptv" {
			kind = "channel_row"
		}
		screen.Sections = append(screen.Sections, domain.Section{ID: id, Type: kind, Title: providers.Titles[id], Items: items})

	}
	if search == "" {
		screen.Hero = s.heroes.Select(r.Context(), d.ID, scope, screen.Sections)
	}
	respond(w, 200, screen)
}

const youtubeActivityPageSize = 40

type youtubeActivityCursor struct {
	UpdatedAt int64  `json:"t"`
	ItemID    string `json:"i"`
}

func encodeYouTubeActivityCursor(cursor youtubeActivityCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeYouTubeActivityCursor(raw string) (*youtubeActivityCursor, error) {
	if len(raw) > 512 {
		return nil, errors.New("activity cursor too long")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("invalid activity cursor")
	}
	var cursor youtubeActivityCursor
	if err := json.Unmarshal(data, &cursor); err != nil ||
		cursor.UpdatedAt <= 0 ||
		cursor.ItemID == "" ||
		len(cursor.ItemID) > 256 ||
		strings.ContainsAny(cursor.ItemID, "\x00\r\n") {
		return nil, errors.New("invalid activity cursor")
	}
	return &cursor, nil
}

func (s *Server) youtubeActivityHome(w http.ResponseWriter, r *http.Request, d domain.Device, rawCursor string) {
	cursor, err := decodeYouTubeActivityCursor(rawCursor)
	if err != nil {
		fail(w, 400, "invalid_activity_cursor")
		return
	}
	items, next, _, err := s.youtubeActivityPage(r.Context(), d.ID, cursor)
	if err != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.respondYouTubeActivity(w, items, next, nil)
}

func (s *Server) respondYouTubeActivity(w http.ResponseWriter, items []domain.Item, next string, recommendations []domain.Item) {
	sections := []domain.Section{}
	if len(recommendations) > 0 {
		sections = append(sections, domain.Section{
			ID:    "youtube-watch-title-suggestions",
			Type:  "landscape_row",
			Title: "Title search suggestions from ZombieBox watch activity",
			Items: recommendations,
		})
	}
	if len(items) > 0 {
		sections = append(sections, domain.Section{
			ID:    "youtube-activity",
			Type:  "landscape_row",
			Title: "Your ZombieBox YouTube activity",
			Items: items,
		})
	}
	response := map[string]any{
		"apiVersion":      1,
		"uiSchemaVersion": 1,
		"screen":          "home",
		"feedType":        "zombiebox_activity",
		"sections":        sections,
	}
	if next != "" {
		response["nextCursor"] = next
	}
	respond(w, 200, response)
}

func (s *Server) youtubeActivityPage(ctx context.Context, device string, cursor *youtubeActivityCursor) ([]domain.Item, string, bool, error) {
	records, err := s.youtubeActivityRecords(ctx, device)
	if err != nil {
		return nil, "", false, err
	}
	items, next, hasActivity := youtubeActivityPageFromRecords(records, cursor)
	return items, next, hasActivity, nil
}

func (s *Server) youtubeActivityRecords(ctx context.Context, device string) ([]domain.Progress, error) {
	stored, err := s.db.List(ctx, "progress:"+device)
	if err != nil {
		return nil, err
	}
	progress := make([]domain.Progress, 0, len(stored))
	seen := make(map[string]struct{}, len(stored))
	for _, raw := range stored {
		var record domain.Progress
		if json.Unmarshal(raw, &record) != nil ||
			record.Item.Provider != "youtube" ||
			record.Item.Kind != "video" ||
			!record.Item.Playable ||
			record.Item.ID == "" ||
			record.PositionMS <= 0 ||
			record.UpdatedAt <= 0 {
			continue
		}
		if _, exists := seen[record.Item.ID]; exists {
			continue
		}
		seen[record.Item.ID] = struct{}{}
		progress = append(progress, record)
	}
	if len(progress) == 0 {
		return progress, nil
	}
	sort.Slice(progress, func(i, j int) bool {
		if progress[i].UpdatedAt != progress[j].UpdatedAt {
			return progress[i].UpdatedAt > progress[j].UpdatedAt
		}
		return progress[i].Item.ID < progress[j].Item.ID
	})
	return progress, nil
}

func youtubeActivityPageFromRecords(progress []domain.Progress, cursor *youtubeActivityCursor) ([]domain.Item, string, bool) {
	if len(progress) == 0 {
		return nil, "", false
	}
	filtered := make([]domain.Progress, 0, len(progress))
	for _, record := range progress {
		if cursor != nil {
			if record.UpdatedAt > cursor.UpdatedAt || (record.UpdatedAt == cursor.UpdatedAt && record.Item.ID <= cursor.ItemID) {
				continue
			}
		}
		filtered = append(filtered, record)
	}
	end := youtubeActivityPageSize
	if len(filtered) < end {
		end = len(filtered)
	}
	items := make([]domain.Item, 0, end)
	for _, record := range filtered[:end] {
		item := record.Item
		item.PositionMS = record.PositionMS
		if record.DurationMS > 0 {
			item.DurationMS = record.DurationMS
		}
		items = append(items, item)
	}
	next := ""
	if len(filtered) > end {
		last := filtered[end-1]
		next = encodeYouTubeActivityCursor(youtubeActivityCursor{UpdatedAt: last.UpdatedAt, ItemID: last.Item.ID})
	}
	return items, next, true
}
