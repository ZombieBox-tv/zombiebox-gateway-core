package server

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"
	"zombiebox.local/gateway/internal/domain"
)

type youtubeReceiver struct {
	id, device string
	config     domain.Config
	expires    time.Time
	busy       bool
	source     *domain.Source
}

var receiverIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var receiverVideoPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var tvCodePattern = regexp.MustCompile(`^[0-9][0-9 -]{4,30}$`)

func validReceiverState(value domain.YouTubeReceiverState, id string) bool {
	if value.ReceiverID != id || (value.TVCode != "" && !tvCodePattern.MatchString(value.TVCode)) {
		return false
	}
	switch value.State {
	case "STARTING", "WAITING", "READY", "UNAVAILABLE":
	default:
		return false
	}
	if c := value.Command; c != nil {
		if !receiverIDPattern.MatchString(c.ID) || c.PositionMS < 0 || c.PositionMS > 604800000 || c.Volume < 0 || c.Volume > 100 {
			return false
		}
		switch c.Action {
		case "play":
			return receiverVideoPattern.MatchString(c.VideoID)
		case "pause", "resume", "stop", "seek", "volume":
		default:
			return false
		}
	}
	return true
}
func (s *Server) startYouTubeReceiver(w http.ResponseWriter, r *http.Request, d domain.Device) {
	if s.deps.YouTubeReceiver == nil {
		fail(w, 503, "receiver_unavailable")
		return
	}
	s.mu.Lock()
	if s.youtubeReceiver != nil && (s.youtubeReceiver.busy || time.Now().Before(s.youtubeReceiver.expires)) {
		s.mu.Unlock()
		fail(w, 409, "receiver_in_use")
		return
	}
	c, youtube := s.config(r.Context(), "youtube_receiver"), s.config(r.Context(), "youtube")
	if !c.Enabled || !youtube.Enabled {
		s.mu.Unlock()
		fail(w, 409, "provider_disabled")
		return
	}
	owner := &youtubeReceiver{id: randomID(16), device: d.ID, config: c, expires: time.Now().Add(45 * time.Second), busy: true}
	s.youtubeReceiver = owner
	s.mu.Unlock()
	state, err := s.deps.YouTubeReceiver.OpenReceiver(r.Context(), c, owner.id)
	s.mu.Lock()
	owner.busy = false
	if err != nil || !validReceiverState(state, owner.id) {
		s.youtubeReceiver = nil
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.deps.YouTubeReceiver.CloseReceiver(ctx, c, owner.id)
		fail(w, 502, "receiver_unavailable")
		return
	}
	owner.expires = time.Now().Add(45 * time.Second)
	s.mu.Unlock()
	respond(w, 201, state)
}
func (s *Server) youTubeReceiverOperation(w http.ResponseWriter, r *http.Request, d domain.Device) {
	s.mu.Lock()
	owner := s.youtubeReceiver
	if owner == nil || owner.device != d.ID || owner.id != r.PathValue("receiver") || (r.Method != "DELETE" && time.Now().After(owner.expires)) {
		s.mu.Unlock()
		fail(w, 404, "receiver_not_found")
		return
	}
	if owner.busy {
		s.mu.Unlock()
		fail(w, 409, "receiver_busy")
		return
	}
	owner.busy = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); owner.busy = false; s.mu.Unlock() }()
	switch r.Method {
	case "DELETE":
		err := s.deps.YouTubeReceiver.CloseReceiver(r.Context(), owner.config, owner.id)
		s.mu.Lock()
		s.youtubeReceiver = nil
		s.mu.Unlock()
		if err != nil {
			fail(w, 502, "receiver_unavailable")
			return
		}
		respond(w, 200, map[string]bool{"closed": true})
	case "POST":
		var state domain.ReceiverAcknowledgement
		if !decode(w, r, &state) {
			return
		}
		if (state.CommandID != "" && !receiverIDPattern.MatchString(state.CommandID)) || state.PositionMS < 0 || state.DurationMS < 0 || state.PositionMS > 604800000 || state.DurationMS > 604800000 || state.Volume < 0 || state.Volume > 100 {
			fail(w, 400, "invalid_receiver_state")
			return
		}
		switch state.State {
		case "PLAYING", "PAUSED", "STOPPED", "ENDED", "BUFFERING", "FAILED":
		default:
			fail(w, 400, "invalid_receiver_state")
			return
		}
		if s.deps.YouTubeReceiver.AcknowledgeReceiver(r.Context(), owner.config, owner.id, state) != nil {
			fail(w, 502, "receiver_unavailable")
			return
		}
		respond(w, 200, map[string]bool{"accepted": true})
	case "GET":
		state, err := s.deps.YouTubeReceiver.PollReceiver(r.Context(), owner.config, owner.id)
		if err != nil || !validReceiverState(state, owner.id) {
			fail(w, 502, "receiver_unavailable")
			return
		}
		s.mu.Lock()
		owner.expires = time.Now().Add(45 * time.Second)
		if command := state.Command; command != nil && command.Action == "play" {
			c := s.config(r.Context(), "youtube")
			if !c.Enabled {
				s.mu.Unlock()
				fail(w, 409, "provider_disabled")
				return
			}
			source := domain.Source{Item: domain.Item{ID: "youtube-" + command.VideoID, Provider: "youtube", Kind: "video", Title: "YouTube", Playable: true}, URL: strings.TrimRight(c.URL, "/") + "/resolve/" + command.VideoID, Headers: http.Header{"Authorization": {"Bearer " + c.Token}}, MIME: "application/x-zombie-youtube"}
			owner.source = &source
			command.ItemID = source.Item.ID
		}
		s.mu.Unlock()
		respond(w, 200, state)
	}
}
func (s *Server) youTubeReceiverSource(device, id string) *domain.Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.youtubeReceiver
	if owner == nil || owner.device != device || time.Now().After(owner.expires) || owner.source == nil || owner.source.Item.ID != id {
		return nil
	}
	copy := *owner.source
	return &copy
}
