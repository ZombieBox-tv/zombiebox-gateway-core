package server

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

// A bounded page lets the client browse large IPTV lists without retaining
// thousands of Views or fetching upstream provider DTOs.
func (s *Server) catalogPage(w http.ResponseWriter, r *http.Request, d domain.Device) {
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 0 || n > 10000 {
			fail(w, 400, "invalid_offset")
			return
		}
		offset = n
	}
	provider := r.URL.Query().Get("provider")
	category := r.URL.Query().Get("category")
	if category != "" && (provider != "iptv" || len([]rune(category)) > 80 || strings.ContainsAny(category, "\x00\r\n")) {
		fail(w, 400, "invalid_category_filter")
		return
	}
	favoritesOnly := r.URL.Query().Get("favorites") == "1"
	if favoritesOnly && provider != "iptv" {
		fail(w, 400, "invalid_favorites_filter")
		return
	}
	favorites, err := s.iptvFavoriteIDs(r.Context())
	if err != nil {
		fail(w, 500, "storage_error")
		return
	}
	query := strings.ToLower(r.URL.Query().Get("q"))
	matches := []domain.Item{}
	categorySet := map[string]bool{}
	sources, err := s.screenSources(r.Context(), d.ID, provider, r.URL.Query().Get("q"))
	if err != nil {
		fail(w, 502, "search_unavailable")
		return
	}
	for _, source := range sources {
		if source.Item.Provider == "iptv" {
			source.Item.Favorite = favorites[source.Item.ID]
			if source.Item.Category != "" {
				categorySet[source.Item.Category] = true
			}
		}
		if category != "" && source.Item.Category != category {
			continue
		}
		if favoritesOnly && !source.Item.Favorite {
			continue
		}
		if (provider == "" || source.Item.Provider == provider) && (query == "" || provider == "youtube" || strings.Contains(strings.ToLower(source.Item.Title), query)) {
			matches = append(matches, source.Item)
		}
	}
	items := []domain.Item{}
	categories := make([]string, 0, len(categorySet))
	for name := range categorySet {
		categories = append(categories, name)
	}
	sort.Strings(categories)
	if len(categories) > 128 {
		categories = categories[:128]
	}
	next := -1
	if offset < len(matches) {
		end := offset + 40
		if end < len(matches) {
			next = end
		} else {
			end = len(matches)
		}
		items = matches[offset:end]
	}
	respond(w, 200, map[string]any{"apiVersion": 1, "items": items, "nextOffset": next, "total": len(matches), "categories": categories})
}
