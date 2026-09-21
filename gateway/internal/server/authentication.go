// Package server exposes the versioned client protocol. Provider DTOs stay here.
package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func (s *Server) auth(next func(http.ResponseWriter, *http.Request, domain.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Zombie-Device")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !installationID.MatchString(id) || len(token) != 64 {
			fail(w, 401, "unauthorized")
			return
		}
		var expected string
		err := s.db.Get(r.Context(), "tokens", id, &expected)
		if err != nil || subtle.ConstantTimeCompare([]byte(tokenHash(token)), []byte(expected)) != 1 {
			fail(w, 401, "unauthorized")
			return
		}
		var d domain.Device
		if s.db.Get(r.Context(), "devices", id, &d) != nil {
			fail(w, 401, "unauthorized")
			return
		}
		s.mu.Lock()
		s.seen[d.ID] = time.Now()
		s.mu.Unlock()
		next(w, r, d)
	}
}
