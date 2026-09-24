package server

import (
	"net/http"

	"zombiebox.local/gateway/internal/domain"
)

// The receiver PIN is available to a paired TV without granting provider administration.
func (s *Server) airplayPairing(w http.ResponseWriter, r *http.Request, d domain.Device) {
	w.Header().Set("Cache-Control", "no-store")
	c := s.config(r.Context(), "airplay")
	if !c.Enabled {
		fail(w, http.StatusConflict, "provider_disabled")
		return
	}
	if s.deps.AirPlayPairing == nil {
		fail(w, http.StatusBadGateway, "receiver_unavailable")
		return
	}
	pin, err := s.deps.AirPlayPairing.AirPlayPIN(r.Context(), c)
	if err != nil {
		fail(w, http.StatusBadGateway, "receiver_unavailable")
		return
	}
	respond(w, http.StatusOK, map[string]string{"pin": pin})
}
