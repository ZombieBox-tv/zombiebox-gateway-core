package server

import (
	"context"
	"net/http"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/playback"
)

const qualityPrefBucket = "quality_preferences"

type qualityPreference struct {
	QualityID string `json:"qualityId"`
	Kind      string `json:"kind"`
	UpdatedAt int64  `json:"updatedAt"`
}

func (s *Server) getQualityPreference(ctx context.Context, deviceID, kind string) string {
	if deviceID == "" || kind == "" {
		return ""
	}
	var pref qualityPreference
	if err := s.db.Get(ctx, qualityPrefBucket+":"+deviceID, kind, &pref); err == nil {
		return pref.QualityID
	}
	return ""
}

func (s *Server) setQualityPreference(ctx context.Context, deviceID, kind, qualityID string) error {
	if deviceID == "" || kind == "" {
		return nil
	}
	return s.db.Put(ctx, qualityPrefBucket+":"+deviceID, kind, qualityPreference{
		QualityID: qualityID,
		Kind:      kind,
		UpdatedAt: time.Now().Unix(),
	})
}

func (s *Server) revertQualityPreference(ctx context.Context, deviceID, kind string) error {
	if deviceID == "" || kind == "" {
		return nil
	}
	return s.db.Put(ctx, qualityPrefBucket+":"+deviceID, kind, qualityPreference{
		QualityID: "auto",
		Kind:      kind,
		UpdatedAt: time.Now().Unix(),
	})
}

// revertFailedSessionQualityLocked only forgets a manual preference when the
// session using it actually fails. A superseded stream can report a late
// failure while its replacement is already playing at the new quality.
// The caller holds s.mu so session replacement and this check are ordered.
func (s *Server) revertFailedSessionQualityLocked(ctx context.Context, id string, sess *session) error {
	if sess == nil || s.sessions[id] != sess || sess.supersededBy != "" || sess.ctx.Err() != nil {
		return nil
	}
	kind := sess.source.Item.Kind
	if kind == "" {
		kind = "video"
	}
	selected := sess.selection.Quality
	if selected == "LOW" {
		selected = "240p"
	} else if selected == "STANDARD" {
		selected = "360p"
	}
	preference := s.getQualityPreference(ctx, sess.device, kind)
	if preference == "" || preference == "auto" || preference != selected {
		return nil
	}
	return s.revertQualityPreference(ctx, sess.device, kind)
}

func (s *Server) cleanupSupersededSessionsLocked(deviceID, activeOldID string) {
	for sid, sess := range s.sessions {
		if sess.device != deviceID || sid == activeOldID {
			continue
		}
		if sess.supersededBy != "" || sess.ctx.Err() != nil {
			sess.cancel()
			delete(s.sessions, sid)
			s.events.publish(deviceID, "playback.stopped", map[string]string{"sessionId": sid})
		}
	}
}

func isManualYouTubeSession(sess *session) bool {
	if sess == nil || sess.source.Item.Provider != "youtube" {
		return false
	}
	if sess.selection.Quality != "" && sess.selection.Quality != "auto" {
		return true
	}
	if sess.source.ResolveQuality != "" && sess.source.ResolveQuality != "auto" {
		return true
	}
	if sess.source.AudioURL != "" {
		return true
	}
	return false
}

func (s *Server) probeRemoteMedia(ctx context.Context, src domain.Source) (domain.Metadata, bool) {
	if s.deps.RemoteMedia == nil || !media.RemoteCandidate(src) {
		return domain.Metadata{}, false
	}
	meta, err := s.deps.RemoteMedia.ProbeRemote(ctx, src)
	if err != nil {
		return domain.Metadata{}, false
	}
	hasVideo, hasAudio := false, false
	for _, stream := range meta.Streams {
		hasVideo = hasVideo || stream.Type == "video"
		hasAudio = hasAudio || stream.Type == "audio"
	}
	if !hasVideo || !hasAudio {
		return domain.Metadata{}, false
	}
	return meta, true
}

func (s *Server) resolveAndProbeYouTubeSource(ctx context.Context, base domain.Source, quality string) (domain.Source, domain.Metadata, bool) {
	if s.deps.Resolver == nil {
		return domain.Source{}, domain.Metadata{}, false
	}
	req := base
	req.ResolveQuality = quality
	resolved, err := s.deps.Resolver.Resolve(ctx, req)
	if err != nil {
		return domain.Source{}, domain.Metadata{}, false
	}
	meta, ok := s.probeRemoteMedia(ctx, resolved)
	if !ok {
		return domain.Source{}, domain.Metadata{}, false
	}
	return resolved, meta, true
}

func (s *Server) playbackQualities(w http.ResponseWriter, r *http.Request, d domain.Device) {
	sess := s.ownedMediaSession(r, d.ID)
	if sess == nil {
		fail(w, 404, "session_not_found")
		return
	}
	metadata := sess.metadata
	if metadata == nil {
		var value domain.Metadata
		var err error
		if sess.source.Path != "" && s.deps.Media != nil {
			value, err = s.deps.Media.Probe(r.Context(), sess.source.Path)
		} else if s.deps.RemoteMedia != nil && media.RemoteCandidate(sess.source) {
			value, err = s.deps.RemoteMedia.ProbeRemote(r.Context(), sess.source)
		}
		if err != nil {
			respond(w, 200, domain.QualityInventory{
				SelectedID: "auto",
				Options:    []domain.QualityOption{{ID: "auto", Label: "Auto"}},
			})
			return
		}
		metadata = &value
		sess.metadata = metadata
	}

	inventory := playback.Qualities(*metadata, sess.source, d, sess.selection.Quality)
	respond(w, 200, inventory)
}

func (s *Server) selectQuality(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var request struct {
		QualityID  string `json:"qualityId"`
		Quality    string `json:"quality"`
		PositionMS int64  `json:"positionMs"`
	}
	if !decode(w, r, &request) {
		return
	}
	if request.QualityID == "" && request.Quality != "" {
		request.QualityID = request.Quality
	}
	if request.QualityID == "" {
		request.QualityID = "auto"
	}
	if request.PositionMS < 0 || request.PositionMS > 7*24*60*60*1000 {
		fail(w, 400, "invalid_position")
		return
	}

	sess := s.ownedMediaSession(r, d.ID)
	if sess == nil {
		fail(w, 404, "session_not_found")
		return
	}

	metadata := sess.metadata
	if metadata == nil {
		var value domain.Metadata
		var err error
		if sess.source.Path != "" && s.deps.Media != nil {
			value, err = s.deps.Media.Probe(r.Context(), sess.source.Path)
		} else if s.deps.RemoteMedia != nil && media.RemoteCandidate(sess.source) {
			value, err = s.deps.RemoteMedia.ProbeRemote(r.Context(), sess.source)
		}
		if err != nil {
			fail(w, 502, "media_probe_failed")
			return
		}
		metadata = &value
		sess.metadata = metadata
	}

	inventory := playback.Qualities(*metadata, sess.source, d, sess.selection.Quality)
	if !playback.HasQuality(inventory, request.QualityID) {
		fail(w, 409, "quality_unavailable")
		return
	}

	originalSource := sess.source
	originalMetadata := metadata
	targetSource := sess.source
	targetMetadata := metadata

	if sess.source.Item.Provider == "youtube" && (len(sess.source.Variants) > 0 || sess.source.ResolveURL != "") {
		kind := sess.source.Item.Kind
		if kind == "" {
			kind = "video"
		}
		if request.QualityID != "auto" && request.QualityID != "" {
			target, meta, ok := s.resolveAndProbeYouTubeSource(r.Context(), sess.source, request.QualityID)
			if ok {
				targetSource = target
				targetMetadata = &meta
			} else {
				// Resolution or required remote media probe failed:
				// Revert preference to Auto
				_ = s.revertQualityPreference(r.Context(), d.ID, kind)
				request.QualityID = "auto"

				if isManualYouTubeSession(sess) {
					autoSource, autoMeta, autoOK := s.resolveAndProbeYouTubeSource(r.Context(), originalSource, "auto")
					if !autoOK {
						fail(w, 502, "quality_unavailable")
						return
					}
					targetSource = autoSource
					targetMetadata = &autoMeta
				} else {
					targetSource = originalSource
					targetMetadata = originalMetadata
				}
			}
		} else if request.QualityID == "auto" {
			if isManualYouTubeSession(sess) {
				autoSource, autoMeta, autoOK := s.resolveAndProbeYouTubeSource(r.Context(), sess.source, "auto")
				if !autoOK {
					fail(w, 502, "quality_unavailable")
					return
				}
				targetSource = autoSource
				targetMetadata = &autoMeta
				_ = s.revertQualityPreference(r.Context(), d.ID, kind)
			}
		}
	}

	mode, chosenQuality := playback.SelectedQualityMode(*targetMetadata, targetSource, d, request.QualityID, request.PositionMS, sess.mode)
	if mode == "EXTERNAL_PLAYER" {
		fail(w, 409, "quality_unavailable")
		return
	}

	oldID := r.PathValue("session")
	s.mu.Lock()
	oldSess := s.sessions[oldID]
	if oldSess == nil || oldSess.device != d.ID || oldSess.ctx.Err() != nil {
		s.mu.Unlock()
		fail(w, 404, "session_not_found")
		return
	}

	// Bounds and cleans earlier superseded sessions for this device to prevent unbounded growth:
	s.cleanupSupersededSessionsLocked(d.ID, oldID)

	if len(s.sessions) >= 64 {
		s.mu.Unlock()
		fail(w, 429, "session_limit")
		return
	}

	id, ticket := randomID(16), randomID(24)
	ctx, cancel := context.WithDeadline(context.Background(), oldSess.expires)
	selection := oldSess.selection
	selection.Quality = chosenQuality
	selection.PositionMS = request.PositionMS
	if oldSess.source.Live {
		selection.PositionMS = 0
	}

	newSess := &session{
		networkAdaptation: oldSess.networkAdaptation,
		adaptation:        oldSess.adaptation,
		mode:              mode,
		device:            d.ID,
		ticket:            ticket,
		expires:           oldSess.expires,
		source:            targetSource,
		metadata:          targetMetadata,
		subtitleID:        oldSess.subtitleID,
		ctx:               ctx,
		cancel:            cancel,
		selection:         selection,
		resources:         map[string]string{},
		supersedes:        oldID,
	}
	oldSess.supersededBy = id
	s.sessions[id] = newSess

	plan := domain.Plan{
		Version:    1,
		SubtitleID: oldSess.subtitleID,
		SessionID:  id,
		Mode:       mode,
		URL:        "/v1/streams/" + id + "?ticket=" + ticket,
		MIME:       targetSource.MIME,
		Live:       targetSource.Live,
		Item:       targetSource.Item,
	}
	if mode == "TRANSCODE" || mode == "REMUX" {
		plan.MIME = "video/mp4"
		plan.Seekable = false
		plan.ResumeMS = 0
		if mode == "TRANSCODE" {
			plan.TimelineOffsetMS = request.PositionMS
		}
	} else {
		plan.Seekable = !targetSource.Live
		plan.ResumeMS = request.PositionMS
	}

	s.events.publish(d.ID, "playback.created", map[string]string{"sessionId": id})
	s.mu.Unlock()

	// Persist preference to SQLite outside s.mu to avoid blocking global server state on DB I/O.
	// Make failure semantics explicit: roll back created session if preference write fails.
	kind := oldSess.source.Item.Kind
	if kind == "" {
		kind = "video"
	}
	if err := s.setQualityPreference(r.Context(), d.ID, kind, request.QualityID); err != nil {
		s.mu.Lock()
		if created := s.sessions[id]; created != nil {
			created.cancel()
			delete(s.sessions, id)
		}
		if s.sessions[oldID] != nil && s.sessions[oldID].supersededBy == id {
			s.sessions[oldID].supersededBy = ""
		}
		s.mu.Unlock()
		fail(w, 500, "storage_error")
		return
	}

	respond(w, 201, plan)
}
