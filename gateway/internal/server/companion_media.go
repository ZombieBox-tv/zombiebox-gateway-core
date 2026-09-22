package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"zombiebox.local/gateway/internal/companion"
	"zombiebox.local/gateway/internal/companionmedia"
	"zombiebox.local/gateway/internal/domain"
)

func (s *Server) companionMediaUpload(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	if s.deps.Uploads == nil || s.deps.Media == nil {
		fail(w, 503, "media_upload_disabled")
		return
	}
	if !companionmedia.ID.MatchString(r.PathValue("media")) || r.ContentLength < 1 || r.ContentLength > companionmedia.MaxBytes {
		fail(w, 413, "invalid_media_size")
		return
	}
	// Override the short JSON deadline only for this bounded upload body.
	deadline := time.Now().Add(5 * time.Minute)
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	_ = http.NewResponseController(w).SetWriteDeadline(deadline.Add(10 * time.Second))
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	asset, err := s.deps.Uploads.Put(ctx, g.ID, r.PathValue("media"), r.ContentLength, r.Body)
	if err != nil {
		if errors.Is(err, companionmedia.ErrBusy) {
			fail(w, 429, "media_upload_busy")
		} else {
			fail(w, 400, "invalid_media_upload")
		}
		return
	}
	s.receiverClaims.Lock()
	defer s.receiverClaims.Unlock()
	if !s.companions.Active(ctx, g.ID, g.TargetID) || ctx.Err() != nil {
		s.deps.Uploads.Remove(g.ID, asset.ID)
		fail(w, 403, "companion_revoked")
		return
	}
	respond(w, 201, map[string]any{"mediaId": asset.ID, "state": "UPLOADED"})
}

// Upload, preparation and handoff are distinct. The old receiver is retained if
// probing, consent, ownership or capacity checks fail. A phone cannot select a TV
// other than the target bound to its revocable companion grant.
func (s *Server) companionMediaPlay(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	if s.deps.Uploads == nil || s.deps.Media == nil {
		fail(w, 503, "media_upload_disabled")
		return
	}
	var request struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &request) {
		return
	}
	title := strings.TrimSpace(request.Title)
	if len([]rune(title)) > 120 || strings.IndexFunc(title, unicode.IsControl) >= 0 {
		fail(w, 400, "invalid_media_title")
		return
	}
	if title == "" {
		title = "Shared media"
	}
	if s.mediaQueue.Busy() {
		fail(w, 409, "media_queue_active")
		return
	}
	status, state := s.startCompanionMedia(r.Context(), g, r.PathValue("media"), title)
	if status != 200 {
		fail(w, status, state)
		return
	}
	respond(w, 200, map[string]string{"mediaId": r.PathValue("media"), "state": state})
}

// Serialized preparation is shared by single files and the gateway-owned URL queue.
func (s *Server) startCompanionMedia(ctx context.Context, g companion.Grant, mediaID, title string) (int, string) {
	s.receiverClaims.Lock()
	defer s.receiverClaims.Unlock()
	// Idempotent start: a lost HTTP response must not allocate a second session.
	s.mu.Lock()
	for _, c := range s.casts {
		if c.mediaID == mediaID && c.sender == "companion-"+g.ID && c.plan != nil && time.Now().Before(c.expires) {
			s.mu.Unlock()
			return 200, "ACCEPTED"
		}
	}
	s.mu.Unlock()
	asset, err := s.deps.Uploads.Get(g.ID, mediaID)
	if err != nil {
		return 404, "media_not_found"
	}
	if !s.companions.Active(ctx, g.ID, g.TargetID) {
		return 403, "companion_revoked"
	}
	var target domain.Device
	if s.db.Get(ctx, "devices", g.TargetID, &target) != nil || !target.Preferences.AllowCasting {
		return 409, "receiver_unavailable"
	}
	source := domain.Source{Path: asset.Path, MIME: asset.MIME, Item: domain.Item{ID: "phone-" + asset.ID, Provider: "android_mirror", Kind: asset.Kind, Title: title, Playable: true}}
	decision, err := s.playbackMode(ctx, source, target, "AUTO")
	if err != nil || decision.metadata == nil || len(decision.metadata.Streams) == 0 {
		return 422, "media_probe_failed"
	}
	audio, video := false, false
	for _, stream := range decision.metadata.Streams {
		audio = audio || stream.Type == "audio"
		video = video || stream.Type == "video"
	}
	if !audio && !video || decision.mode == "EXTERNAL_PLAYER" {
		return 422, "media_unsupported"
	}
	if !video {
		source.Item.Kind = "audio"
		if source.MIME == "video/mp4" {
			source.MIME = "audio/mp4"
		}
	}
	if video {
		source.Item.Kind = "video"
	}
	quality := s.networkQuality(target, decision)
	if quality != "" {
		decision.mode = "TRANSCODE"
	}
	busy := s.receiverBusy(target.ID, "cast")
	// Consent/settings may change while FFprobe was running; use current values.
	if s.db.Get(ctx, "devices", g.TargetID, &target) != nil || !target.Preferences.AllowCasting || (busy && !target.Preferences.AllowReceiverHandoff) || !s.companions.Active(ctx, g.ID, g.TargetID) || ctx.Err() != nil {
		return 409, "receiver_unavailable"
	}
	s.mu.Lock()
	if time.Since(s.seen[target.ID]) > 45*time.Second || len(s.casts) > 0 || len(s.sessions) >= 64 {
		s.mu.Unlock()
		return 409, "receiver_busy"
	}
	if s.deps.Uploads.Retain(g.ID, asset.ID) != nil {
		s.mu.Unlock()
		return 404, "media_not_found"
	}
	id, ticket, castID := randomID(16), randomID(24), randomID(16)
	expires := time.Now().Add(6 * time.Hour)
	sessionCtx, cancel := context.WithDeadline(context.Background(), expires)
	s.sessions[id] = &session{device: target.ID, castID: castID, ticket: ticket, source: source, mode: decision.mode, metadata: decision.metadata, subtitleID: decision.subtitleID, selection: domain.MediaSelection{AudioID: decision.audioID, Quality: quality}, expires: expires, ctx: sessionCtx, cancel: cancel, resources: map[string]string{}}
	plan := &domain.Plan{Version: 1, SessionID: id, Mode: decision.mode, URL: "/v1/streams/" + id + "?ticket=" + ticket, MIME: source.MIME, Seekable: true, Item: source.Item, SubtitleID: decision.subtitleID}
	if decision.mode == "REMUX" || decision.mode == "TRANSCODE" {
		plan.MIME = "video/mp4"
		plan.Seekable = false
	}
	c := &castSession{id: castID, mediaID: asset.ID, mode: "MEDIA", sender: "companion-" + g.ID, receiver: target.ID, expires: expires}
	s.casts[castID] = c
	s.mu.Unlock()
	s.retireReceivers(ctx, target.ID, "cast")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.casts[castID] != c {
		cancel()
		delete(s.sessions, id)
		return 409, "receiver_changed"
	}
	c.plan = plan
	s.events.publish(target.ID, "receiver.changed", map[string]string{"transport": "cast"})
	s.events.publish(target.ID, "cast.started", plan)
	return 200, "ACCEPTED"
}
func (s *Server) companionMediaDelete(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	s.receiverClaims.Lock()
	defer s.receiverClaims.Unlock()
	s.mu.Lock()
	for _, c := range s.casts {
		if c.sender == "companion-"+g.ID && c.mediaID == r.PathValue("media") {
			s.endCastLocked(c)
		}
	}
	s.mu.Unlock()
	if s.deps.Uploads != nil {
		s.deps.Uploads.Remove(g.ID, r.PathValue("media"))
	}
	respond(w, 200, map[string]string{"state": "STOPPED"})
}

func (s *Server) companionMediaStatus(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.casts {
		if c.sender == "companion-"+g.ID && c.mediaID != "" && c.plan != nil && time.Now().Before(c.expires) {
			respond(w, 200, map[string]string{"state": "ACCEPTED", "mediaId": c.mediaID, "title": c.plan.Item.Title})
			return
		}
	}
	respond(w, 200, map[string]string{"state": "NONE"})
}
