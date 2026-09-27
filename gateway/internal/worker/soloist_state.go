package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxSoloistEventBytes      = 64 << 10
	maxSoloistJSONDepth       = 32
	maxSoloistJSONTokens      = 4096
	maxSoloistNameBytes       = 256
	maxSoloistCreatorBytes    = 128
	maxSoloistCreators        = 8
	maxSoloistArtistBytes     = maxSoloistCreatorBytes*maxSoloistCreators + 2*(maxSoloistCreators-1)
	maxSoloistURIBytes        = 512
	maxSoloistAlbumBytes      = maxSoloistNameBytes
	maxSoloistCoverCandidates = 8
	maxSoloistCoverURLBytes   = 2048
	maxSoloistCoverIDBytes    = 256
	maxSoloistRatingCount     = 16
	maxSoloistRatingBytes     = 32
	maxSoloistPositionMS      = int64(7 * 24 * time.Hour / time.Millisecond)
	maxSoloistDurationMS      = maxSoloistPositionMS
	maxSoloistPlaybackSpeed   = 4.0
	maxSoloistLocalOutputAge  = 5 * time.Second
	maxSoloistPositionAge     = 24 * time.Hour
	minimumSoloistTimestampMS = int64(1)
	maximumSoloistTimestampMS = int64(253402300799999)
)

var (
	ErrSoloistMalformedEvent     = errors.New("invalid Soloist event")
	ErrSoloistEventTooLarge      = errors.New("Soloist event exceeds size limit")
	ErrSoloistUnsupported        = errors.New("unsupported Soloist event")
	ErrSoloistStaleSession       = errors.New("stale Soloist session")
	ErrSoloistStaleEvent         = errors.New("stale Soloist event")
	ErrSoloistDisconnected       = errors.New("Soloist session is disconnected")
	ErrSoloistUnauthenticated    = errors.New("Soloist authentication required")
	ErrSoloistInvalidCommand     = errors.New("invalid Soloist command")
	ErrSoloistControlUnavailable = errors.New("Soloist control transport unavailable")
	ErrSoloistTransport          = errors.New("Soloist control transport failed")
)

// SoloistSession is an opaque WebSocket connection identity. A token returned
// by BeginSession fences messages and controls from earlier connections.
type SoloistSession struct {
	generation uint64
}

// SoloistPlaybackStatus contains only the documented fixed playback states.
type SoloistPlaybackStatus string

const (
	SoloistIdle      SoloistPlaybackStatus = "idle"
	SoloistPlaying   SoloistPlaybackStatus = "playing"
	SoloistPaused    SoloistPlaybackStatus = "paused"
	SoloistBuffering SoloistPlaybackStatus = "buffering"
)

// SoloistMetadata is a bounded semantic subset of a Soloist entity. Explicit
// is nil when no rating list is known, otherwise it records whether that list
// contains the explicit label. Spotify URIs and artwork URLs are omitted.
type SoloistMetadata struct {
	Title      string `json:"title,omitempty"`
	Artist     string `json:"artist,omitempty"`
	Album      string `json:"album,omitempty"`
	DurationMS int64  `json:"durationMs,omitempty"`
	Explicit   *bool  `json:"explicit,omitempty"`
}

type soloistArtworkCandidate struct {
	url               string
	size              string
	sessionGeneration uint64
	trackRevision     uint64
}

// SoloistSnapshot is local-playback state. TVAudioVerified is intentionally
// always false: this state reducer has no evidence of sound at a TV.
type SoloistSnapshot struct {
	Connected                   bool                  `json:"connected"`
	AuthenticationKnown         bool                  `json:"authenticationKnown"`
	Authenticated               bool                  `json:"authenticated"`
	ActivityKnown               bool                  `json:"activityKnown"`
	Active                      bool                  `json:"active"`
	Status                      SoloistPlaybackStatus `json:"status"`
	Track                       *SoloistMetadata      `json:"track,omitempty"`
	TrackRevision               uint64                `json:"trackRevision"`
	PositionKnown               bool                  `json:"positionKnown"`
	PositionMS                  int64                 `json:"positionMs"`
	PositionAgeMS               int64                 `json:"positionAgeMs,omitempty"`
	LocalOutputKnown            bool                  `json:"localOutputKnown"`
	LocalOutputObservedRecently bool                  `json:"localOutputObservedRecently"`
	LocalOutputAgeMS            int64                 `json:"localOutputAgeMs,omitempty"`
	CommandResults              uint64                `json:"commandResults"`
	TVAudioVerified             bool                  `json:"tvAudioVerified"`
}

// SoloistDiagnostics deliberately excludes metadata and every upstream
// identifier. Local output and TV output are independent evidence flags.
type SoloistDiagnostics struct {
	Connected                   bool                  `json:"connected"`
	AuthenticationKnown         bool                  `json:"authenticationKnown"`
	Authenticated               bool                  `json:"authenticated"`
	ActivityKnown               bool                  `json:"activityKnown"`
	Active                      bool                  `json:"active"`
	Status                      SoloistPlaybackStatus `json:"status"`
	PositionKnown               bool                  `json:"positionKnown"`
	LocalOutputKnown            bool                  `json:"localOutputKnown"`
	LocalOutputObservedRecently bool                  `json:"localOutputObservedRecently"`
	LocalOutputAgeMS            int64                 `json:"localOutputAgeMs,omitempty"`
	CommandResults              uint64                `json:"commandResults"`
	TVAudioVerified             bool                  `json:"tvAudioVerified"`
}

// SoloistState reduces a bounded subset of Soloist's documented JSON events.
// It does not connect to Soloist or wire itself into Full/Edge.
type SoloistState struct {
	mu  sync.Mutex
	now func() time.Time

	generation uint64
	connected  bool

	authenticationKnown bool
	authenticated       bool
	activityKnown       bool
	active              bool
	status              SoloistPlaybackStatus

	track          *SoloistMetadata
	trackIdentity  [32]byte
	trackKnown     bool
	trackRevision  uint64
	coverCandidate *soloistArtworkCandidate

	positionKnown       bool
	positionMS          int64
	positionReceivedAt  time.Time
	positionSpeed       float64
	resumeSpeed         float64
	positionTimestampMS int64

	localOutputKnown  bool
	localOutputActive bool
	localOutputAt     time.Time

	commandResults uint64
}

// NewSoloistState creates a reducer using the supplied clock. A nil clock uses
// time.Now; production callers can inject a monotonic source and tests a fake.
func NewSoloistState(now func() time.Time) *SoloistState {
	if now == nil {
		now = time.Now
	}
	return &SoloistState{now: now, status: SoloistIdle}
}

// BeginSession starts a fresh WebSocket identity and clears all session state.
// Independent local-output observations remain owned by their audio observer.
func (s *SoloistState) BeginSession() SoloistSession {
	if s == nil {
		return SoloistSession{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation++
	if s.generation == 0 {
		s.generation++
	}
	s.connected = true
	s.resetSessionState()
	return SoloistSession{generation: s.generation}
}

// EndSession disconnects the current WebSocket identity and clears its state.
func (s *SoloistState) EndSession(session SoloistSession) error {
	if s == nil {
		return ErrSoloistStaleSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesSession(session) {
		return ErrSoloistStaleSession
	}
	s.connected = false
	s.resetSessionState()
	return nil
}

// Apply parses and applies one Soloist JSON text frame. The frame is bounded,
// duplicate-key checked, and reduced to semantic fields before state changes.
func (s *SoloistState) Apply(session SoloistSession, frame []byte) error {
	if s == nil {
		return ErrSoloistStaleSession
	}
	if len(frame) > maxSoloistEventBytes {
		return ErrSoloistEventTooLarge
	}
	event, err := parseSoloistEvent(frame)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesSession(session) {
		return ErrSoloistStaleSession
	}
	if !s.connected {
		return ErrSoloistDisconnected
	}
	now := s.now()
	if event.position != nil && event.position.timestampMS <= s.positionTimestampMS {
		return ErrSoloistStaleEvent
	}
	if event.position != nil {
		durationMS := int64(0)
		if event.track != nil {
			durationMS = event.track.metadata.DurationMS
		} else if s.track != nil {
			durationMS = s.track.DurationMS
		}
		if durationMS > 0 && event.position.positionMS > durationMS {
			return ErrSoloistMalformedEvent
		}
	}

	switch event.kind {
	case "auth_state":
		s.authenticationKnown = true
		s.authenticated = event.loggedIn
		s.activityKnown = true
		s.active = event.isActive
		if !event.loggedIn {
			s.clearTrack(true)
			s.status = SoloistIdle
		}
	case "playback_state":
		if !s.authenticated {
			return ErrSoloistUnauthenticated
		}
		if event.hasActivity {
			s.activityKnown = true
			s.active = event.isActive
		}
		if event.track != nil {
			s.updateTrack(event.track)
		} else if event.status == SoloistIdle {
			// Soloist can publish a partial snapshot while preserving playback.
			// An absent item is not a track-end signal unless status is idle.
			s.clearTrack(true)
		}
		s.setPlaybackStatus(event.status, now)
		if event.position != nil {
			s.setPosition(event.position, now)
		}
	case "track_changed":
		if !s.authenticated {
			return ErrSoloistUnauthenticated
		}
		s.updateTrackForced(event.track)
		s.clearPosition()
	case "playback_changed":
		if !s.authenticated {
			return ErrSoloistUnauthenticated
		}
		s.setPlaybackStatus(event.status, now)
	case "position_sync":
		if !s.authenticated {
			return ErrSoloistUnauthenticated
		}
		if !s.trackKnown {
			return ErrSoloistMalformedEvent
		}
		s.setPosition(event.position, now)
	case "command_result":
		if s.commandResults < ^uint64(0) {
			s.commandResults++
		}
	default:
		return ErrSoloistUnsupported
	}
	return nil
}

// ObserveLocalOutput records one independent local audio-output observation.
// It says nothing about a selected TV or a relayed stream.
func (s *SoloistState) ObserveLocalOutput(active bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.localOutputKnown = true
	s.localOutputActive = active
	if active {
		s.localOutputAt = s.now()
	} else {
		s.localOutputAt = time.Time{}
	}
}

// Snapshot returns bounded semantic state. Its track contains labels only;
// diagnostics should use Diagnostics to avoid exposing those labels.
func (s *SoloistState) Snapshot() SoloistSnapshot {
	if s == nil {
		return SoloistSnapshot{Status: SoloistIdle, LocalOutputAgeMS: -1}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	snapshot := SoloistSnapshot{
		Connected:           s.connected,
		AuthenticationKnown: s.authenticationKnown,
		Authenticated:       s.authenticated,
		ActivityKnown:       s.activityKnown,
		Active:              s.active,
		Status:              s.status,
		TrackRevision:       s.trackRevision,
		CommandResults:      s.commandResults,
		TVAudioVerified:     false,
		LocalOutputKnown:    s.localOutputKnown,
		LocalOutputAgeMS:    -1,
	}
	if s.track != nil {
		track := *s.track
		track.Explicit = cloneSoloistBool(s.track.Explicit)
		snapshot.Track = &track
	}
	if s.positionKnown {
		positionMS, age, fresh := s.estimatedPosition(now)
		snapshot.PositionKnown = fresh
		snapshot.PositionMS = positionMS
		if fresh {
			snapshot.PositionAgeMS = age.Milliseconds()
		}
	}
	if s.localOutputKnown && s.localOutputActive {
		age := now.Sub(s.localOutputAt)
		if age >= 0 {
			snapshot.LocalOutputAgeMS = age.Milliseconds()
			snapshot.LocalOutputObservedRecently = age <= maxSoloistLocalOutputAge
		}
	}
	return snapshot
}

// artworkCandidate returns private upstream artwork only to an internal caller
// holding the current Soloist session. It is never included in snapshots.
func (s *SoloistState) artworkCandidate(session SoloistSession) (soloistArtworkCandidate, bool, error) {
	if s == nil {
		return soloistArtworkCandidate{}, false, ErrSoloistStaleSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesSession(session) {
		return soloistArtworkCandidate{}, false, ErrSoloistStaleSession
	}
	if !s.connected {
		return soloistArtworkCandidate{}, false, ErrSoloistDisconnected
	}
	candidate := s.coverCandidate
	if candidate == nil || candidate.sessionGeneration != session.generation || candidate.trackRevision != s.trackRevision || !s.trackKnown {
		return soloistArtworkCandidate{}, false, nil
	}
	return *candidate, true, nil
}

// Diagnostics returns only fixed playback/evidence fields and never track text,
// Spotify account/device identity, URIs, image URLs, or transport contents.
func (s *SoloistState) Diagnostics() SoloistDiagnostics {
	snapshot := s.Snapshot()
	return SoloistDiagnostics{
		Connected:                   snapshot.Connected,
		AuthenticationKnown:         snapshot.AuthenticationKnown,
		Authenticated:               snapshot.Authenticated,
		ActivityKnown:               snapshot.ActivityKnown,
		Active:                      snapshot.Active,
		Status:                      snapshot.Status,
		PositionKnown:               snapshot.PositionKnown,
		LocalOutputKnown:            snapshot.LocalOutputKnown,
		LocalOutputObservedRecently: snapshot.LocalOutputObservedRecently,
		LocalOutputAgeMS:            snapshot.LocalOutputAgeMS,
		CommandResults:              snapshot.CommandResults,
		TVAudioVerified:             false,
	}
}

func (s *SoloistState) matchesSession(session SoloistSession) bool {
	return session.generation != 0 && session.generation == s.generation
}

func (s *SoloistState) resetSessionState() {
	s.authenticationKnown = false
	s.authenticated = false
	s.activityKnown = false
	s.active = false
	s.status = SoloistIdle
	s.track = nil
	s.trackIdentity = [32]byte{}
	s.trackKnown = false
	s.trackRevision = 0
	s.coverCandidate = nil
	s.clearPosition()
	s.positionTimestampMS = 0
	s.commandResults = 0
}

func (s *SoloistState) clearPosition() {
	s.positionKnown = false
	s.positionMS = 0
	s.positionReceivedAt = time.Time{}
	s.positionSpeed = 0
	s.resumeSpeed = 1
}

func (s *SoloistState) clearTrack(revise bool) {
	if revise && s.trackKnown {
		s.trackRevision++
	}
	s.track = nil
	s.trackIdentity = [32]byte{}
	s.trackKnown = false
	s.coverCandidate = nil
	s.clearPosition()
}

func (s *SoloistState) updateTrack(track *soloistParsedTrack) {
	if track == nil {
		s.clearTrack(true)
		return
	}
	changed := !s.trackKnown || s.trackIdentity != track.identity
	if changed {
		s.trackRevision++
		s.clearPosition()
		s.coverCandidate = nil
	}
	s.trackKnown = true
	s.trackIdentity = track.identity
	s.mergeTrackMetadata(track, changed)
	if track.coverPresent {
		s.setCoverCandidate(track.coverCandidate)
	}
}

func (s *SoloistState) updateTrackForced(track *soloistParsedTrack) {
	s.trackRevision++
	s.trackKnown = true
	s.trackIdentity = track.identity
	s.coverCandidate = nil
	s.mergeTrackMetadata(track, true)
	if track.coverPresent {
		s.setCoverCandidate(track.coverCandidate)
	}
}

func (s *SoloistState) mergeTrackMetadata(track *soloistParsedTrack, replace bool) {
	metadata := SoloistMetadata{}
	if !replace && s.track != nil {
		metadata = *s.track
		metadata.Explicit = cloneSoloistBool(s.track.Explicit)
	}
	if replace || track.hasTitle {
		metadata.Title = track.metadata.Title
	}
	if replace || track.hasArtist {
		metadata.Artist = track.metadata.Artist
	}
	if replace || track.hasAlbum {
		metadata.Album = track.metadata.Album
	}
	if replace || track.hasDuration {
		metadata.DurationMS = track.metadata.DurationMS
	}
	if replace || track.hasExplicit {
		metadata.Explicit = cloneSoloistBool(track.metadata.Explicit)
	}
	s.track = &metadata
}

func (s *SoloistState) setCoverCandidate(candidate *soloistArtworkCandidate) {
	s.coverCandidate = nil
	if candidate == nil {
		return
	}
	copy := *candidate
	copy.sessionGeneration = s.generation
	copy.trackRevision = s.trackRevision
	s.coverCandidate = &copy
}

func (s *SoloistState) setPlaybackStatus(status SoloistPlaybackStatus, now time.Time) {
	positionMS := s.positionMS
	if s.positionKnown {
		var fresh bool
		positionMS, _, fresh = s.estimatedPosition(now)
		if !fresh {
			s.clearPosition()
		}
	}
	if status == SoloistPlaying {
		speed := s.positionSpeed
		if speed <= 0 {
			speed = s.resumeSpeed
		}
		if speed <= 0 {
			speed = 1
		}
		s.positionSpeed = speed
		s.resumeSpeed = speed
	} else if s.positionSpeed > 0 {
		s.resumeSpeed = s.positionSpeed
		s.positionSpeed = 0
	}
	if s.positionKnown {
		s.positionMS = positionMS
		s.positionReceivedAt = now
	}
	s.status = status
}

func (s *SoloistState) setPosition(position *soloistParsedPosition, now time.Time) {
	s.positionKnown = true
	s.positionMS = position.positionMS
	s.positionReceivedAt = now
	s.positionSpeed = position.speed
	if position.speed > 0 {
		s.resumeSpeed = position.speed
	}
	s.positionTimestampMS = position.timestampMS
}

func (s *SoloistState) estimatedPosition(now time.Time) (int64, time.Duration, bool) {
	if !s.positionKnown {
		return 0, 0, false
	}
	age := now.Sub(s.positionReceivedAt)
	if age < 0 || age > maxSoloistPositionAge {
		return 0, age, false
	}
	position := s.positionMS
	if s.status == SoloistPlaying && s.positionSpeed > 0 && age > 0 {
		maxPosition := maxSoloistPositionMS
		if s.track != nil && s.track.DurationMS > 0 {
			maxPosition = s.track.DurationMS
		}
		remaining := maxPosition - position
		advanceMS := float64(age/time.Millisecond) * s.positionSpeed
		if remaining <= 0 || advanceMS >= float64(remaining) {
			position = maxPosition
		} else {
			position += int64(advanceMS)
		}
	}
	return position, age, true
}

type soloistParsedEvent struct {
	kind        string
	loggedIn    bool
	isActive    bool
	hasActivity bool
	status      SoloistPlaybackStatus
	track       *soloistParsedTrack
	position    *soloistParsedPosition
}

type soloistParsedTrack struct {
	metadata       SoloistMetadata
	identity       [32]byte
	hasTitle       bool
	hasArtist      bool
	hasAlbum       bool
	hasDuration    bool
	hasExplicit    bool
	coverPresent   bool
	coverCandidate *soloistArtworkCandidate
}

type soloistParsedPosition struct {
	positionMS  int64
	timestampMS int64
	speed       float64
}

type soloistTypeFrame struct {
	Type string `json:"type"`
}

type soloistAuthFrame struct {
	Type       string  `json:"type"`
	LoggedIn   *bool   `json:"logged_in"`
	IsActive   *bool   `json:"is_active"`
	DeviceName *string `json:"device_name"`
}

type soloistPlaybackFrame struct {
	Type             string          `json:"type"`
	Status           string          `json:"status"`
	Item             json.RawMessage `json:"item"`
	Context          json.RawMessage `json:"context"`
	Position         json.RawMessage `json:"position"`
	Volume           json.RawMessage `json:"volume"`
	IsActive         json.RawMessage `json:"is_active"`
	Options          json.RawMessage `json:"options"`
	AvailableActions json.RawMessage `json:"available_actions"`
}

type soloistTrackChangedFrame struct {
	Type string          `json:"type"`
	Item json.RawMessage `json:"item"`
}

type soloistPlaybackChangedFrame struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type soloistPositionFrame struct {
	Type     string          `json:"type"`
	Position json.RawMessage `json:"position"`
}

type soloistCommandResultFrame struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type soloistPositionWire struct {
	PositionMS  *int64   `json:"position_ms"`
	TimestampMS *int64   `json:"timestamp_ms"`
	Speed       *float64 `json:"speed"`
}

type soloistEntityWire struct {
	URI         string          `json:"uri"`
	EntityType  string          `json:"entity_type"`
	Decorations json.RawMessage `json:"decorations"`
}

type soloistDecorationsWire struct {
	Identity       json.RawMessage `json:"identity"`
	VisualIdentity json.RawMessage `json:"visual_identity"`
	Parent         json.RawMessage `json:"parent"`
	Creators       json.RawMessage `json:"creators"`
	Playback       json.RawMessage `json:"playback"`
}

type soloistIdentityWire struct {
	Name *string `json:"name"`
}

type soloistPlaybackDecorationsWire struct {
	DurationMS     *int64          `json:"duration_ms"`
	ContentRatings json.RawMessage `json:"content_ratings"`
}

type soloistCreatorWire struct {
	Entity json.RawMessage `json:"entity"`
}

type soloistParentWire struct {
	Entity json.RawMessage `json:"entity"`
}

type soloistVisualIdentityWire struct {
	Cover json.RawMessage `json:"cover"`
}

type soloistCoverWire struct {
	URL  string `json:"url"`
	Size string `json:"size"`
}

func parseSoloistEvent(frame []byte) (soloistParsedEvent, error) {
	if len(frame) == 0 || len(frame) > maxSoloistEventBytes || !utf8.Valid(frame) || !json.Valid(frame) || !soloistJSONHasUniqueKeys(frame) {
		return soloistParsedEvent{}, ErrSoloistMalformedEvent
	}
	var header soloistTypeFrame
	if json.Unmarshal(frame, &header) != nil || header.Type == "" || len(header.Type) > 32 {
		return soloistParsedEvent{}, ErrSoloistMalformedEvent
	}
	event := soloistParsedEvent{kind: header.Type}
	switch header.Type {
	case "auth_state":
		var wire soloistAuthFrame
		if decodeSoloistStrict(frame, &wire) != nil || wire.LoggedIn == nil || wire.IsActive == nil || (wire.DeviceName != nil && !validSoloistLabel(*wire.DeviceName, 128)) {
			return soloistParsedEvent{}, ErrSoloistMalformedEvent
		}
		event.loggedIn = *wire.LoggedIn
		event.isActive = *wire.IsActive
	case "playback_state":
		var wire soloistPlaybackFrame
		if decodeSoloistStrict(frame, &wire) != nil || !validSoloistStatus(wire.Status) {
			return soloistParsedEvent{}, ErrSoloistMalformedEvent
		}
		track, err := parseSoloistTrack(wire.Item)
		if err != nil {
			return soloistParsedEvent{}, err
		}
		position, err := parseSoloistPosition(wire.Position)
		if err != nil {
			return soloistParsedEvent{}, err
		}
		event.status = SoloistPlaybackStatus(wire.Status)
		event.track = track
		event.position = position
		if len(wire.IsActive) > 0 {
			active, err := parseSoloistOptionalBool(wire.IsActive)
			if err != nil {
				return soloistParsedEvent{}, ErrSoloistMalformedEvent
			}
			event.hasActivity = true
			event.isActive = active
		}
	case "track_changed":
		var wire soloistTrackChangedFrame
		if decodeSoloistStrict(frame, &wire) != nil || len(wire.Item) == 0 || isSoloistNull(wire.Item) {
			return soloistParsedEvent{}, ErrSoloistMalformedEvent
		}
		track, err := parseSoloistTrack(wire.Item)
		if err != nil || track == nil {
			return soloistParsedEvent{}, ErrSoloistMalformedEvent
		}
		event.track = track
	case "playback_changed":
		var wire soloistPlaybackChangedFrame
		if decodeSoloistStrict(frame, &wire) != nil || !validSoloistStatus(wire.Status) {
			return soloistParsedEvent{}, ErrSoloistMalformedEvent
		}
		event.status = SoloistPlaybackStatus(wire.Status)
	case "position_sync":
		var wire soloistPositionFrame
		if decodeSoloistStrict(frame, &wire) != nil {
			return soloistParsedEvent{}, ErrSoloistMalformedEvent
		}
		position, err := parseSoloistPosition(wire.Position)
		if err != nil || position == nil {
			return soloistParsedEvent{}, ErrSoloistMalformedEvent
		}
		event.position = position
	case "command_result":
		var wire soloistCommandResultFrame
		if decodeSoloistStrict(frame, &wire) != nil || !knownSoloistCommand(wire.Command) {
			return soloistParsedEvent{}, ErrSoloistMalformedEvent
		}
	default:
		return soloistParsedEvent{}, ErrSoloistUnsupported
	}
	return event, nil
}

func parseSoloistPosition(raw json.RawMessage) (*soloistParsedPosition, error) {
	if len(raw) == 0 || isSoloistNull(raw) {
		return nil, nil
	}
	var wire soloistPositionWire
	if decodeSoloistStrict(raw, &wire) != nil || wire.PositionMS == nil || wire.TimestampMS == nil || wire.Speed == nil {
		return nil, ErrSoloistMalformedEvent
	}
	if *wire.PositionMS < 0 || *wire.PositionMS > maxSoloistPositionMS || *wire.TimestampMS < minimumSoloistTimestampMS || *wire.TimestampMS > maximumSoloistTimestampMS || math.IsNaN(*wire.Speed) || math.IsInf(*wire.Speed, 0) || *wire.Speed < 0 || *wire.Speed > maxSoloistPlaybackSpeed {
		return nil, ErrSoloistMalformedEvent
	}
	return &soloistParsedPosition{
		positionMS:  *wire.PositionMS,
		timestampMS: *wire.TimestampMS,
		speed:       *wire.Speed,
	}, nil
}

func parseSoloistTrack(raw json.RawMessage) (*soloistParsedTrack, error) {
	if len(raw) == 0 || isSoloistNull(raw) {
		return nil, nil
	}
	var entity soloistEntityWire
	if decodeSoloistStrict(raw, &entity) != nil || !validSoloistURI(entity.URI) || !validSoloistEntityType(entity.EntityType) {
		return nil, ErrSoloistMalformedEvent
	}
	var decorations soloistDecorationsWire
	if len(entity.Decorations) > 0 && !isSoloistNull(entity.Decorations) && decodeSoloistStrict(entity.Decorations, &decorations) != nil {
		return nil, ErrSoloistMalformedEvent
	}
	parsed := &soloistParsedTrack{}
	metadata := SoloistMetadata{}
	if len(decorations.Identity) > 0 && !isSoloistNull(decorations.Identity) {
		var identity soloistIdentityWire
		if decodeSoloistStrict(decorations.Identity, &identity) != nil || identity.Name == nil || !validSoloistLabel(*identity.Name, maxSoloistNameBytes) {
			return nil, ErrSoloistMalformedEvent
		}
		metadata.Title = cleanSoloistLabel(*identity.Name)
		parsed.hasTitle = true
	}
	if len(decorations.Playback) > 0 && !isSoloistNull(decorations.Playback) {
		var playback soloistPlaybackDecorationsWire
		if decodeSoloistStrict(decorations.Playback, &playback) != nil {
			return nil, ErrSoloistMalformedEvent
		}
		if playback.DurationMS != nil {
			if *playback.DurationMS < 0 || *playback.DurationMS > maxSoloistDurationMS {
				return nil, ErrSoloistMalformedEvent
			}
			metadata.DurationMS = *playback.DurationMS
			parsed.hasDuration = true
		}
		if len(playback.ContentRatings) > 0 {
			explicit, err := parseSoloistContentRatings(playback.ContentRatings)
			if err != nil {
				return nil, ErrSoloistMalformedEvent
			}
			metadata.Explicit = explicit
			parsed.hasExplicit = true
		}
	}
	if len(decorations.Creators) > 0 && !isSoloistNull(decorations.Creators) {
		var creators []soloistCreatorWire
		if decodeSoloistStrict(decorations.Creators, &creators) != nil || len(creators) > maxSoloistCreators {
			return nil, ErrSoloistMalformedEvent
		}
		names := make([]string, 0, len(creators))
		for _, creator := range creators {
			name, err := parseSoloistCreatorName(creator.Entity)
			if err != nil {
				return nil, ErrSoloistMalformedEvent
			}
			if name != "" {
				names = append(names, name)
			}
		}
		metadata.Artist = strings.Join(names, ", ")
		if len(metadata.Artist) > maxSoloistArtistBytes {
			return nil, ErrSoloistMalformedEvent
		}
		parsed.hasArtist = true
	}
	if len(decorations.Parent) > 0 {
		album, err := parseSoloistAlbum(decorations.Parent)
		if err != nil {
			return nil, ErrSoloistMalformedEvent
		}
		if album.present {
			metadata.Album = album.name
			parsed.hasAlbum = true
		}
	}
	if len(decorations.VisualIdentity) > 0 && !isSoloistNull(decorations.VisualIdentity) {
		var visual soloistVisualIdentityWire
		if decodeSoloistStrict(decorations.VisualIdentity, &visual) != nil {
			return nil, ErrSoloistMalformedEvent
		}
		if len(visual.Cover) > 0 {
			candidate, err := parseSoloistCoverCandidates(visual.Cover)
			if err != nil {
				return nil, ErrSoloistMalformedEvent
			}
			parsed.coverPresent = true
			parsed.coverCandidate = candidate
		}
	}
	parsed.metadata = metadata
	parsed.identity = soloistTrackIdentity(entity.URI, metadata)
	return parsed, nil
}

func parseSoloistCreatorName(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || isSoloistNull(raw) {
		return "", nil
	}
	var entity soloistEntityWire
	if decodeSoloistStrict(raw, &entity) != nil || !validSoloistURI(entity.URI) {
		return "", ErrSoloistMalformedEvent
	}
	var decorations soloistDecorationsWire
	if len(entity.Decorations) == 0 || isSoloistNull(entity.Decorations) || decodeSoloistStrict(entity.Decorations, &decorations) != nil || len(decorations.Identity) == 0 || isSoloistNull(decorations.Identity) {
		return "", ErrSoloistMalformedEvent
	}
	var identity soloistIdentityWire
	if decodeSoloistStrict(decorations.Identity, &identity) != nil || identity.Name == nil || !validSoloistLabel(*identity.Name, maxSoloistCreatorBytes) {
		return "", ErrSoloistMalformedEvent
	}
	return cleanSoloistLabel(*identity.Name), nil
}

type soloistAlbumResult struct {
	name    string
	present bool
}

func parseSoloistAlbum(raw json.RawMessage) (soloistAlbumResult, error) {
	if len(raw) == 0 || isSoloistNull(raw) {
		return soloistAlbumResult{}, nil
	}
	var parent soloistParentWire
	if decodeSoloistStrict(raw, &parent) != nil || len(parent.Entity) == 0 || isSoloistNull(parent.Entity) {
		return soloistAlbumResult{}, ErrSoloistMalformedEvent
	}
	var entity soloistEntityWire
	if decodeSoloistStrict(parent.Entity, &entity) != nil || !validSoloistURI(entity.URI) || !validSoloistEntityType(entity.EntityType) {
		return soloistAlbumResult{}, ErrSoloistMalformedEvent
	}
	if entity.EntityType != "album" {
		return soloistAlbumResult{}, nil
	}
	var decorations soloistDecorationsWire
	if len(entity.Decorations) == 0 || isSoloistNull(entity.Decorations) || decodeSoloistStrict(entity.Decorations, &decorations) != nil || len(decorations.Identity) == 0 || isSoloistNull(decorations.Identity) {
		return soloistAlbumResult{}, ErrSoloistMalformedEvent
	}
	var identity soloistIdentityWire
	if decodeSoloistStrict(decorations.Identity, &identity) != nil || identity.Name == nil || !validSoloistLabel(*identity.Name, maxSoloistAlbumBytes) {
		return soloistAlbumResult{}, ErrSoloistMalformedEvent
	}
	return soloistAlbumResult{name: cleanSoloistLabel(*identity.Name), present: true}, nil
}

func parseSoloistContentRatings(raw json.RawMessage) (*bool, error) {
	if isSoloistNull(raw) {
		return nil, nil
	}
	var ratings []string
	if decodeSoloistStrict(raw, &ratings) != nil || ratings == nil || len(ratings) > maxSoloistRatingCount {
		return nil, ErrSoloistMalformedEvent
	}
	seen := make(map[string]struct{}, len(ratings))
	explicit := false
	for _, rating := range ratings {
		if !validSoloistRating(rating) {
			return nil, ErrSoloistMalformedEvent
		}
		if _, exists := seen[rating]; exists {
			return nil, ErrSoloistMalformedEvent
		}
		seen[rating] = struct{}{}
		if rating == "explicit" {
			explicit = true
		}
	}
	return &explicit, nil
}

func validSoloistRating(value string) bool {
	if len(value) == 0 || len(value) > maxSoloistRatingBytes {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func parseSoloistCoverCandidates(raw json.RawMessage) (*soloistArtworkCandidate, error) {
	if isSoloistNull(raw) {
		return nil, nil
	}
	var covers []soloistCoverWire
	if decodeSoloistStrict(raw, &covers) != nil || covers == nil || len(covers) > maxSoloistCoverCandidates {
		return nil, ErrSoloistMalformedEvent
	}
	var selected *soloistArtworkCandidate
	for _, cover := range covers {
		if !validSoloistCoverSize(cover.Size) || !validSoloistCoverURL(cover.URL) {
			continue
		}
		candidate := &soloistArtworkCandidate{url: cover.URL, size: cover.Size}
		if selected == nil || soloistCoverPreference(candidate.size) < soloistCoverPreference(selected.size) {
			selected = candidate
		}
	}
	return selected, nil
}

func validSoloistCoverSize(value string) bool {
	switch value {
	case "small", "default", "large", "xlarge":
		return true
	default:
		return false
	}
}

func soloistCoverPreference(size string) int {
	switch size {
	case "large":
		return 0
	case "xlarge":
		return 1
	case "default":
		return 2
	case "small":
		return 3
	default:
		return 4
	}
}

func validSoloistCoverURL(value string) bool {
	if len(value) == 0 || len(value) > maxSoloistCoverURLBytes || strings.ContainsAny(value, "?#") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if !allowlistedSoloistCoverHost(host) {
		return false
	}
	parsedHost := strings.ToLower(parsed.Host)
	if parsedHost != host && parsedHost != host+":443" {
		return false
	}
	if parsed.RawPath != "" && parsed.RawPath != parsed.Path {
		return false
	}
	if !strings.HasPrefix(parsed.Path, "/image/") {
		return false
	}
	imageID := strings.TrimPrefix(parsed.Path, "/image/")
	if len(imageID) == 0 || len(imageID) > maxSoloistCoverIDBytes || imageID == "." || imageID == ".." {
		return false
	}
	for _, character := range imageID {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func allowlistedSoloistCoverHost(host string) bool {
	return host == "i.scdn.co" || host == "scdn.co" || strings.HasSuffix(host, ".scdn.co") || host == "spotifycdn.com" || strings.HasSuffix(host, ".spotifycdn.com")
}

func cloneSoloistBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func soloistTrackIdentity(uri string, metadata SoloistMetadata) [32]byte {
	if uri != "" {
		return sha256.Sum256([]byte("uri\x00" + uri))
	}
	// Album artwork and album name can arrive after the track identity. They
	// must not create a new playback revision when the URI is unavailable.
	return sha256.Sum256([]byte("metadata\x00" + metadata.Title + "\x00" + metadata.Artist + "\x00" + stringInt64(metadata.DurationMS)))
}

func validSoloistEntityType(value string) bool {
	switch value {
	case "", "track", "episode", "artist", "album", "playlist", "show", "ad", "unknown":
		return true
	default:
		return false
	}
}

func validSoloistStatus(status string) bool {
	switch SoloistPlaybackStatus(status) {
	case SoloistIdle, SoloistPlaying, SoloistPaused, SoloistBuffering:
		return true
	default:
		return false
	}
}

func validSoloistLabel(value string, maxBytes int) bool {
	return len(value) <= maxBytes && validSoloistText(value)
}

func validSoloistText(value string) bool {
	return utf8.ValidString(value)
}

func validSoloistURI(value string) bool {
	if len(value) > maxSoloistURIBytes || !validSoloistText(value) {
		return false
	}
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "spotify:") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func cleanSoloistLabel(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func parseSoloistOptionalBool(raw json.RawMessage) (bool, error) {
	if isSoloistNull(raw) {
		return false, ErrSoloistMalformedEvent
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false, ErrSoloistMalformedEvent
	}
	return value, nil
}

func isSoloistNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeSoloistStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	// Keep consumed fields type-checked while allowing optional API extensions.
	// The input size/depth/token and duplicate-key limits are enforced separately;
	// unknown values are decoded into no retained state.
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrSoloistMalformedEvent
	}
	return nil
}

func soloistJSONHasUniqueKeys(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	tokens := 0
	if !walkSoloistJSON(decoder, 0, &tokens) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func walkSoloistJSON(decoder *json.Decoder, depth int, tokens *int) bool {
	if depth > maxSoloistJSONDepth {
		return false
	}
	*tokens++
	if *tokens > maxSoloistJSONTokens {
		return false
	}
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return true
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			*tokens++
			if *tokens > maxSoloistJSONTokens {
				return false
			}
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok {
				return false
			}
			if _, exists := seen[key]; exists {
				return false
			}
			seen[key] = struct{}{}
			if !walkSoloistJSON(decoder, depth+1, tokens) {
				return false
			}
		}
		closing, err := decoder.Token()
		return err == nil && closing == json.Delim('}')
	case '[':
		for decoder.More() {
			if !walkSoloistJSON(decoder, depth+1, tokens) {
				return false
			}
		}
		closing, err := decoder.Token()
		return err == nil && closing == json.Delim(']')
	default:
		return false
	}
}

func knownSoloistCommand(command string) bool {
	switch command {
	case "play", "pause", "skip_next", "skip_prev", "seek", "set_volume", "set_shuffle", "set_repeat_context", "set_repeat_track", "add_to_queue", "activate", "deactivate":
		return true
	default:
		return false
	}
}

func stringInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

// SoloistCommandTransport is the injected boundary to a private local WebSocket
// adapter. Implementations must reject a token that is no longer connected.
type SoloistCommandTransport interface {
	SendCommand(context.Context, SoloistSession, []byte) error
}

// SoloistController emits only the small fixed control subset used by a player.
// It never changes observed state; Soloist events remain the source of truth.
type SoloistController struct {
	state     *SoloistState
	session   SoloistSession
	transport SoloistCommandTransport
}

// NewSoloistController binds controls to one state identity and injected local
// transport. It performs no connection or fallback setup.
func NewSoloistController(state *SoloistState, session SoloistSession, transport SoloistCommandTransport) *SoloistController {
	return &SoloistController{state: state, session: session, transport: transport}
}

func (c *SoloistController) Play(ctx context.Context) error {
	return c.send(ctx, "play", nil, nil)
}

func (c *SoloistController) Pause(ctx context.Context) error {
	return c.send(ctx, "pause", nil, nil)
}

func (c *SoloistController) SkipNext(ctx context.Context) error {
	return c.send(ctx, "skip_next", nil, nil)
}

func (c *SoloistController) SkipPrevious(ctx context.Context) error {
	return c.send(ctx, "skip_prev", nil, nil)
}

func (c *SoloistController) Seek(ctx context.Context, positionMS int64) error {
	if positionMS < 0 || positionMS > maxSoloistPositionMS {
		return ErrSoloistInvalidCommand
	}
	return c.send(ctx, "seek", &positionMS, nil)
}

func (c *SoloistController) SetVolume(ctx context.Context, volume int) error {
	if volume < 0 || volume > 100 {
		return ErrSoloistInvalidCommand
	}
	return c.send(ctx, "set_volume", nil, &volume)
}

func (c *SoloistController) send(ctx context.Context, command string, positionMS *int64, volume *int) error {
	if c == nil || c.state == nil || c.transport == nil || ctx == nil {
		return ErrSoloistControlUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.state.validateControl(c.session, positionMS); err != nil {
		return err
	}
	frame, err := json.Marshal(struct {
		Type       string `json:"type"`
		Command    string `json:"command"`
		PositionMS *int64 `json:"position_ms,omitempty"`
		Volume     *int   `json:"volume,omitempty"`
	}{Type: "command", Command: command, PositionMS: positionMS, Volume: volume})
	if err != nil {
		return ErrSoloistInvalidCommand
	}
	if err := c.transport.SendCommand(ctx, c.session, frame); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrSoloistTransport
	}
	return nil
}

func (s *SoloistState) validateControl(session SoloistSession, positionMS *int64) error {
	if s == nil {
		return ErrSoloistStaleSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesSession(session) {
		return ErrSoloistStaleSession
	}
	if !s.connected {
		return ErrSoloistDisconnected
	}
	if !s.authenticationKnown || !s.authenticated {
		return ErrSoloistUnauthenticated
	}
	if positionMS != nil && s.track != nil && s.track.DurationMS > 0 && *positionMS > s.track.DurationMS {
		return ErrSoloistInvalidCommand
	}
	return nil
}
