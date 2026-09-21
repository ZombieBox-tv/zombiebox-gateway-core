package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

type browserSession struct {
	id, device string
	config     providers.Config
	touched    time.Time
	busy       bool
}

func (s *Server) startBrowser(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var request struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &request) {
		return
	}
	u, err := url.Parse(request.URL)
	if err != nil || len(request.URL) > 2048 || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		fail(w, 400, "invalid_browser_url")
		return
	}
	c := s.config(r.Context(), "rebrowser")
	if !c.Enabled {
		fail(w, 409, "provider_disabled")
		return
	}
	s.mu.Lock()
	if s.browser != nil && time.Since(s.browser.touched) > 90*time.Second && !s.browser.busy {
		s.browser = nil
	}
	if s.browser != nil {
		s.mu.Unlock()
		fail(w, 409, "browser_busy")
		return
	}
	session := &browserSession{id: randomID(16), device: d.ID, config: c, touched: time.Now(), busy: true}
	s.browser = session
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, err = s.deps.Browser.BrowserRequest(ctx, c, "POST", "/session", map[string]string{"id": session.id, "url": request.URL})
	s.mu.Lock()
	session.busy = false
	session.touched = time.Now()
	if err != nil {
		s.browser = nil
	}
	s.mu.Unlock()
	if err != nil {
		fail(w, 502, "browser_unavailable")
		return
	}
	respond(w, 201, map[string]any{"sessionId": session.id, "width": 960, "height": 540, "idleSeconds": 90})
}

func (s *Server) browserOperation(w http.ResponseWriter, r *http.Request, d domain.Device) {
	s.mu.Lock()
	session := s.browser
	if session == nil || session.id != r.PathValue("browser") || session.device != d.ID || time.Since(session.touched) > 90*time.Second {
		s.mu.Unlock()
		fail(w, 404, "browser_not_found")
		return
	}
	if session.busy {
		s.mu.Unlock()
		fail(w, 429, "browser_busy")
		return
	}
	session.busy = true
	session.touched = time.Now()
	s.mu.Unlock()
	defer func() { s.mu.Lock(); session.busy = false; session.touched = time.Now(); s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	path := "/session/" + session.id
	var body any
	if r.Method == "GET" {
		path += "/frame"
	}
	if r.Method == "POST" {
		var command struct {
			Action string `json:"action"`
			Text   string `json:"text"`
			X      *int   `json:"x,omitempty"`
			Y      *int   `json:"y,omitempty"`
		}
		if !decode(w, r, &command) {
			return
		}
		allowed := map[string]bool{"navigate": true, "text": true, "key": true, "back": true, "forward": true, "reload": true, "click": true, "move": true, "scroll": true}
		if !allowed[command.Action] || len(command.Text) > 2048 {
			fail(w, 400, "invalid_browser_command")
			return
		}
		if command.Action == "click" || command.Action == "move" || command.Action == "scroll" {
			if command.X == nil || command.Y == nil {
				fail(w, 400, "invalid_pointer")
				return
			}
			x, y := *command.X, *command.Y
			valid := x >= 0 && x < 960 && y >= 0 && y < 540
			if command.Action == "scroll" {
				valid = x >= -960 && x <= 960 && y >= -540 && y <= 540
			}
			if !valid {
				fail(w, 400, "invalid_pointer")
				return
			}
		}
		body = command
		path += "/input"
	}
	data, err := s.deps.Browser.BrowserRequest(ctx, session.config, r.Method, path, body)
	if errors.Is(err, domain.ErrNotFound) {
		s.mu.Lock()
		if s.browser == session {
			s.browser = nil
		}
		s.mu.Unlock()
		fail(w, 410, "browser_expired")
		return
	}
	if err != nil {
		fail(w, 502, "browser_unavailable")
		return
	}
	if r.Method == "DELETE" {
		s.mu.Lock()
		if s.browser == session {
			s.browser = nil
		}
		s.mu.Unlock()
		respond(w, 200, map[string]bool{"closed": true})
		return
	}
	if r.Method == "GET" {
		if len(data) < 3 || data[0] != 0xff || data[1] != 0xd8 || data[2] != 0xff {
			fail(w, 502, "invalid_browser_frame")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
		return
	}
	respond(w, 200, map[string]bool{"accepted": true})
}
