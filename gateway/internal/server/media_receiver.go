package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/playback"
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

func (a receiverAdapter) Start(ctx context.Context, device string, source domain.Source) (domain.Plan, error) {
	s := a.server
	s.mu.Lock()
	for id, x := range s.sessions {
		if time.Now().After(x.expires) {
			x.cancel()
			delete(s.sessions, id)
		}
	}
	if len(s.sessions) >= 64 {
		s.mu.Unlock()
		return domain.Plan{}, inbox.ErrBusy
	}
	s.mu.Unlock()

	var d domain.Device
	if err := s.db.Get(ctx, "devices", device, &d); err != nil {
		return domain.Plan{}, err
	}

	decision, err := s.playbackMode(ctx, source, d, "")
	if err != nil {
		return domain.Plan{}, err
	}
	mode := decision.mode
	if media.ManifestKind(source) == "hls" && mode == "DIRECT_PLAY" && !playback.HasFreshHLSEvidence(d.Capabilities) {
		mode = "REMUX"
	}
	if mode != "DIRECT_PLAY" && mode != "REMUX" && mode != "TRANSCODE" {
		return domain.Plan{}, errors.New("unsupported playback mode")
	}
	if (mode == "REMUX" || mode == "TRANSCODE") && ((source.Path != "" && s.deps.Media == nil) || (source.Path == "" && (s.deps.RemoteMedia == nil || !media.RemoteCandidate(source)))) {
		return domain.Plan{}, errors.New("conversion unavailable")
	}

	mime := source.MIME
	if mode == "REMUX" || mode == "TRANSCODE" {
		mime = "video/mp4"
		if isAudioOnly(source, decision.metadata) {
			mime = "audio/mp4"
		}
		if media.LiveAACRemux(source, decision.metadata, mode) {
			mime = "audio/aac"
		}
	}
	item := source.Item
	if isAudioOnly(source, decision.metadata) && item.Kind == "" {
		item.Kind = "audio"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, x := range s.sessions {
		if time.Now().After(x.expires) {
			x.cancel()
			delete(s.sessions, id)
		}
	}
	if len(s.sessions) >= 64 {
		return domain.Plan{}, inbox.ErrBusy
	}
	id, ticket := randomID(16), randomID(24)
	sessCtx, cancel := context.WithCancel(context.Background())
	s.sessions[id] = &session{
		device:    device,
		ticket:    ticket,
		source:    source,
		mode:      mode,
		metadata:  decision.metadata,
		selection: domain.MediaSelection{AudioID: decision.audioID},
		expires:   time.Now().Add(24 * time.Hour),
		ctx:       sessCtx,
		cancel:    cancel,
		resources: map[string]string{},
	}
	return domain.Plan{
		Version:   1,
		SessionID: id,
		Mode:      mode,
		URL:       "/v1/streams/" + id + "?ticket=" + ticket,
		MIME:      mime,
		Live:      true,
		Seekable:  false,
		Item:      item,
	}, nil
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
		if request.Provider != "spotify" && request.Provider != "airplay" && request.Provider != "auto" && request.Provider != "universal" {
			fail(w, 400, "invalid_provider")
			return
		}
		s.receiverClaims.Lock()
		defer s.receiverClaims.Unlock()
		if request.Provider == "universal" && (!d.Preferences.AllowReceiverHandoff || !d.Preferences.AllowCasting) {
			fail(w, 409, "receiver_handoff_disabled")
			return
		}
		if request.Provider != "universal" && s.receiverBusy(d.ID, "media") && !request.ReplaceExisting {
			fail(w, 409, "receiver_busy")
			return
		}
		if err := s.mediaReceiverInbox.Claim(d.ID, request.Provider); err != nil {
			fail(w, 409, "receiver_busy")
			return
		}
		if request.Provider != "universal" {
			s.retireReceivers(r.Context(), d.ID, "media")
		}
		s.events.publish(d.ID, "receiver.changed", map[string]string{"transport": "media"})
		respond(w, 200, map[string]any{"enabled": true, "provider": request.Provider})
		return
	}
	s.receiverClaims.Lock()
	defer s.receiverClaims.Unlock()
	snapshot, err := s.mediaReceiverInbox.Snapshot(r.Context(), d.ID)
	if err != nil {
		if errors.Is(err, inbox.ErrBusy) || errors.Is(err, inbox.ErrChanged) {
			fail(w, 409, "receiver_busy")
		} else {
			fail(w, 502, "receiver_unavailable")
		}
		return
	}
	if snapshot.Provider == "universal" {
		if !d.Preferences.AllowReceiverHandoff || !d.Preferences.AllowCasting {
			s.mediaReceiverInbox.Release(d.ID)
			s.youtubeReceiver.Revoke(r.Context(), d.ID)
			fail(w, 409, "receiver_handoff_disabled")
			return
		}
		if snapshot.Plan != nil && s.receiverHasPlayback(d.ID, "media") {
			s.retireReceivers(r.Context(), d.ID, "media")
		}
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
