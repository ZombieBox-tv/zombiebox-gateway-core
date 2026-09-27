package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeSoloistClock struct {
	now time.Time
}

func (c *fakeSoloistClock) Now() time.Time { return c.now }

func (c *fakeSoloistClock) Advance(duration time.Duration) { c.now = c.now.Add(duration) }

func TestSoloistStateParsesEventsAndEstimatesPositionAcrossPauseResume(t *testing.T) {
	clock := &fakeSoloistClock{now: time.UnixMilli(1780000000000)}
	state := NewSoloistState(clock.Now)
	session := state.BeginSession()

	applySoloistFrame(t, state, session, `{"type":"auth_state","logged_in":true,"is_active":true,"device_name":"Private Device"}`)
	applySoloistFrame(t, state, session, `{"type":"playback_state","status":"playing","item":{"uri":"spotify:track:track-private","entity_type":"track","decorations":{"identity":{"name":"Song\nTitle"},"visual_identity":{"cover":[{"url":"https://private.invalid/cover"}]},"creators":[{"entity":{"uri":"spotify:artist:artist-private","entity_type":"artist","decorations":{"identity":{"name":"Artist"}}}}],"playback":{"duration_ms":210000,"content_ratings":[]}}},"context":{"uri":"spotify:playlist:private"},"position":{"position_ms":45000,"timestamp_ms":1780000000000,"speed":1.0},"volume":65,"is_active":true,"options":{},"available_actions":{"pause":{}}}`)

	clock.Advance(1500 * time.Millisecond)
	snapshot := state.Snapshot()
	if !snapshot.Connected || !snapshot.Authenticated || !snapshot.ActivityKnown || !snapshot.Active {
		t.Fatalf("authentication/activity evidence missing: %+v", snapshot)
	}
	if snapshot.Track == nil || snapshot.Track.Title != "Song Title" || snapshot.Track.Artist != "Artist" || snapshot.Track.DurationMS != 210000 {
		t.Fatalf("unexpected semantic track: %+v", snapshot.Track)
	}
	if !snapshot.PositionKnown || snapshot.PositionMS != 46500 || snapshot.PositionAgeMS != 1500 {
		t.Fatalf("playing position did not advance from the injected clock: %+v", snapshot)
	}
	if snapshot.TVAudioVerified {
		t.Fatal("Soloist metadata was treated as evidence of TV audio")
	}

	applySoloistFrame(t, state, session, `{"type":"playback_changed","status":"paused"}`)
	pausedAt := state.Snapshot().PositionMS
	clock.Advance(5 * time.Second)
	paused := state.Snapshot()
	if !paused.PositionKnown || paused.PositionMS != pausedAt || paused.Status != SoloistPaused {
		t.Fatalf("paused playback position moved: %+v (paused at %d)", paused, pausedAt)
	}

	applySoloistFrame(t, state, session, `{"type":"playback_changed","status":"playing"}`)
	clock.Advance(2500 * time.Millisecond)
	resumed := state.Snapshot()
	if resumed.Status != SoloistPlaying || !resumed.PositionKnown || resumed.PositionMS != pausedAt+2500 {
		t.Fatalf("resumed playback position did not advance: %+v (paused at %d)", resumed, pausedAt)
	}

	applySoloistFrame(t, state, session, `{"type":"track_changed","item":{"uri":"spotify:track:next-private","entity_type":"track","decorations":{"identity":{"name":"Next Song"},"creators":[{"entity":{"uri":"spotify:artist:artist-private","entity_type":"artist","decorations":{"identity":{"name":"Next Artist"}}}}],"playback":{"duration_ms":180000}}}}`)
	changed := state.Snapshot()
	if changed.Track == nil || changed.Track.Title != "Next Song" || changed.PositionKnown || changed.TrackRevision != snapshot.TrackRevision+1 {
		t.Fatalf("track change did not fence the previous position: %+v", changed)
	}
	applySoloistFrame(t, state, session, `{"type":"position_sync","position":{"position_ms":12000,"timestamp_ms":1780000010000,"speed":1.0}}`)
	if next := state.Snapshot(); !next.PositionKnown || next.PositionMS != 12000 {
		t.Fatalf("new track position was not accepted: %+v", next)
	}
}

func TestSoloistStateFencesStaleEventsAndReconnects(t *testing.T) {
	clock := &fakeSoloistClock{now: time.UnixMilli(1780000000000)}
	state := NewSoloistState(clock.Now)
	first := state.BeginSession()
	applySoloistFrame(t, state, first, `{"type":"auth_state","logged_in":true,"is_active":false}`)
	applySoloistFrame(t, state, first, `{"type":"playback_state","status":"playing","item":{"uri":"spotify:track:first","entity_type":"track","decorations":{"identity":{"name":"First"},"playback":{"duration_ms":120000}}},"position":{"position_ms":20000,"timestamp_ms":1780000000000,"speed":1.0}}`)

	err := state.Apply(first, []byte(`{"type":"position_sync","position":{"position_ms":1000,"timestamp_ms":1779999999999,"speed":1.0}}`))
	if !errors.Is(err, ErrSoloistStaleEvent) {
		t.Fatalf("out-of-order position was not rejected as stale: %v", err)
	}
	if got := state.Snapshot().PositionMS; got != 20000 {
		t.Fatalf("stale position changed state: %d", got)
	}

	second := state.BeginSession()
	if second == first {
		t.Fatal("reconnect reused the prior session identity")
	}
	reset := state.Snapshot()
	if !reset.Connected || reset.AuthenticationKnown || reset.Authenticated || reset.Track != nil || reset.PositionKnown {
		t.Fatalf("reconnect retained stale session state: %+v", reset)
	}
	if err := state.Apply(first, []byte(`{"type":"auth_state","logged_in":true,"is_active":true}`)); !errors.Is(err, ErrSoloistStaleSession) {
		t.Fatalf("prior connection event crossed the reconnect fence: %v", err)
	}
	if err := state.EndSession(first); !errors.Is(err, ErrSoloistStaleSession) {
		t.Fatalf("prior connection closed the new session: %v", err)
	}
	if err := state.EndSession(second); err != nil {
		t.Fatalf("current connection could not end: %v", err)
	}
	if err := state.Apply(second, []byte(`{"type":"auth_state","logged_in":true,"is_active":true}`)); !errors.Is(err, ErrSoloistDisconnected) {
		t.Fatalf("ended connection accepted a late event: %v", err)
	}
}

func TestSoloistStateRejectsMalformedAndOversizedEventsWithoutMutation(t *testing.T) {
	state := NewSoloistState(nil)
	session := state.BeginSession()
	invalid := []string{
		`{`,
		`[]`,
		`{"type":"auth_state","logged_in":true,"logged_in":false,"is_active":true}`,
		`{"type":"auth_state","logged_in":"yes","is_active":true}`,
		`{"type":"playback_changed","status":"finished"}`,
		`{"type":"playback_changed","status":true}`,
		`{"type":"position_sync","position":{"position_ms":1,"timestamp_ms":1780000000000}}`,
		`{"type":"position_sync","position":{"position_ms":-1,"timestamp_ms":1780000000000,"speed":1.0}}`,
		`{"type":"position_sync","position":{"position_ms":"1","timestamp_ms":1780000000000,"speed":1.0}}`,
		`{"type":"position_sync","position":{"position_ms":1,"timestamp_ms":1780000000000,"speed":5.0}}`,
		`{"type":"track_changed","item":{"uri":"https://private.invalid/track","entity_type":"track","decorations":{}}}`,
		`{"type":"playback_state","status":"playing","item":{"entity_type":"track","decorations":{"identity":{"name":7}}}}`,
	}
	for _, frame := range invalid {
		err := state.Apply(session, []byte(frame))
		if !errors.Is(err, ErrSoloistMalformedEvent) {
			t.Errorf("invalid event was not rejected safely (%q): %v", frame, err)
		}
	}
	if err := state.Apply(session, []byte(strings.Repeat("x", maxSoloistEventBytes+1))); !errors.Is(err, ErrSoloistEventTooLarge) {
		t.Fatalf("oversized event was not bounded: %v", err)
	}
	if got := state.Snapshot(); got.AuthenticationKnown || got.Track != nil || got.PositionKnown {
		t.Fatalf("invalid input mutated state: %+v", got)
	}
}

func TestSoloistParserToleratesUnconsumedOptionalFields(t *testing.T) {
	state := NewSoloistState(nil)
	session := state.BeginSession()
	applySoloistFrame(t, state, session, `{"type":"auth_state","logged_in":true,"is_active":false,"future_auth_hint":{"version":2}}`)
	applySoloistFrame(t, state, session, `{"type":"playback_state","status":"playing","item":{"uri":"spotify:track:forward-compatible","entity_type":"track","future_entity_field":true,"decorations":{"identity":{"name":"Forward Compatible","future_identity_field":"ignored"},"playback":{"duration_ms":90000,"future_playback_field":1},"future_decoration_field":{"ignored":true}}},"position":{"position_ms":1000,"timestamp_ms":1780000000000,"speed":1.0},"future_playback_field":{"version":2}}`)
	snapshot := state.Snapshot()
	if !snapshot.Authenticated || snapshot.Track == nil || snapshot.Track.Title != "Forward Compatible" || !snapshot.PositionKnown {
		t.Fatalf("unknown optional fields blocked known Soloist state: %+v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "future_") {
		t.Fatalf("unconsumed optional fields were retained in state: %s", encoded)
	}
}

func TestSoloistPlaybackSnapshotWithoutItemPreservesActiveTrack(t *testing.T) {
	clock := &fakeSoloistClock{now: time.UnixMilli(1780000000000)}
	state := NewSoloistState(clock.Now)
	session := state.BeginSession()
	applySoloistFrame(t, state, session, `{"type":"auth_state","logged_in":true,"is_active":false}`)
	applySoloistFrame(t, state, session, `{"type":"playback_state","status":"playing","item":{"uri":"spotify:track:retained-track","entity_type":"track","decorations":{"identity":{"name":"Retained Track"},"playback":{"duration_ms":60000}}},"position":{"position_ms":5000,"timestamp_ms":1780000000000,"speed":1.0}}`)

	clock.Advance(time.Second)
	applySoloistFrame(t, state, session, `{"type":"playback_state","status":"playing"}`)
	missingItem := state.Snapshot()
	if missingItem.Track == nil || missingItem.Track.Title != "Retained Track" || !missingItem.PositionKnown || missingItem.PositionMS != 6000 {
		t.Fatalf("item omission cleared active track or progress: %+v", missingItem)
	}
	applySoloistFrame(t, state, session, `{"type":"playback_state","status":"paused","item":null}`)
	transientNull := state.Snapshot()
	if transientNull.Track == nil || transientNull.Track.Title != "Retained Track" || !transientNull.PositionKnown {
		t.Fatalf("null item cleared a non-idle track: %+v", transientNull)
	}
	applySoloistFrame(t, state, session, `{"type":"playback_state","status":"idle","item":null}`)
	ended := state.Snapshot()
	if ended.Track != nil || ended.PositionKnown || ended.Status != SoloistIdle {
		t.Fatalf("explicit idle did not clear the ended track: %+v", ended)
	}
}

func TestSoloistDiagnosticsSeparateLocalOutputFromTVAudioAndOmitPrivateFields(t *testing.T) {
	clock := &fakeSoloistClock{now: time.UnixMilli(1780000000000)}
	state := NewSoloistState(clock.Now)
	session := state.BeginSession()
	applySoloistFrame(t, state, session, `{"type":"auth_state","logged_in":true,"is_active":true,"device_name":"Private Device"}`)
	applySoloistFrame(t, state, session, `{"type":"playback_state","status":"playing","item":{"uri":"spotify:track:secret-track-id","entity_type":"track","decorations":{"identity":{"name":"Private Track"},"visual_identity":{"cover":[{"url":"https://private.invalid/cover.jpg"}]},"creators":[{"entity":{"uri":"spotify:artist:secret-artist-id","entity_type":"artist","decorations":{"identity":{"name":"Private Artist"}}}}],"playback":{"duration_ms":90000}}},"position":{"position_ms":1000,"timestamp_ms":1780000000000,"speed":1.0}}`)
	state.ObserveLocalOutput(true)

	diagnostics, err := json.Marshal(state.Diagnostics())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"secret-track-id",
		"secret-artist-id",
		"Private Device",
		"Private Track",
		"Private Artist",
		"private.invalid/cover.jpg",
	} {
		if strings.Contains(string(diagnostics), secret) {
			t.Fatalf("private upstream value leaked into diagnostics: %s", diagnostics)
		}
	}
	if !strings.Contains(string(diagnostics), `"authenticated":true`) || !strings.Contains(string(diagnostics), `"active":true`) || !strings.Contains(string(diagnostics), `"localOutputObservedRecently":true`) || !strings.Contains(string(diagnostics), `"tvAudioVerified":false`) {
		t.Fatalf("independent evidence flags missing: %s", diagnostics)
	}

	clock.Advance(maxSoloistLocalOutputAge + time.Millisecond)
	stale := state.Diagnostics()
	if stale.LocalOutputObservedRecently || stale.TVAudioVerified {
		t.Fatalf("stale local output or unmeasured TV audio reported as verified: %+v", stale)
	}
	second := state.BeginSession()
	if got := state.Snapshot(); !got.LocalOutputKnown || got.LocalOutputObservedRecently || got.TVAudioVerified || !got.Connected || got.AuthenticationKnown {
		t.Fatalf("connection reset mixed independent output evidence: %+v (session %v)", got, second.generation)
	}
}

func TestSoloistControllerUsesInjectedTransportAndSessionFence(t *testing.T) {
	state := NewSoloistState(nil)
	session := state.BeginSession()
	transport := &recordingSoloistTransport{}
	controller := NewSoloistController(state, session, transport)
	if err := controller.Play(context.Background()); !errors.Is(err, ErrSoloistUnauthenticated) {
		t.Fatalf("unauthenticated control was accepted: %v", err)
	}
	if len(transport.frames) != 0 {
		t.Fatal("unauthenticated control reached transport")
	}
	applySoloistFrame(t, state, session, `{"type":"auth_state","logged_in":true,"is_active":true}`)
	applySoloistFrame(t, state, session, `{"type":"playback_state","status":"paused","item":{"uri":"spotify:track:control-track","entity_type":"track","decorations":{"identity":{"name":"Control Track"},"playback":{"duration_ms":30000}}}}`)
	for _, action := range []func(context.Context) error{
		controller.Play,
		controller.Pause,
		controller.SkipNext,
		controller.SkipPrevious,
	} {
		if err := action(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := controller.Seek(context.Background(), 12345); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetVolume(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	if err := controller.Seek(context.Background(), maxSoloistPositionMS+1); !errors.Is(err, ErrSoloistInvalidCommand) {
		t.Fatalf("unbounded seek was accepted: %v", err)
	}
	if err := controller.SetVolume(context.Background(), 101); !errors.Is(err, ErrSoloistInvalidCommand) {
		t.Fatalf("out-of-range volume was accepted: %v", err)
	}
	if err := controller.Seek(context.Background(), 30001); !errors.Is(err, ErrSoloistInvalidCommand) {
		t.Fatalf("seek past known duration was accepted: %v", err)
	}
	if len(transport.frames) != 6 {
		t.Fatalf("unexpected command count: %d", len(transport.frames))
	}
	var seek struct {
		Type       string `json:"type"`
		Command    string `json:"command"`
		PositionMS int64  `json:"position_ms"`
	}
	if err := json.Unmarshal(transport.frames[4], &seek); err != nil || seek.Type != "command" || seek.Command != "seek" || seek.PositionMS != 12345 {
		t.Fatalf("seek payload was not the documented command shape: %+v (%v)", seek, err)
	}

	state.BeginSession()
	if err := controller.Play(context.Background()); !errors.Is(err, ErrSoloistStaleSession) {
		t.Fatalf("old controller crossed the reconnect fence: %v", err)
	}
	if len(transport.frames) != 6 {
		t.Fatal("stale controller reached the transport")
	}
}

func TestSoloistCommandResultIsAnAcknowledgmentOnly(t *testing.T) {
	state := NewSoloistState(nil)
	session := state.BeginSession()
	if err := state.Apply(session, []byte(`{"type":"command_result","command":"pause"}`)); err != nil {
		t.Fatal(err)
	}
	if snapshot := state.Snapshot(); snapshot.CommandResults != 1 || snapshot.Status != SoloistIdle {
		t.Fatalf("command_result was mistaken for playback state: %+v", snapshot)
	}
	if err := state.Apply(session, []byte(`{"type":"command_result","command":"unknown"}`)); !errors.Is(err, ErrSoloistMalformedEvent) {
		t.Fatalf("unknown command acknowledgment was accepted: %v", err)
	}
}

func applySoloistFrame(t *testing.T, state *SoloistState, session SoloistSession, frame string) {
	t.Helper()
	if err := state.Apply(session, []byte(frame)); err != nil {
		t.Fatalf("apply Soloist event %s: %v", frame, err)
	}
}

type recordingSoloistTransport struct {
	frames  [][]byte
	session SoloistSession
	err     error
}

func (r *recordingSoloistTransport) SendCommand(_ context.Context, session SoloistSession, frame []byte) error {
	r.session = session
	r.frames = append(r.frames, append([]byte(nil), frame...))
	return r.err
}

func TestSoloistParserReportsFixedErrorsWithoutFrameContents(t *testing.T) {
	state := NewSoloistState(nil)
	session := state.BeginSession()
	secret := "account@example.invalid"
	frame := []byte(fmt.Sprintf(`{"type":"auth_state","logged_in":"%s","is_active":true}`, secret))
	err := state.Apply(session, frame)
	if !errors.Is(err, ErrSoloistMalformedEvent) || strings.Contains(err.Error(), secret) {
		t.Fatalf("parser returned frame data through its error: %v", err)
	}
}
