package server

import (
	"crypto/subtle"
	"net"
	"net/http"
	"time"
)

// Administration needs both a paired device and the operator's current code.
// Failed attempts share the bounded pairing throttle, including across devices.
func (s *Server) admin(w http.ResponseWriter, r *http.Request) bool {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, a := range s.attempts {
		if now.After(a.until) {
			delete(s.attempts, key)
		}
	}
	a := s.attempts[ip]
	if a.count >= 5 || len(s.attempts) >= 256 {
		fail(w, 429, "administration_rate_limited")
		return false
	}
	if s.opt.PairingCode == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Zombie-Admin-Code")), []byte(s.opt.PairingCode)) != 1 {
		a.count++
		a.until = now.Add(time.Minute)
		s.attempts[ip] = a
		fail(w, 403, "admin_code_required")
		return false
	}
	delete(s.attempts, ip)
	return true
}
