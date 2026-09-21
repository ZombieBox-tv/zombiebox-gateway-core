package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

type castSession struct {
	preparing                                                  bool
	id, sender, receiver, publishToken, readToken, publisherID string
	expires                                                    time.Time
	plan                                                       *domain.Plan
}

func (s *Server) castReceivers(w http.ResponseWriter, r *http.Request, d domain.Device) {
	records, err := s.db.List(r.Context(), "devices")
	if err != nil {
		fail(w, 500, "storage_error")
		return
	}
	out := []map[string]any{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, raw := range records {
		var receiver domain.Device
		if json.Unmarshal(raw, &receiver) != nil || receiver.ID == d.ID || !receiver.Preferences.AllowCasting {
			continue
		}
		out = append(out, map[string]any{"deviceId": receiver.ID, "name": strings.TrimSpace(receiver.Registration.Platform.Manufacturer + " " + receiver.Registration.Platform.Model), "online": time.Since(s.seen[receiver.ID]) < 45*time.Second})
	}
	respond(w, 200, map[string]any{"receivers": out, "relayAvailable": s.opt.RelayURL != ""})
}
func (s *Server) createCast(w http.ResponseWriter, r *http.Request, sender domain.Device) {
	if s.opt.RelayURL == "" {
		fail(w, 503, "relay_disabled")
		return
	}
	var request struct {
		ReceiverID string `json:"receiverId"`
	}
	if !decode(w, r, &request) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var receiver domain.Device
	if request.ReceiverID == sender.ID || s.db.Get(r.Context(), "devices", request.ReceiverID, &receiver) != nil || !receiver.Preferences.AllowCasting || time.Since(s.seen[receiver.ID]) > 45*time.Second {
		fail(w, 409, "receiver_unavailable")
		return
	}
	if len(s.casts) >= 1 || len(s.sessions) >= 64 || len(s.relayJobs) == cap(s.relayJobs) {
		fail(w, 429, "cast_busy")
		return
	}
	c := &castSession{id: randomID(16), sender: sender.ID, receiver: receiver.ID, publishToken: randomID(24), readToken: randomID(24), expires: time.Now().Add(90 * time.Second)}
	s.casts[c.id] = c
	respond(w, 201, map[string]any{"castId": c.id, "publishPath": "zombie/" + c.id, "publishUser": "zombie", "publishToken": c.publishToken, "rtspPort": s.opt.RTSPPort, "leaseSeconds": 90, "video": map[string]any{"codec": "h264", "maxWidth": 1280, "maxHeight": 720, "fps": 30, "bitrate": 2000000}})
}
func (s *Server) castLease(w http.ResponseWriter, r *http.Request, d domain.Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.casts[r.PathValue("cast")]
	if c == nil || c.sender != d.ID || time.Now().After(c.expires) {
		fail(w, 404, "cast_not_found")
		return
	}
	c.expires = time.Now().Add(90 * time.Second)
	respond(w, 200, map[string]any{"state": "ACTIVE", "leaseSeconds": 90})
}
func (s *Server) castReady(w http.ResponseWriter, r *http.Request, d domain.Device) {
	s.mu.Lock()
	c := s.casts[r.PathValue("cast")]
	if c == nil || c.sender != d.ID || time.Now().After(c.expires) {
		s.mu.Unlock()
		fail(w, 404, "cast_not_found")
		return
	}
	if c.plan != nil {
		s.mu.Unlock()
		respond(w, 200, map[string]string{"state": "ACTIVE"})
		return
	}
	if c.preparing {
		s.mu.Unlock()
		fail(w, 503, "cast_buffering")
		return
	}
	c.preparing = true
	defer func() { s.mu.Lock(); c.preparing = false; s.mu.Unlock() }()
	source := providers.Source{URL: strings.TrimRight(s.opt.RelayURL, "/") + "/zombie/" + c.id + "/index.m3u8", MIME: "application/vnd.apple.mpegurl", Live: true, Headers: http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("zombie:"+c.readToken))}}, Item: domain.Item{ID: "cast-" + c.id, Provider: "android_mirror", Kind: "video", Title: "Screen mirroring", Playable: true}}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", source.URL, nil)
	request.Header = source.Headers.Clone()
	response, err := providers.Client.Do(request)
	if err != nil {
		fail(w, 503, "cast_buffering")
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 65536))
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !strings.HasPrefix(string(body), "#EXTM3U") {
		fail(w, 503, "cast_buffering")
		return
	}
	// MediaMTX 1.21 creates a reader on each master-playlist request. Retain its
	// session-bearing variant URL so repeated TV polls reuse one reader.
	if strings.Contains(string(body), "#EXT-X-STREAM-INF:") {
		origin, _ := url.Parse(response.Request.URL.String())
		selected := false
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			relative, e := url.Parse(line)
			if e != nil {
				break
			}
			variant := origin.ResolveReference(relative)
			if variant.Scheme != origin.Scheme || variant.Host != origin.Host || variant.User != nil || !strings.HasPrefix(variant.Path, "/zombie/"+c.id+"/") {
				break
			}
			source.URL = variant.String()
			selected = true
			break
		}
		if !selected {
			fail(w, 502, "invalid_relay_playlist")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.casts[c.id] != c || time.Now().After(c.expires) {
		fail(w, 404, "cast_not_found")
		return
	}
	var receiver domain.Device
	if s.db.Get(ctx, "devices", c.receiver, &receiver) != nil || !receiver.Preferences.AllowCasting {
		s.endCastLocked(c)
		fail(w, 409, "receiver_unavailable")
		return
	}
	if c.plan == nil {
		if len(s.sessions) >= 64 {
			fail(w, 429, "session_limit")
			return
		}
		id, ticket := randomID(16), randomID(24)
		sessionCtx, sessionCancel := context.WithCancel(context.Background())
		s.sessions[id] = &session{device: c.receiver, ticket: ticket, source: source, expires: time.Now().Add(24 * time.Hour), ctx: sessionCtx, cancel: sessionCancel, resources: map[string]string{}, castID: c.id}
		c.plan = &domain.Plan{Version: 1, SessionID: id, Mode: "LIVE_LOW_LATENCY", URL: "/v1/streams/" + id + "?ticket=" + ticket, MIME: source.MIME, Live: true, Item: source.Item}
		s.events.publish(c.receiver, "cast.started", c.plan)
	}
	respond(w, 200, map[string]string{"state": "ACTIVE"})
}
func (s *Server) activeCast(w http.ResponseWriter, r *http.Request, d domain.Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.casts {
		if c.receiver == d.ID && c.plan != nil && time.Now().Before(c.expires) {
			respond(w, 200, map[string]any{"plan": c.plan})
			return
		}
	}
	respond(w, 200, map[string]any{"plan": nil})
}
func (s *Server) stopCast(w http.ResponseWriter, r *http.Request, d domain.Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.casts[r.PathValue("cast")]
	if c == nil || (c.sender != d.ID && c.receiver != d.ID) {
		fail(w, 404, "cast_not_found")
		return
	}
	s.endCastLocked(c)
	respond(w, 200, map[string]string{"state": "STOPPED"})
}
func sameSecret(a, b string) bool {
	return b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func (s *Server) relayAuth(w http.ResponseWriter, r *http.Request) {
	if !sameSecret(r.URL.Query().Get("key"), s.opt.RelayAdminToken) {
		fail(w, 403, "relay_unauthorized")
		return
	}
	var request struct{ User, Password, Path, Action, ID string }
	if !decode(w, r, &request) {
		return
	}
	if request.Action == "api" && request.User == "gateway" && sameSecret(request.Password, s.opt.RelayAdminToken) {
		w.WriteHeader(204)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.casts[strings.TrimPrefix(request.Path, "zombie/")]
	if c != nil && request.Path == "zombie/"+c.id && time.Now().Before(c.expires) && request.User == "zombie" {
		if request.Action == "publish" && sameSecret(request.Password, c.publishToken) {
			if installationID.MatchString(request.ID) {
				c.publisherID = request.ID
			}
			w.WriteHeader(204)
			return
		}
		if request.Action == "read" && sameSecret(request.Password, c.readToken) {
			w.WriteHeader(204)
			return
		}
	}
	fail(w, 403, "relay_unauthorized")
}
func (s *Server) endCastLocked(c *castSession) {
	delete(s.casts, c.id)
	if c.plan != nil {
		if session := s.sessions[c.plan.SessionID]; session != nil {
			session.cancel()
			delete(s.sessions, c.plan.SessionID)
		}
		s.events.publish(c.receiver, "cast.ended", map[string]string{"sessionId": c.plan.SessionID})
	}
	s.events.publish(c.sender, "cast.ended", map[string]string{"castId": c.id})
	if c.publisherID != "" && s.opt.RelayControlURL != "" {
		id := c.publisherID
		s.relayJobs <- struct{}{}
		go func() {
			defer func() { <-s.relayJobs }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(s.opt.RelayControlURL, "/")+"/v3/rtspsessions/kick/"+id, nil)
			if err != nil {
				return
			}
			req.SetBasicAuth("gateway", s.opt.RelayAdminToken)
			res, err := providers.Client.Do(req)
			if err == nil {
				res.Body.Close()
			}
		}()
	}
}
func (s *Server) reapCasts() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			for _, c := range s.casts {
				if time.Now().After(c.expires) || time.Since(s.seen[c.receiver]) > 60*time.Second {
					s.endCastLocked(c)
				}
			}
			s.mu.Unlock()
		}
	}
}
