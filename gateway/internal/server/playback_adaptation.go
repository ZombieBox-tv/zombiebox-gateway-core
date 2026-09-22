package server

import (
	"context"
	"net/http"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/playback"
)

func (s *Server) adaptPlayback(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var request struct {
		PositionMS int64 `json:"positionMs"`
	}
	if !decode(w, r, &request) {
		return
	}
	if request.PositionMS < 0 || request.PositionMS > 7*24*60*60*1000 {
		fail(w, 400, "invalid_position")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.sessions[r.PathValue("session")]
	if old == nil || old.device != d.ID || old.ctx.Err() != nil || time.Now().After(old.expires) {
		fail(w, 404, "session_not_found")
		return
	}
	unchanged := func() { respond(w, 200, map[string]any{"plan": nil}) }
	if !old.networkAdaptation || old.castID != "" || old.receiverID != "" || old.mode == "EXTERNAL_PLAYER" {
		unchanged()
		return
	}
	if old.source.Path != "" && s.deps.Media == nil || old.source.Path == "" && (s.deps.RemoteMedia == nil || !media.RemoteCandidate(old.source)) {
		unchanged()
		return
	}
	for _, probe := range d.Capabilities.Probes {
		if probe.Status == "FAIL" && (probe.ID == "http-fmp4" || probe.ID == "h264-baseline-360" || probe.ID == "aac") {
			unchanged()
			return
		}
	}
	sample := s.networkSamples[d.ID]
	candidate := playback.NetworkQuality(old.metadata, sample.kbps)
	current := ""
	if old.mode == "TRANSCODE" {
		current = "STANDARD"
		if old.selection.Quality == "LOW" {
			current = "LOW"
		}
	}
	if !old.adaptation.Observe(sample.measured, time.Now(), candidate, current) {
		unchanged()
		return
	}
	if len(s.sessions) >= 64 {
		fail(w, 429, "session_limit")
		return
	}
	id, ticket := randomID(16), randomID(24)
	ctx, cancel := context.WithDeadline(context.Background(), old.expires)
	selection := old.selection
	selection.Quality = candidate
	selection.PositionMS = request.PositionMS
	if old.source.Live {
		selection.PositionMS = 0
	}
	s.sessions[id] = &session{device: d.ID, source: old.source, mode: "TRANSCODE", metadata: old.metadata, subtitleID: old.subtitleID, selection: selection, expires: old.expires, ticket: ticket, ctx: ctx, cancel: cancel, resources: map[string]string{}, networkAdaptation: true, adaptation: playback.Adaptation{Measured: sample.measured}}
	plan := domain.Plan{Version: 1, SessionID: id, Mode: "TRANSCODE", URL: "/v1/streams/" + id + "?ticket=" + ticket, MIME: "video/mp4", Live: old.source.Live, Seekable: false, TimelineOffsetMS: selection.PositionMS, Item: old.source.Item, SubtitleID: old.subtitleID}
	respond(w, 200, map[string]any{"plan": plan})
}
