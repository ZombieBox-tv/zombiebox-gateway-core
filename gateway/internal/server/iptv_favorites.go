package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"

	"zombiebox.local/gateway/internal/domain"
)

const iptvFavoriteBucket = "iptv_favorites"

var iptvItemID = regexp.MustCompile(`^iptv-[a-f0-9]{16}$`)

type iptvFavoriteRecord struct {
	ID string `json:"id"`
}

func (s *Server) iptvFavoriteIDs(ctx context.Context) (map[string]bool, error) {
	records, err := s.db.List(ctx, iptvFavoriteBucket)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(records))
	for _, raw := range records {
		var record iptvFavoriteRecord
		if json.Unmarshal(raw, &record) == nil && iptvItemID.MatchString(record.ID) {
			ids[record.ID] = true
		}
	}
	return ids, nil
}

// Favorites are a household collection of current channel IDs; stream URLs and
// provider credentials are never copied into the preference record.
func (s *Server) iptvFavorite(w http.ResponseWriter, r *http.Request, d domain.Device) {
	id := r.PathValue("item")
	if !iptvItemID.MatchString(id) {
		fail(w, 400, "invalid_iptv_item")
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.db.Delete(r.Context(), iptvFavoriteBucket, id); err != nil {
			fail(w, 500, "storage_error")
			return
		}
		s.events.publish("", "catalog.changed", map[string]string{"provider": "iptv"})
		respond(w, 200, map[string]any{"itemId": id, "favorite": false})
		return
	}
	if !s.config(r.Context(), "iptv").Enabled {
		fail(w, 409, "provider_disabled")
		return
	}
	sources, err := s.screenSources(r.Context(), d.ID, "iptv", "")
	if err != nil {
		fail(w, 502, "catalog_unavailable")
		return
	}
	found := false
	for _, source := range sources {
		if source.Item.Provider == "iptv" && source.Item.ID == id {
			found = true
			break
		}
	}
	if !found {
		fail(w, 404, "channel_not_found")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	favorites, err := s.iptvFavoriteIDs(r.Context())
	if err != nil {
		fail(w, 500, "storage_error")
		return
	}
	if len(favorites) >= 256 && !favorites[id] {
		fail(w, 409, "favorite_limit")
		return
	}
	if err := s.db.Put(r.Context(), iptvFavoriteBucket, id, iptvFavoriteRecord{ID: id}); err != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.events.publish("", "catalog.changed", map[string]string{"provider": "iptv"})
	respond(w, 200, map[string]any{"itemId": id, "favorite": true})
}
