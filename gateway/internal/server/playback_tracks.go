package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/playback"
)

// Copy immutable fields before slow media work; never hold the server mutex
// while running a process. Only the resource cache is mutable on a session.
func (s *Server) ownedMediaSession(r *http.Request, device string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[r.PathValue("session")]
	if sess == nil || sess.device != device || time.Now().After(sess.expires) {
		return nil
	}
	copy := *sess
	return &copy
}

func (s *Server) sessionTracks(ctx context.Context, sess *session) (domain.TrackInventory, error) {
	if s.deps.Media == nil || sess.source.Path == "" || sess.source.Live {
		return domain.TrackInventory{Tracks: []domain.Track{}}, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(sess.ctx, cancel)
	defer stop()
	metadata, err := s.deps.Media.Probe(ctx, sess.source.Path)
	if err != nil {
		return domain.TrackInventory{}, err
	}
	return playback.Inventory(metadata, sess.selection.AudioID), nil
}

func mediaFailure(w http.ResponseWriter, err error) {
	if errors.Is(err, media.ErrBusy) {
		fail(w, 429, "media_busy")
	} else {
		fail(w, 502, "media_tracks_unavailable")
	}
}

func (s *Server) playbackTracks(w http.ResponseWriter, r *http.Request, d domain.Device) {
	sess := s.ownedMediaSession(r, d.ID)
	if sess == nil {
		fail(w, 404, "session_not_found")
		return
	}
	inventory, err := s.sessionTracks(r.Context(), sess)
	if err != nil {
		mediaFailure(w, err)
		return
	}
	respond(w, 200, inventory)
}

func (s *Server) playbackSubtitles(w http.ResponseWriter, r *http.Request, d domain.Device) {
	sess := s.ownedMediaSession(r, d.ID)
	if sess == nil {
		fail(w, 404, "session_not_found")
		return
	}
	index, err := strconv.Atoi(r.PathValue("track"))
	if err != nil || index < 0 {
		fail(w, 400, "invalid_track")
		return
	}
	inventory, err := s.sessionTracks(r.Context(), sess)
	if err != nil {
		mediaFailure(w, err)
		return
	}
	if !playback.HasTrack(inventory, index, "subtitle") {
		fail(w, 409, "subtitle_track_unavailable")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(sess.ctx, cancel)
	defer stop()
	cues, err := s.deps.Media.Subtitles(ctx, sess.source.Path, index)
	if err != nil {
		mediaFailure(w, err)
		return
	}
	payload := struct {
		Cues []domain.SubtitleCue `json:"cues"`
	}{cues}
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > 1<<20 {
		fail(w, 413, "subtitle_payload_limit")
		return
	}
	respond(w, 200, payload)
}

// Create a new plan so a failed selection does not stop the previous session.
// The client releases that session after adopting the replacement.
func (s *Server) selectAudio(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var request struct {
		AudioID    *int  `json:"audioId"`
		PositionMS int64 `json:"positionMs"`
	}
	if !decode(w, r, &request) {
		return
	}
	if request.AudioID == nil || *request.AudioID < 0 || request.PositionMS < 0 || request.PositionMS > 7*24*60*60*1000 {
		fail(w, 400, "invalid_track_selection")
		return
	}
	sess := s.ownedMediaSession(r, d.ID)
	if sess == nil {
		fail(w, 404, "session_not_found")
		return
	}
	inventory, err := s.sessionTracks(r.Context(), sess)
	if err != nil {
		mediaFailure(w, err)
		return
	}
	if !playback.HasTrack(inventory, *request.AudioID, "audio") {
		fail(w, 409, "audio_track_unavailable")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[r.PathValue("session")] == nil || sess.ctx.Err() != nil {
		fail(w, 404, "session_not_found")
		return
	}
	if len(s.sessions) >= 64 {
		fail(w, 429, "session_limit")
		return
	}
	id, ticket := randomID(16), randomID(24)
	ctx, cancel := context.WithDeadline(context.Background(), sess.expires)
	selection := domain.MediaSelection{AudioID: request.AudioID, PositionMS: request.PositionMS}
	s.sessions[id] = &session{
		mode:      "TRANSCODE",
		device:    d.ID,
		ticket:    ticket,
		expires:   sess.expires,
		source:    sess.source,
		ctx:       ctx,
		cancel:    cancel,
		selection: selection,
		resources: map[string]string{},
	}
	plan := domain.Plan{
		Version:          1,
		SessionID:        id,
		Mode:             "TRANSCODE",
		URL:              "/v1/streams/" + id + "?ticket=" + ticket,
		MIME:             "video/mp4",
		Seekable:         false,
		TimelineOffsetMS: request.PositionMS,
		Item:             sess.source.Item,
	}
	s.events.publish(d.ID, "playback.created", map[string]string{"sessionId": id})
	respond(w, 201, plan)
}
