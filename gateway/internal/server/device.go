// Package server exposes the versioned client protocol. Provider DTOs stay here.
package server

import (
	"net/http"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func (s *Server) preferences(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var p domain.Preferences
	if !decode(w, r, &p) {
		return
	}
	if !(p.Mode == "AUTO" || p.Mode == "TV" || p.Mode == "DOCKED" || p.Mode == "HANDHELD") || !(p.UILanguage == "en" || p.UILanguage == "es") || !(p.SubtitleMode == "auto" || p.SubtitleMode == "off" || p.SubtitleMode == "forced" || p.SubtitleMode == "always") || len(p.AudioLanguages) > 8 || len(p.SubtitleLanguages) > 8 {
		fail(w, 400, "invalid_preferences")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.Get(r.Context(), "devices", d.ID, &d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	if p.AudioLanguages == nil {
		p.AudioLanguages = []string{}
	}
	if p.SubtitleLanguages == nil {
		p.SubtitleLanguages = []string{}
	}
	d.Preferences = p
	if !p.AllowCasting {
		for _, c := range s.casts {
			if c.receiver == d.ID {
				s.endCastLocked(c)
			}
		}
	}
	if s.db.Put(r.Context(), "devices", d.ID, d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.events.publish(d.ID, "preferences.changed", p)
	respond(w, 200, p)
}
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var c domain.Capabilities
	if !decode(w, r, &c) {
		return
	}
	if c.Version != 1 || c.DeviceID != d.ID || len(c.Probes) > 32 {
		fail(w, 400, "invalid_capabilities")
		return
	}
	seenProbes := map[string]bool{}
	for _, p := range c.Probes {
		if (p.Status == "PASS" && p.Stalled) || seenProbes[p.ID] || p.FirstFrameMS < 0 || p.PositionMS < 0 || p.PrepareMS > 60000 || p.FirstFrameMS > 60000 || p.PositionMS > 60000 || p.ID == "" || len(p.ID) > 80 || p.PrepareMS < 0 || !(p.Status == "PASS" || p.Status == "FAIL" || p.Status == "UNKNOWN") {
			fail(w, 400, "invalid_probe")
			return
		}
		seenProbes[p.ID] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.Get(r.Context(), "devices", d.ID, &d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	if c.Probes == nil {
		c.Probes = []domain.Probe{}
	}
	d.Capabilities = c
	if s.db.Put(r.Context(), "devices", d.ID, d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.events.publish(d.ID, "capabilities.changed", c)
	respond(w, 200, c)
}
func (s *Server) poll(w http.ResponseWriter, r *http.Request, d domain.Device) {
	select {
	case s.polls <- struct{}{}:
		defer func() { <-s.polls }()
	default:
		fail(w, 429, "poll_limit")
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 100 {
		fail(w, 400, "invalid_cursor")
		return
	}
	timer := time.NewTimer(s.opt.PollWait)
	defer timer.Stop()
	for {
		events, next, expired, changed := s.events.read(d.ID, cursor)
		if expired {
			respond(w, 409, map[string]any{"error": map[string]string{"code": "cursor_expired"}, "cursor": next, "events": events})
			return
		}
		if cursor == "" || len(events) > 0 || r.URL.Query().Get("wait") == "0" {
			respond(w, 200, map[string]any{"apiVersion": 1, "cursor": next, "events": events})
			return
		}
		cursor = next
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			respond(w, 200, map[string]any{"apiVersion": 1, "cursor": next, "events": []domain.Event{}})
			return
		case <-changed:
		}
	}
}
