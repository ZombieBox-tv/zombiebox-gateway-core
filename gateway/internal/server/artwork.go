package server

import (
	"net/http"
	"net/url"

	"zombiebox.local/gateway/internal/domain"
)

func decorateArtwork(sources []domain.Source) {
	for i := range sources {
		if sources[i].ArtworkURL != "" {
			sources[i].Item.ImageURL = "/v1/artwork/" + url.PathEscape(sources[i].Item.ID)
		}
	}
}
func (s *Server) artwork(w http.ResponseWriter, r *http.Request, d domain.Device) {
	if s.deps.Artwork == nil {
		fail(w, 503, "artwork_unavailable")
		return
	}
	id := r.PathValue("item")
	source := s.searchSource(r.Context(), d.ID, id)
	if source == nil {
		for _, candidate := range s.catalog(r.Context()) {
			if candidate.Item.ID == id {
				source = &candidate
				break
			}
		}
	}
	if source == nil {
		fail(w, 404, "artwork_unavailable")
		return
	}
	data, err := s.deps.Artwork.Image(r.Context(), *source, r.URL.Query().Get("size") == "hero")
	if err != nil {
		fail(w, 404, "artwork_unavailable")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Write(data)
}
