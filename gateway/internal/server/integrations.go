package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/integrations"
	"zombiebox.local/gateway/internal/providers"
)

type integrationStatus struct {
	integrations.Definition
	State string `json:"state"`
}

// Availability is a bounded, on-demand process check. READY does not mean a
// provider account or an end-to-end playback path has passed validation.
func (s *Server) integrationList(w http.ResponseWriter, r *http.Request, d domain.Device) {
	select {
	case s.integrationChecks <- struct{}{}:
		defer func() { <-s.integrationChecks }()
	default:
		fail(w, 429, "integration_check_busy")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	definitions := integrations.All()
	out := make([]integrationStatus, len(definitions))
	jobs := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, definition := range definitions {
		out[i] = integrationStatus{Definition: definition, State: "DISABLED"}
		if definition.Implementation == "pending" {
			out[i].State = "NOT_IMPLEMENTED"
			continue
		}
		c := s.config(ctx, definition.Provider)
		var target string
		headers := http.Header{}
		switch definition.ID {
		case "local":
			out[i].State = "READY"
			continue
		case "ffmpeg":
			if s.opt.MediaTools != nil {
				out[i].State = "READY"
			}
			continue
		case "mediamtx":
			if s.opt.RelayControlURL == "" {
				continue
			}
			target = strings.TrimRight(s.opt.RelayControlURL, "/") + "/v3/paths/list"
		case "threadfin":
			if s.opt.ThreadfinURL != "" {
				target = strings.TrimRight(s.opt.ThreadfinURL, "/") + "/discover.json"
				break
			}
			if !c.Enabled || !strings.Contains(c.URL, "/m3u/threadfin.m3u") {
				continue
			}
			target = c.URL
		default:
			if !c.Enabled {
				continue
			}
			target = c.URL
			if definition.ID == "youtube" || definition.ID == "rebrowser" || definition.ID == "spotify" {
				target = strings.TrimRight(target, "/") + "/health"
			}
			if definition.ID == "airplay" {
				target = strings.TrimRight(target, "/") + "/status"
			}
			if c.Token != "" {
				headers.Set("Authorization", "Bearer "+c.Token)
			}
		}
		if target == "" {
			out[i].State = "CONFIGURED"
			continue
		}
		wg.Add(1)
		go func(i int, target string, headers http.Header) {
			defer wg.Done()
			out[i].State = "UNAVAILABLE"
			select {
			case jobs <- struct{}{}:
				defer func() { <-jobs }()
			case <-ctx.Done():
				return
			}
			req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
			if err != nil {
				return
			}
			req.Header = headers
			if out[i].ID == "mediamtx" {
				req.SetBasicAuth("gateway", s.opt.RelayAdminToken)
			}
			client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, err := client.Do(req)
			if err != nil {
				return
			}
			defer res.Body.Close()
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				out[i].State = "READY"
			} else if res.StatusCode == 401 || res.StatusCode == 403 {
				out[i].State = "AUTH_REQUIRED"
			}
		}(i, target, headers)
	}
	wg.Wait()
	respond(w, 200, map[string]any{"apiVersion": 1, "integrations": out})
}

func (s *Server) nowPlaying(w http.ResponseWriter, r *http.Request, d domain.Device) {
	c := s.config(r.Context(), "spotify")
	if !c.Enabled {
		fail(w, 409, "provider_disabled")
		return
	}
	status, err := providers.SpotifyStatus(r.Context(), c)
	if err != nil {
		fail(w, 502, "player_unavailable")
		return
	}
	respond(w, 200, status)
}

func (s *Server) spotifyAuthorization(w http.ResponseWriter, r *http.Request, d domain.Device) {
	if !s.admin(w, r) {
		return
	}
	c := s.config(r.Context(), "spotify")
	if !c.Enabled {
		fail(w, 409, "provider_disabled")
		return
	}
	prompt, err := providers.SpotifyAuthorization(r.Context(), c)
	if err != nil {
		fail(w, 502, "authorization_unavailable")
		return
	}
	respond(w, 200, prompt)
}

func (s *Server) playerCommand(w http.ResponseWriter, r *http.Request, d domain.Device) {
	// This controls a shared Connect device. Require the operator code until a
	// per-receiver ownership lease is implemented; pairing alone is insufficient.
	if !s.admin(w, r) {
		return
	}
	var command providers.PlayerCommand
	if !decode(w, r, &command) {
		return
	}
	valid := map[string]bool{"pause": true, "resume": true, "next": true, "previous": true, "stop": true, "seek": true, "volume": true}
	if !valid[command.Action] || command.PositionMS < 0 || command.PositionMS > 604800000 || command.Volume < 0 || command.Volume > 100 {
		fail(w, 400, "invalid_player_command")
		return
	}
	c := s.config(r.Context(), "spotify")
	if !c.Enabled {
		fail(w, 409, "provider_disabled")
		return
	}
	if providers.SpotifyCommand(r.Context(), c, command) != nil {
		fail(w, 502, "player_unavailable")
		return
	}
	s.mu.Lock()
	delete(s.catalogCache, "spotify")
	s.mu.Unlock()
	s.events.publish("", "player.changed", map[string]string{"provider": "spotify"})
	respond(w, 200, map[string]bool{"accepted": true})
}
