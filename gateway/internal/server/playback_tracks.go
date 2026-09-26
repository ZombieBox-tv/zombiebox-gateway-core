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
	if err := sess.ctx.Err(); err != nil {
		return domain.TrackInventory{}, err
	}
	if sess.source.Live || sess.source.AudioURL != "" {
		return domain.TrackInventory{Tracks: []domain.Track{}}, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(sess.ctx, cancel)
	defer stop()
	metadata := sess.metadata
	if metadata == nil {
		var value domain.Metadata
		var err error
		if sess.source.Path != "" && s.deps.Media != nil {
			value, err = s.deps.Media.Probe(ctx, sess.source.Path)
		} else if s.deps.RemoteMedia != nil && media.RemoteCandidate(sess.source) {
			value, err = s.deps.RemoteMedia.ProbeRemote(ctx, sess.source)
		} else {
			return domain.TrackInventory{Tracks: []domain.Track{}}, nil
		}
		if err != nil {
			return domain.TrackInventory{}, err
		}
		metadata = &value
	}
	if err := ctx.Err(); err != nil {
		return domain.TrackInventory{}, err
	}
	// ownedMediaSession returns a request-local copy. Retain this probe for the
	// selection decision without a second process or mutating the shared session.
	sess.metadata = metadata
	inventory := playback.Inventory(playback.WithSubtitles(*metadata, sess.source), sess.selection.AudioID)
	inventory.SubtitleID = sess.subtitleID
	for index := range inventory.Tracks {
		track := &inventory.Tracks[index]
		if sess.source.Path != "" {
			track.Selectable = track.Selectable && s.deps.Media != nil
		} else if track.Kind == "audio" {
			track.Selectable = track.Selectable && s.deps.RemoteMedia != nil && media.RemoteCandidate(sess.source)
		} else {
			track.Selectable = track.Selectable && s.deps.RemoteSubtitles != nil
		}
	}
	return inventory, nil
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
	var cues []domain.SubtitleCue
	if sess.source.Path != "" {
		cues, err = s.deps.Media.Subtitles(ctx, sess.source.Path, index)
	} else {
		cues, err = s.deps.RemoteSubtitles.SubtitlesRemote(ctx, sess.source, index)
	}
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
	selection := domain.MediaSelection{AudioID: request.AudioID, PositionMS: request.PositionMS, Quality: sess.selection.Quality}
	mode := playback.SelectedAudioMode(*sess.metadata, sess.source.MIME, d.Capabilities, selection, sess.mode)
	if mode == "EXTERNAL_PLAYER" {
		fail(w, 409, "audio_track_unavailable")
		return
	}
	if (mode == "REMUX" || mode == "HYBRID") && (request.PositionMS > 0 || sess.selection.Quality == "LOW" || (sess.metadata != nil && playback.RequiresTranscodeForQuality(*sess.metadata, sess.selection.Quality))) {
		mode = "TRANSCODE"
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
	s.sessions[id] = &session{
		networkAdaptation: sess.networkAdaptation,
		adaptation:        sess.adaptation,
		mode:              mode,
		device:            d.ID,
		ticket:            ticket,
		expires:           sess.expires,
		source:            sess.source,
		metadata:          sess.metadata,
		subtitleID:        sess.subtitleID,
		ctx:               ctx,
		cancel:            cancel,
		selection:         selection,
		resources:         map[string]string{},
	}
	mime := "video/mp4"
	if isAudioOnly(sess.source, sess.metadata) {
		mime = "audio/mp4"
	}
	seekable := false
	if mode == "HYBRID" {
		seekable = !sess.source.Live
	}
	plan := domain.Plan{
		Version:          1,
		SubtitleID:       sess.subtitleID,
		SessionID:        id,
		Mode:             mode,
		URL:              "/v1/streams/" + id + "?ticket=" + ticket,
		MIME:             mime,
		Seekable:         seekable,
		TimelineOffsetMS: request.PositionMS,
		Item:             sess.source.Item,
	}
	s.events.publish(d.ID, "playback.created", map[string]string{"sessionId": id})
	respond(w, 201, plan)
}
