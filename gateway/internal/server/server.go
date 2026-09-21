// Package server exposes the versioned client protocol. Provider DTOs stay here.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"zombiebox.local/gateway/internal/domain"
)

type Health struct {
	Status     string `json:"status"`
	APIVersion int    `json:"apiVersion"`
}
type Options struct {
	ProbeDir                                   string
	ThreadfinURL                               string
	RelayURL, RelayControlURL, RelayAdminToken string
	RTSPPort                                   int
	PairingCode                                string
	MediaDir                                   string
	PollWait                                   time.Duration
	CatalogWait                                time.Duration
}
type attempt struct {
	count int
	until time.Time
}
type Server struct {
	probeKey          string
	browser           *browserSession
	integrationChecks chan struct{}
	relayJobs         chan struct{}
	casts             map[string]*castSession
	seen              map[string]time.Time
	done              chan struct{}
	closeOnce         sync.Once
	db                Persistence
	deps              Dependencies
	opt               Options
	events            *eventLog
	mux               *http.ServeMux
	mu                sync.Mutex
	attempts          map[string]attempt
	sessions          map[string]*session
	polls             chan struct{}
	catalogCache      map[string]catalogEntry
	searchResults     map[string]searchResult
	configRevision    map[string]uint64
	managed           map[string]bool
	streams           chan struct{}
}

func New(db Persistence, opt Options, deps Dependencies) *Server {
	deps.validate(db)
	if opt.PollWait <= 0 {
		opt.PollWait = 20 * time.Second
	}
	if opt.CatalogWait <= 0 {
		opt.CatalogWait = 5 * time.Second
	}
	s := &Server{db: db, opt: opt, deps: deps, events: newEvents(), mux: http.NewServeMux(), attempts: map[string]attempt{}, sessions: map[string]*session{}, polls: make(chan struct{}, 32), catalogCache: map[string]catalogEntry{}}
	s.probeKey = randomID(32)
	s.mux.HandleFunc("GET /v1/artwork/{item}", s.auth(s.artwork))
	s.mux.HandleFunc("GET /v1/probes", s.auth(s.probeManifest))
	s.mux.HandleFunc("GET /v1/probes/{probe}", s.probeStream)
	s.relayJobs = make(chan struct{}, 4)
	s.integrationChecks = make(chan struct{}, 2)
	s.casts = map[string]*castSession{}
	s.seen = map[string]time.Time{}
	s.done = make(chan struct{})
	if s.opt.RTSPPort == 0 {
		s.opt.RTSPPort = 8554
	}
	go s.reapCasts()
	s.configRevision = map[string]uint64{}
	s.managed = map[string]bool{}
	s.searchResults = map[string]searchResult{}
	s.streams = make(chan struct{}, 4)
	s.mux.HandleFunc("GET /v1/cast/receivers", s.auth(s.castReceivers))
	s.mux.HandleFunc("POST /v1/cast", s.auth(s.createCast))
	s.mux.HandleFunc("GET /v1/cast/active", s.auth(s.activeCast))
	s.mux.HandleFunc("PUT /v1/cast/{cast}", s.auth(s.castLease))
	s.mux.HandleFunc("POST /v1/cast/{cast}/ready", s.auth(s.castReady))
	s.mux.HandleFunc("DELETE /v1/cast/{cast}", s.auth(s.stopCast))
	s.mux.HandleFunc("POST /internal/relay/auth", s.relayAuth)
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, Health{"ok", 1}) })
	s.mux.HandleFunc("POST /v1/devices/register", s.register)
	s.mux.HandleFunc("GET /v1/device", s.auth(func(w http.ResponseWriter, r *http.Request, d domain.Device) { respond(w, 200, d) }))
	s.mux.HandleFunc("GET /v1/device/preferences", s.auth(func(w http.ResponseWriter, r *http.Request, d domain.Device) { respond(w, 200, d.Preferences) }))
	s.mux.HandleFunc("PUT /v1/device/preferences", s.auth(s.preferences))
	s.mux.HandleFunc("PUT /v1/device/capabilities", s.auth(s.capabilities))
	s.mux.HandleFunc("GET /v1/modules", s.auth(s.modules))
	s.mux.HandleFunc("GET /v1/catalog", s.auth(s.catalogPage))
	s.mux.HandleFunc("GET /v1/home", s.auth(s.home))
	s.mux.HandleFunc("GET /v1/events", s.auth(s.poll))
	s.mux.HandleFunc("GET /v1/integrations", s.auth(s.integrationList))
	s.mux.HandleFunc("POST /v1/browser", s.auth(s.startBrowser))
	s.mux.HandleFunc("GET /v1/browser/{browser}/frame", s.auth(s.browserOperation))
	s.mux.HandleFunc("POST /v1/browser/{browser}/input", s.auth(s.browserOperation))
	s.mux.HandleFunc("DELETE /v1/browser/{browser}", s.auth(s.browserOperation))
	s.mux.HandleFunc("GET /v1/player/spotify", s.auth(s.nowPlaying))
	s.mux.HandleFunc("GET /v1/player/spotify/authorization", s.auth(s.spotifyAuthorization))
	s.mux.HandleFunc("POST /v1/player/spotify", s.auth(s.playerCommand))
	s.mux.HandleFunc("GET /v1/providers", s.auth(s.providers))
	s.mux.HandleFunc("PUT /v1/providers/{provider}", s.auth(s.configureProvider))
	s.mux.HandleFunc("POST /v1/playback", s.auth(s.playback))
	s.mux.HandleFunc("PUT /v1/playback/{session}/progress", s.auth(s.progress))
	s.mux.HandleFunc("DELETE /v1/playback/{session}", s.auth(s.stop))
	s.mux.HandleFunc("GET /v1/streams/{session}", s.stream)
	s.mux.HandleFunc("GET /v1/streams/{session}/{resource}", s.stream)
	return s
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}
func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func tokenHash(token string) string {
	v := sha256.Sum256([]byte(token))
	return hex.EncodeToString(v[:])
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, code string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code}, "apiVersion": 1})
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil {
		fail(w, 400, "invalid_json")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		fail(w, 400, "invalid_json")
		return false
	}
	return true
}

var installationID = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`)

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req domain.Registration
	if !decode(w, r, &req) {
		return
	}
	if req.ProtocolVersion != 1 {
		fail(w, 426, "unsupported_protocol")
		return
	}
	if req.Platform.AndroidAPI < 9 || req.Display.Width < 0 || req.Display.Height < 0 || req.Memory.ClassMB < 0 || len(req.Platform.ABIs) > 8 || req.ClientVersion == "" || len(req.ClientVersion) > 40 || !installationID.MatchString(req.InstallationID) {
		fail(w, 400, "invalid_registration")
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, a := range s.attempts {
		if now.After(a.until) {
			delete(s.attempts, key)
		}
	}
	a := s.attempts[ip]
	if a.count >= 5 || len(s.attempts) >= 256 {
		fail(w, 429, "pairing_rate_limited")
		return
	}
	if s.opt.PairingCode == "" || subtle.ConstantTimeCompare([]byte(req.PairingCode), []byte(s.opt.PairingCode)) != 1 {
		a.count++
		a.until = now.Add(time.Minute)
		s.attempts[ip] = a
		fail(w, 403, "pairing_required")
		return
	}
	delete(s.attempts, ip)
	var existing domain.Device
	err := s.db.Get(r.Context(), "devices", req.InstallationID, &existing)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		fail(w, 500, "storage_error")
		return
	}
	if errors.Is(err, domain.ErrNotFound) {
		count, e := s.db.Count(r.Context(), "devices")
		if e != nil {
			fail(w, 500, "storage_error")
			return
		}
		if count >= 64 {
			fail(w, 409, "device_limit")
			return
		}
		existing = domain.Device{ID: req.InstallationID, Preferences: domain.DefaultPreferences(), Capabilities: domain.Capabilities{Version: 1, DeviceID: req.InstallationID, Probes: []domain.Probe{}}}
	}
	if req.Platform.ABIs == nil {
		req.Platform.ABIs = []string{}
	}
	req.PairingCode = ""
	if !reflect.DeepEqual(existing.Registration.Platform, req.Platform) || existing.Registration.Memory != req.Memory {
		existing.Capabilities = domain.Capabilities{Version: 1, DeviceID: existing.ID, Probes: []domain.Probe{}}
	}
	existing.Registration = req
	token := randomID(32)
	if s.db.PutMany(r.Context(), domain.Record{Bucket: "devices", ID: existing.ID, Value: existing}, domain.Record{Bucket: "tokens", ID: existing.ID, Value: tokenHash(token)}) != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.events.publish(existing.ID, "device.registered", map[string]string{"deviceId": existing.ID})
	mode := "HANDHELD"
	if req.Display.Dpad || !req.Display.Touch {
		mode = "TV"
	}
	respond(w, 201, map[string]any{"apiVersion": 1, "uiSchemaVersion": 1, "playbackVersion": 1, "capabilitiesVersion": 1, "deviceId": existing.ID, "deviceToken": token, "pairingRequired": false, "presentation": map[string]string{"suggestedMode": mode}, "preferences": existing.Preferences})
}
func (s *Server) auth(next func(http.ResponseWriter, *http.Request, domain.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Zombie-Device")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !installationID.MatchString(id) || len(token) != 64 {
			fail(w, 401, "unauthorized")
			return
		}
		var expected string
		err := s.db.Get(r.Context(), "tokens", id, &expected)
		if err != nil || subtle.ConstantTimeCompare([]byte(tokenHash(token)), []byte(expected)) != 1 {
			fail(w, 401, "unauthorized")
			return
		}
		var d domain.Device
		if s.db.Get(r.Context(), "devices", id, &d) != nil {
			fail(w, 401, "unauthorized")
			return
		}
		s.mu.Lock()
		s.seen[d.ID] = time.Now()
		s.mu.Unlock()
		next(w, r, d)
	}
}
func (s *Server) preferences(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var p domain.Preferences
	if !decode(w, r, &p) {
		return
	}
	if !(p.Mode == "AUTO" || p.Mode == "TV" || p.Mode == "DOCKED" || p.Mode == "HANDHELD") || !(p.UILanguage == "en" || p.UILanguage == "es") || !(p.SubtitleMode == "auto" || p.SubtitleMode == "off" || p.SubtitleMode == "forced" || p.SubtitleMode == "always") || len(p.AudioLanguages) > 8 || len(p.SubtitleLanguages) > 8 {
		fail(w, 400, "invalid_preferences")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.Get(r.Context(), "devices", d.ID, &d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	if p.AudioLanguages == nil {
		p.AudioLanguages = []string{}
	}
	if p.SubtitleLanguages == nil {
		p.SubtitleLanguages = []string{}
	}
	d.Preferences = p
	if !p.AllowCasting {
		for _, c := range s.casts {
			if c.receiver == d.ID {
				s.endCastLocked(c)
			}
		}
	}
	if s.db.Put(r.Context(), "devices", d.ID, d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.events.publish(d.ID, "preferences.changed", p)
	respond(w, 200, p)
}
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var c domain.Capabilities
	if !decode(w, r, &c) {
		return
	}
	if c.Version != 1 || c.DeviceID != d.ID || len(c.Probes) > 32 {
		fail(w, 400, "invalid_capabilities")
		return
	}
	seenProbes := map[string]bool{}
	for _, p := range c.Probes {
		if (p.Status == "PASS" && p.Stalled) || seenProbes[p.ID] || p.FirstFrameMS < 0 || p.PositionMS < 0 || p.PrepareMS > 60000 || p.FirstFrameMS > 60000 || p.PositionMS > 60000 || p.ID == "" || len(p.ID) > 80 || p.PrepareMS < 0 || !(p.Status == "PASS" || p.Status == "FAIL" || p.Status == "UNKNOWN") {
			fail(w, 400, "invalid_probe")
			return
		}
		seenProbes[p.ID] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.Get(r.Context(), "devices", d.ID, &d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	if c.Probes == nil {
		c.Probes = []domain.Probe{}
	}
	d.Capabilities = c
	if s.db.Put(r.Context(), "devices", d.ID, d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.events.publish(d.ID, "capabilities.changed", c)
	respond(w, 200, c)
}
func (s *Server) poll(w http.ResponseWriter, r *http.Request, d domain.Device) {
	select {
	case s.polls <- struct{}{}:
		defer func() { <-s.polls }()
	default:
		fail(w, 429, "poll_limit")
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 100 {
		fail(w, 400, "invalid_cursor")
		return
	}
	timer := time.NewTimer(s.opt.PollWait)
	defer timer.Stop()
	for {
		events, next, expired, changed := s.events.read(d.ID, cursor)
		if expired {
			respond(w, 409, map[string]any{"error": map[string]string{"code": "cursor_expired"}, "cursor": next, "events": events})
			return
		}
		if cursor == "" || len(events) > 0 || r.URL.Query().Get("wait") == "0" {
			respond(w, 200, map[string]any{"apiVersion": 1, "cursor": next, "events": events})
			return
		}
		cursor = next
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			respond(w, 200, map[string]any{"apiVersion": 1, "cursor": next, "events": []domain.Event{}})
			return
		case <-changed:
		}
	}
}
