package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/receivers/inbox"
)

// Adapter joins the feature's ports to injected providers and owned stream sessions.
type receiverAdapter struct{ server *Server }

func (a receiverAdapter) Read(ctx context.Context, provider string) (*domain.Source, domain.NowPlaying, error) {
	s := a.server
	s.mu.Lock()
	config, revision := s.config(ctx, provider), s.configRevision[provider]
	s.mu.Unlock()
	if !config.Enabled {
		return nil, domain.NowPlaying{Provider: provider, State: "DISABLED"}, nil
	}
	if s.deps.Reception == nil {
		return nil, domain.NowPlaying{}, errors.New("receiver unavailable")
	}
	selected, status, err := s.deps.Reception.Reception(ctx, provider, config)
	if err != nil {
		return nil, status, err
	}
	s.mu.Lock()
	unchanged := revision == s.configRevision[provider]
	s.mu.Unlock()
	if !unchanged {
		return nil, status, inbox.ErrChanged
	}
	if selected != nil {
		sources := []domain.Source{*selected}
		decorateArtwork(sources)
		selected = &sources[0]
		status.Item = &selected.Item
	}
	return selected, status, nil
}

func (a receiverAdapter) Start(device string, source domain.Source) (domain.Plan, error) {
	s := a.server
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= 64 {
		return domain.Plan{}, inbox.ErrBusy
	}
	id, ticket := randomID(16), randomID(24)
	ctx, cancel := context.WithCancel(context.Background())
	s.sessions[id] = &session{device: device, ticket: ticket, source: source, expires: time.Now().Add(24 * time.Hour), ctx: ctx, cancel: cancel, resources: map[string]string{}}
	return domain.Plan{Version: 1, SessionID: id, Mode: "DIRECT_PLAY", URL: "/v1/streams/" + id + "?ticket=" + ticket, MIME: source.MIME, Live: true, Seekable: false, Item: source.Item}, nil
}
func (a receiverAdapter) Stop(id string) {
	s := a.server
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.sessions[id]; session != nil {
		session.cancel()
		delete(s.sessions, id)
	}
}

func (s *Server) mediaReceiver(w http.ResponseWriter, r *http.Request, d domain.Device) {
	if r.Method == "DELETE" {
		s.mediaReceiverInbox.Release(d.ID)
		respond(w, 200, inbox.Snapshot{})
		return
	}
	if r.Method == "PUT" {
		var request struct {
			Provider        string `json:"provider"`
			ReplaceExisting bool   `json:"replaceExisting"`
		}
		if !decode(w, r, &request) {
			return
		}
		if request.Provider != "spotify" && request.Provider != "airplay" && request.Provider != "auto" {
			fail(w, 400, "invalid_provider")
			return
		}
		s.receiverClaims.Lock()
		defer s.receiverClaims.Unlock()
		if s.receiverBusy(d.ID, "media") && !request.ReplaceExisting {
			fail(w, 409, "receiver_busy")
			return
		}
		if err := s.mediaReceiverInbox.Claim(d.ID, request.Provider); err != nil {
			fail(w, 409, "receiver_busy")
			return
		}
		s.retireReceivers(r.Context(), d.ID, "media")
		s.events.publish(d.ID, "receiver.changed", map[string]string{"transport": "media"})
		respond(w, 200, map[string]any{"enabled": true, "provider": request.Provider})
		return
	}
	snapshot, err := s.mediaReceiverInbox.Snapshot(r.Context(), d.ID)
	if err != nil {
		if errors.Is(err, inbox.ErrBusy) || errors.Is(err, inbox.ErrChanged) {
			fail(w, 409, "receiver_busy")
		} else {
			fail(w, 502, "receiver_unavailable")
		}
		return
	}
	respond(w, 200, snapshot)
}

func (s *Server) reapMediaReceiver() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mediaReceiverInbox.Sweep()
		}
	}
}
