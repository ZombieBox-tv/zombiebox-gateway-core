package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/playback"
	"zombiebox.local/gateway/internal/providers"
)

type session struct {
	networkAdaptation bool
	adaptation        playback.Adaptation
	metadata          *domain.Metadata
	subtitleID        *int
	selection         domain.MediaSelection
	mode              string
	receiverID        string
	castID            string
	device            string
	ticket            string
	expires           time.Time
	source            providers.Source
	ctx               context.Context
	cancel            context.CancelFunc
	resources         map[string]string
	resourceOrder     []string
	supersedes        string
	supersededBy      string
}

func (s *Server) playback(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var req struct {
		ItemID            string `json:"itemId"`
		ReceiverID        string `json:"receiverId"`
		Mode              string `json:"mode"`
		Quality           string `json:"quality"`
		NetworkAdaptation *bool  `json:"networkAdaptation"`
		PositionMS        *int64 `json:"positionMs"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Quality != "" && !playback.ValidQuality(req.Quality) {
		fail(w, 400, "invalid_playback_quality")
		return
	}
	if req.Quality == "LOW" && req.Mode != "TRANSCODE" && req.Mode != "" && req.Mode != "AUTO" {
		fail(w, 400, "invalid_playback_quality")
		return
	}
	if req.PositionMS != nil && (*req.PositionMS < 0 || *req.PositionMS > 7*24*60*60*1000) {
		fail(w, 400, "invalid_position")
		return
	}
	if req.Mode != "" && req.Mode != "AUTO" && req.Mode != "DIRECT_PLAY" && req.Mode != "REMUX" && req.Mode != "TRANSCODE" && req.Mode != "EXTERNAL_PLAYER" {
		fail(w, 400, "invalid_playback_mode")
		return
	}
	receiverID := ""
	var found *domain.Source
	if req.ReceiverID != "" {
		found, receiverID = s.youtubeReceiver.SourceWithLease(d.ID, req.ItemID)
		if found == nil || receiverID != req.ReceiverID {
			fail(w, 409, "receiver_changed")
			return
		}
	} else {
		found = s.searchSource(r.Context(), d.ID, req.ItemID)
		if found == nil {
			found, receiverID = s.youtubeReceiver.SourceWithLease(d.ID, req.ItemID)
		}
	}
	var candidates []providers.Source
	if found == nil {
		candidates = s.catalog(r.Context())
	}
	for _, src := range candidates {
		if src.Item.ID == req.ItemID {
			copy := src
			found = &copy
			break
		}
	}
	if found == nil || !found.Item.Playable {
		fail(w, 404, "item_not_found")
		return
	}
	receiverCommand := s.youtubeReceiver.SourceCommand(d.ID, req.ItemID)
	resolved, err := s.deps.Resolver.Resolve(r.Context(), *found)
	if err != nil {
		fail(w, 502, "stream_unavailable")
		return
	}
	if (req.Mode == "REMUX" || req.Mode == "TRANSCODE") && ((resolved.Path != "" && s.deps.Media == nil) || (resolved.Path == "" && (s.deps.RemoteMedia == nil || !media.RemoteCandidate(resolved)))) {
		fail(w, 409, "conversion_unavailable")
		return
	}
	decision, err := s.playbackMode(r.Context(), resolved, d, req.Mode)
	if err != nil {
		if errors.Is(err, media.ErrBusy) {
			fail(w, 429, "media_busy")
		} else {
			fail(w, 502, "media_probe_failed")
		}
		return
	}
	mode := decision.mode
	if (req.Mode == "" || req.Mode == "AUTO") && (req.NetworkAdaptation == nil || *req.NetworkAdaptation) {
		if quality := s.networkQuality(d, decision); quality != "" {
			mode, req.Quality = "TRANSCODE", quality
		}
	}
	if (req.Mode == "" || req.Mode == "AUTO") && (req.Quality == "" || req.Quality == "auto") {
		kind := resolved.Item.Kind
		if kind == "" {
			kind = "video"
		}
		preferred := s.getQualityPreference(r.Context(), d.ID, kind)
		if preferred != "" && preferred != "auto" && decision.metadata != nil {
			inv := playback.Qualities(*decision.metadata, resolved, d, "")
			if playback.HasQuality(inv, preferred) {
				newMode, newQuality := playback.SelectedQualityMode(*decision.metadata, resolved, d, preferred, 0, mode)
				if newMode != "EXTERNAL_PLAYER" {
					mode, req.Quality = newMode, newQuality
				}
			}
		}
	} else if (req.Mode == "" || req.Mode == "AUTO") && req.Quality != "" && req.Quality != "LOW" && req.Quality != "STANDARD" {
		if decision.metadata != nil {
			newMode, newQuality := playback.SelectedQualityMode(*decision.metadata, resolved, d, req.Quality, 0, mode)
			if newMode != "EXTERNAL_PLAYER" {
				mode, req.Quality = newMode, newQuality
			}
		}
	}

	s.receiverClaims.Lock()
	defer s.receiverClaims.Unlock()
	if receiverID != "" && (s.youtubeReceiver.SourceLease(d.ID, req.ItemID) != receiverID || s.youtubeReceiver.SourceCommand(d.ID, req.ItemID) != receiverCommand) {
		fail(w, 409, "receiver_changed")
		return
	}
	if receiverID != "" && s.mediaReceiverInbox.Listening(d.ID) {
		s.mu.Lock()
		full := len(s.sessions) >= 64
		s.mu.Unlock()
		if full {
			fail(w, 429, "session_limit")
			return
		}
		var current domain.Device
		if s.db.Get(r.Context(), "devices", d.ID, &current) != nil || !current.Preferences.AllowReceiverHandoff || !current.Preferences.AllowCasting {
			fail(w, 409, "receiver_handoff_disabled")
			return
		}
		s.retireReceivers(r.Context(), d.ID, "youtube")
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
		fail(w, 429, "session_limit")
		return
	}
	id := randomID(16)
	ticket := randomID(24)
	expires := time.Now().Add(6 * time.Hour)
	ctx, cancel := context.WithDeadline(context.Background(), expires)
	s.sessions[id] = &session{networkAdaptation: (req.Mode == "" || req.Mode == "AUTO") && (req.NetworkAdaptation == nil || *req.NetworkAdaptation), receiverID: receiverID, mode: mode, metadata: decision.metadata, subtitleID: decision.subtitleID, selection: domain.MediaSelection{AudioID: decision.audioID, Quality: req.Quality}, device: d.ID, ticket: ticket, expires: expires, source: resolved, ctx: ctx, cancel: cancel, resources: map[string]string{}}
	var p domain.Progress
	_ = s.db.Get(r.Context(), "progress:"+d.ID, resolved.Item.ID, &p)
	resume := p.PositionMS
	if p.State == "ENDED" {
		resume = 0
	}
	if req.PositionMS != nil {
		resume = *req.PositionMS
	}
	if resolved.Live {
		resume = 0
	}
	plan := domain.Plan{SubtitleID: decision.subtitleID, Version: 1, SessionID: id, Mode: mode, URL: "/v1/streams/" + id + "?ticket=" + ticket, MIME: resolved.MIME, Live: resolved.Live, Seekable: !resolved.Live, ResumeMS: resume, Item: resolved.Item}
	if mode == "REMUX" && resume > 0 {
		mode, plan.Mode, s.sessions[id].mode = "TRANSCODE", "TRANSCODE", "TRANSCODE"
	}
	if mode == "REMUX" || mode == "TRANSCODE" {
		plan.MIME = "video/mp4"
		plan.Seekable = false
		plan.ResumeMS = 0
		if mode == "TRANSCODE" {
			plan.TimelineOffsetMS = resume
			s.sessions[id].selection.PositionMS = resume
		}
	}
	s.events.publish(d.ID, "playback.created", map[string]string{"sessionId": id})
	respond(w, 201, plan)
}
func (s *Server) progress(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var p domain.Progress
	if !decode(w, r, &p) {
		return
	}
	if p.PositionMS < 0 || p.DurationMS < 0 || p.PositionMS > 7*24*60*60*1000 {
		fail(w, 400, "invalid_progress")
		return
	}
	valid := map[string]bool{"PLAYING": true, "PAUSED": true, "BUFFERING": true, "ENDED": true, "FAILED": true, "STOPPED": true}
	if !valid[p.State] {
		fail(w, 400, "invalid_state")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[r.PathValue("session")]
	if sess == nil || sess.device != d.ID {
		fail(w, 404, "session_not_found")
		return
	}
	if supersededID := sess.supersedes; supersededID != "" && (p.State == "PLAYING" || p.State == "BUFFERING") {
		sess.supersedes = ""
		if supersededSess := s.sessions[supersededID]; supersededSess != nil {
			supersededSess.cancel()
			delete(s.sessions, supersededID)
			s.events.publish(supersededSess.device, "playback.stopped", map[string]string{"sessionId": supersededID})
		}
	}
	if p.State == "FAILED" {
		_ = s.revertFailedSessionQualityLocked(r.Context(), r.PathValue("session"), sess)
	}
	if sess.castID != "" {
		if c := s.casts[sess.castID]; c != nil && c.mediaID != "" && (p.State == "ENDED" || p.State == "STOPPED" || p.State == "FAILED") {
			if p.State == "ENDED" {
				s.mediaQueue.Complete(strings.TrimPrefix(c.sender, "companion-"), c.mediaID, true)
			}
			s.endCastLocked(c)
		}
		respond(w, 200, map[string]string{"state": p.State})
		return
	}
	if sess.source.Live {
		respond(w, 200, map[string]string{"state": p.State})
		return
	}
	p.Item = sess.source.Item
	p.UpdatedAt = time.Now().Unix()
	count, err := s.db.Count(r.Context(), "progress:"+d.ID)
	if err != nil {
		fail(w, 500, "storage_error")
		return
	}
	if count >= 200 {
		var old domain.Progress
		if s.db.Get(r.Context(), "progress:"+d.ID, p.Item.ID, &old) != nil {
			fail(w, 409, "history_limit")
			return
		}
	}
	if s.db.Put(r.Context(), "progress:"+d.ID, p.Item.ID, p) != nil {
		fail(w, 500, "storage_error")
		return
	}
	respond(w, 200, map[string]string{"state": p.State})
}
func (s *Server) stop(w http.ResponseWriter, r *http.Request, d domain.Device) {
	if s.mediaReceiverInbox.Dismiss(d.ID, r.PathValue("session")) {
		respond(w, 200, map[string]string{"state": "STOPPED"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("session")
	sess := s.sessions[id]
	if sess == nil || sess.device != d.ID {
		fail(w, 404, "session_not_found")
		return
	}
	if c := s.casts[sess.castID]; c != nil {
		s.endCastLocked(c)
	}
	if supersededID := sess.supersedes; supersededID != "" {
		sess.supersedes = ""
		if supersededSess := s.sessions[supersededID]; supersededSess != nil {
			supersededSess.cancel()
			delete(s.sessions, supersededID)
		}
	}
	if sess.supersededBy != "" {
		if nextSess := s.sessions[sess.supersededBy]; nextSess != nil {
			nextSess.supersedes = ""
		}
	}
	sess.cancel()
	delete(s.sessions, id)
	s.events.publish(d.ID, "playback.stopped", map[string]string{"sessionId": id})
	respond(w, 200, map[string]string{"state": "STOPPED"})
}
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sess := s.sessions[r.PathValue("session")]
	if sess == nil || time.Now().After(sess.expires) || subtle.ConstantTimeCompare([]byte(sess.ticket), []byte(r.URL.Query().Get("ticket"))) != 1 {
		s.mu.Unlock()
		fail(w, 401, "invalid_stream_ticket")
		return
	}
	if supersededID := sess.supersedes; supersededID != "" {
		sess.supersedes = ""
		if supersededSess := s.sessions[supersededID]; supersededSess != nil {
			supersededSess.cancel()
			delete(s.sessions, supersededID)
			s.events.publish(supersededSess.device, "playback.stopped", map[string]string{"sessionId": supersededID})
		}
	}
	src := sess.source
	if resource := r.PathValue("resource"); resource != "" {
		raw, ok := sess.resources[resource]
		if !ok {
			s.mu.Unlock()
			fail(w, 404, "resource_not_found")
			return
		}
		src.URL = raw
		src.Path = ""
		src.MIME = "application/octet-stream"
		if strings.HasSuffix(strings.ToLower(strings.Split(raw, "?")[0]), ".m3u8") {
			src.MIME = "application/vnd.apple.mpegurl"
		}
		a, _ := url.Parse(sess.source.URL)
		b, _ := url.Parse(raw)
		if a == nil || b == nil || a.Scheme != b.Scheme || a.Host != b.Host {
			src.Headers = nil
		}
	}
	s.mu.Unlock()
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		fail(w, 429, "stream_limit")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopCancel := context.AfterFunc(sess.ctx, cancel)
	defer stopCancel()
	r = r.WithContext(ctx)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", src.MIME)
	w.Header().Set("Cache-Control", "private, no-store")
	if sess.mode == "REMUX" || sess.mode == "TRANSCODE" {
		if r.Header.Get("Range") != "" && r.Header.Get("Range") != "bytes=0-" {
			fail(w, 416, "conversion_not_seekable")
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		if r.Method == "HEAD" {
			return
		}
		writer := &conversionWriter{contextWriter: contextWriter{ResponseWriter: w, ctx: ctx}}
		var err error
		if src.Path != "" {
			err = s.deps.Media.ConvertSelected(ctx, src.Path, sess.mode, sess.selection, writer)
		} else {
			err = s.deps.RemoteMedia.ConvertRemote(ctx, src, sess.mode, sess.selection, writer)
		}
		if err != nil {
			if !writer.started {
				if errors.Is(err, media.ErrBusy) {
					fail(w, 429, "media_busy")
				} else {
					s.mu.Lock()
					_ = s.revertFailedSessionQualityLocked(r.Context(), r.PathValue("session"), sess)
					s.mu.Unlock()
					fail(w, 502, "conversion_failed")
				}
			} else {
				s.mu.Lock()
				_ = s.revertFailedSessionQualityLocked(r.Context(), r.PathValue("session"), sess)
				s.mu.Unlock()
				panic(http.ErrAbortHandler)
			}
		}
		return
	}
	if src.Path != "" {
		f, err := os.Open(src.Path)
		if err != nil {
			fail(w, 404, "media_unavailable")
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			fail(w, 500, "media_error")
			return
		}
		http.ServeContent(contextWriter{ResponseWriter: w, ctx: r.Context()}, r, "media", info.ModTime(), f)
		return
	}
	if r.Method == "HEAD" && src.Live {
		return
	}
	if src.Item.Provider == "youtube" && src.AudioURL == "" && media.YouTubeRangeOrigin(src.URL) && (r.Header.Get("Range") == "" || strings.HasSuffix(r.Header.Get("Range"), "-")) {
		started, err := media.RelayYouTubeProgressive(w, r, s.deps.StreamHTTP, src)
		if err != nil && ctx.Err() == nil {
			if started {
				panic(http.ErrAbortHandler)
			}
			fail(w, 502, "stream_unavailable")
		}
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", src.URL, nil)
	if err != nil {
		fail(w, 502, "stream_unavailable")
		return
	}
	req.Header = src.Headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if value := r.Header.Get("Range"); value != "" {
		req.Header.Set("Range", value)
	}
	// Stream transport has header/idle timeouts, but no total timeout on long media bodies.
	res, err := s.deps.StreamHTTP.Do(req)
	if err != nil {
		fail(w, 502, "stream_unavailable")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 206 {
		fail(w, 502, "stream_unavailable")
		return
	}
	if strings.Contains(src.MIME, "mpegurl") || strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "mpegurl") {
		body, err := io.ReadAll(io.LimitReader(res.Body, (512<<10)+1))
		if err != nil || len(body) > 512<<10 {
			fail(w, 502, "invalid_playlist")
			return
		}
		rewritten, err := s.rewritePlaylist(sess, r.PathValue("session"), res.Request.URL.String(), string(body))
		if err != nil {
			fail(w, 502, "invalid_playlist")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, rewritten)
		return
	}
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := res.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(res.StatusCode)
	buffer := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buffer)
		if n > 0 {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Deadlines apply to each upstream read, including a stalled media body.
// Close cancels streams before the process drains its HTTP server.
func (s *Server) Close() {
	s.mediaQueue.Close()
	s.mediaReceiverInbox.Close()
	s.closeOnce.Do(func() { close(s.done) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	s.youtubeReceiver.Close(ctx)
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.browser != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.deps.Browser.BrowserRequest(ctx, s.browser.config, "DELETE", "/session/"+s.browser.id, nil)
		cancel()
		s.browser = nil
	}
	for _, c := range s.casts {
		s.endCastLocked(c)
	}
	for id, x := range s.sessions {
		x.cancel()
		delete(s.sessions, id)
	}
}

// Bound local media writes and observe explicit session cancellation.
type contextWriter struct {
	http.ResponseWriter
	ctx context.Context
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(30 * time.Second))
	return w.ResponseWriter.Write(p)
}

// Do not append a JSON error to a partially transmitted MP4.
type conversionWriter struct {
	contextWriter
	started bool
}

func (w *conversionWriter) Write(p []byte) (int, error) {
	w.started = true
	return w.contextWriter.Write(p)
}
