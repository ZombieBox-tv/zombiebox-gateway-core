package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"zombiebox.local/gateway/internal/artwork"
	"zombiebox.local/gateway/internal/domain"
)

func decorateArtwork(sources []domain.Source) {
	for i := range sources {
		if sources[i].ArtworkURL != "" {
			identity, _ := json.Marshal(struct {
				URL     string
				Title   string
				Headers http.Header
			}{sources[i].ArtworkURL, sources[i].Item.Title, sources[i].ArtworkHeaders})
			sum := sha256.Sum256(identity)
			sources[i].Item.ImageURL = "/v1/artwork/" + url.PathEscape(sources[i].Item.ID) + "?rev=" + hex.EncodeToString(sum[:8])
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
	if (id == "spotify-connect" || id == "airplay-audio") && s.deps.Reception != nil {
		provider := "spotify"
		if id == "airplay-audio" {
			provider = "airplay"
		}
		s.mu.Lock()
		config := s.config(r.Context(), provider)
		s.mu.Unlock()
		if config.Enabled {
			current, _, err := s.deps.Reception.Reception(r.Context(), provider, config)
			if err == nil && current != nil && current.Item.ID == id {
				source = current
			}
		}
	}
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
	profile := artworkProfile(d, r.URL.Query().Get("size"))
	preferred := artwork.FormatJPEG
	if r.URL.Query().Get("format") == string(artwork.FormatWebP) && d.Registration.Platform.AndroidAPI >= 14 {
		preferred = artwork.FormatWebP
	}
	var data []byte
	actual := artwork.FormatJPEG
	var err error
	if encoder, ok := s.deps.Artwork.(interface {
		ImageAs(context.Context, domain.Source, domain.ArtworkProfile, artwork.ImageFormat) ([]byte, artwork.ImageFormat, error)
	}); ok {
		data, actual, err = encoder.ImageAs(r.Context(), *source, profile, preferred)
	} else {
		data, err = s.deps.Artwork.Image(r.Context(), *source, profile)
	}
	if err != nil {
		fail(w, 404, "artwork_unavailable")
		return
	}
	contentType := ""
	switch actual {
	case artwork.FormatJPEG:
		contentType = "image/jpeg"
	case artwork.FormatWebP:
		contentType = "image/webp"
	default:
		fail(w, 404, "artwork_unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Vary", "Authorization")
	sum := sha256.Sum256(data)
	etag := "\"" + hex.EncodeToString(sum[:]) + "\""
	w.Header().Set("ETag", etag)
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		value := strings.TrimSpace(candidate)
		if strings.TrimPrefix(value, "W/") == etag || value == "*" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Write(data)
}

// Conservative unknown-device defaults; the layout role and memory/display tier
// select a finite variant. Clients cannot request arbitrary pixel dimensions.
func artworkProfile(device domain.Device, size string) domain.ArtworkProfile {
	registration := device.Registration
	low := registration.Memory.PhysicalMB <= 768 || registration.Memory.ClassMB <= 96 || registration.Display.Width <= 960
	if size == "audio" {
		if low {
			return domain.ArtworkAudioSmall
		}
		return domain.ArtworkAudioMedium
	}
	if size == "hero" {
		if low {
			return domain.ArtworkHeroSmall
		}
		return domain.ArtworkHeroMedium
	}
	if size == "poster" {
		if low {
			return domain.ArtworkPosterSmall
		}
		return domain.ArtworkPosterMedium
	}
	if low {
		return domain.ArtworkLandscapeSmall
	}
	return domain.ArtworkLandscapeMedium
}
