package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

type qualityTestMedia struct {
	width, height int
	codec         string
	profile       string
}

func (m *qualityTestMedia) Probe(context.Context, string) (domain.Metadata, error) {
	codec := m.codec
	if codec == "" {
		codec = "h264"
	}
	profile := m.profile
	if profile == "" {
		profile = "High"
	}
	meta := domain.Metadata{
		Streams: []domain.Stream{
			{Index: 0, Type: "video", Codec: codec, Profile: profile, Width: m.width, Height: m.height},
			{Index: 1, Type: "audio", Codec: "aac"},
		},
	}
	meta.Format.Name = "mp4"
	return meta, nil
}

func (*qualityTestMedia) Convert(context.Context, string, string, io.Writer) error { return nil }
func (*qualityTestMedia) ConvertSelected(context.Context, string, string, domain.MediaSelection, io.Writer) error {
	return nil
}
func (*qualityTestMedia) Subtitles(context.Context, string, int) ([]domain.SubtitleCue, error) {
	return nil, nil
}

func setDevicePassingProbes(t *testing.T, s *Server, deviceID string, probeIDs ...string) {
	t.Helper()
	var dev domain.Device
	_ = s.db.Get(t.Context(), "devices", deviceID, &dev)
	dev.Capabilities.Probes = nil
	for _, p := range probeIDs {
		dev.Capabilities.Probes = append(dev.Capabilities.Probes, domain.Probe{
			ID:     p,
			Status: "PASS",
		})
	}
	_ = s.db.Put(t.Context(), "devices", deviceID, dev)
}

func standardCapableProbes() []string {
	return []string{
		"http-fmp4", "aac",
		"h264-baseline-360", "h264-baseline-480",
		"h264-720-main", "h264-1080-high",
	}
}

func TestPlaybackQualitiesOptionFilteringAndDeviceCaps(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Movie.mp4"), []byte("video"), 0600)
	s := testServer(t, nil, dir)
	s.deps.Media = &qualityTestMedia{width: 1920, height: 1080, codec: "h264", profile: "High"}

	owner := pair(t, s, "api13-device")
	other := pair(t, s, "other-device")

	// Set API 13 device capabilities: positive PASS for fmp4, aac, baseline, 480, and 720p, but no 1080p probe
	setDevicePassingProbes(t, s, "api13-device",
		"http-fmp4", "aac",
		"h264-baseline-360", "h264-baseline-480", "h264-720-main",
	)

	sources, _ := providers.Local(dir)
	playReq := fmt.Sprintf(`{"itemId":%q}`, sources[0].Item.ID)
	w := call(s, "POST", "/v1/playback", playReq, "api13-device", owner, "")
	if w.Code != 201 {
		t.Fatalf("failed to create playback: %d %s", w.Code, w.Body)
	}
	var plan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &plan)

	// GET /v1/playback/{session}/qualities for owner
	qResp := call(s, "GET", "/v1/playback/"+plan.SessionID+"/qualities", "", "api13-device", owner, "")
	if qResp.Code != 200 {
		t.Fatalf("qualities returned %d: %s", qResp.Code, qResp.Body)
	}
	var inv domain.QualityInventory
	json.Unmarshal(qResp.Body.Bytes(), &inv)
	if inv.SelectedID != "auto" {
		t.Fatalf("initial selectedId should be auto, got %s", inv.SelectedID)
	}

	// 1080p must not be offered to API 13 device without 1080p probe evidence; 2K/4K never fabricated
	has1080, has4K, has720, has480, hasAuto := false, false, false, false, false
	for _, opt := range inv.Options {
		if opt.ID == "1080p" {
			has1080 = true
		}
		if opt.ID == "2160p" || opt.ID == "1440p" || opt.Height > 1080 {
			has4K = true
		}
		if opt.ID == "720p" {
			has720 = true
		}
		if opt.ID == "480p" {
			has480 = true
		}
		if opt.ID == "auto" {
			hasAuto = true
		}
	}
	if has1080 {
		t.Fatal("1080p must not be offered on API 13 device without 1080p probe PASS")
	}
	if has4K {
		t.Fatal("4K/2K must not be fabricated")
	}
	if !hasAuto || !has720 || !has480 {
		t.Fatalf("expected auto, 720p, 480p to be available: %+v", inv.Options)
	}

	// Other device cannot access this session
	otherResp := call(s, "GET", "/v1/playback/"+plan.SessionID+"/qualities", "", "other-device", other, "")
	if otherResp.Code != 404 {
		t.Fatalf("expected 404 for different device, got %d", otherResp.Code)
	}

	// Now test capable device with h264-1080-high PASS
	capable := pair(t, s, "capable-device")
	setDevicePassingProbes(t, s, "capable-device", standardCapableProbes()...)

	wCap := call(s, "POST", "/v1/playback", playReq, "capable-device", capable, "")
	var capPlan domain.Plan
	json.Unmarshal(wCap.Body.Bytes(), &capPlan)

	capQResp := call(s, "GET", "/v1/playback/"+capPlan.SessionID+"/qualities", "", "capable-device", capable, "")
	var capInv domain.QualityInventory
	json.Unmarshal(capQResp.Body.Bytes(), &capInv)

	has1080Cap := false
	for _, opt := range capInv.Options {
		if opt.ID == "1080p" {
			has1080Cap = true
		}
	}
	if !has1080Cap {
		t.Fatalf("capable device with passing probe should have 1080p: %+v", capInv.Options)
	}
}

func TestSelectQualityPositionPreservingSwitch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Movie.mp4"), []byte("video"), 0600)
	s := testServer(t, nil, dir)
	s.deps.Media = &qualityTestMedia{width: 1920, height: 1080, codec: "h264", profile: "High"}

	token := pair(t, s, "switch-device")
	setDevicePassingProbes(t, s, "switch-device", standardCapableProbes()...)

	sources, _ := providers.Local(dir)
	playReq := fmt.Sprintf(`{"itemId":%q}`, sources[0].Item.ID)
	w := call(s, "POST", "/v1/playback", playReq, "switch-device", token, "")
	var plan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &plan)

	// Switch quality to 480p at position 25000ms
	body := `{"qualityId":"480p","positionMs":25000}`
	qCall := call(s, "POST", "/v1/playback/"+plan.SessionID+"/quality", body, "switch-device", token, "")
	if qCall.Code != 201 {
		t.Fatalf("select quality returned %d: %s", qCall.Code, qCall.Body)
	}
	var replacement domain.Plan
	json.Unmarshal(qCall.Body.Bytes(), &replacement)

	if replacement.SessionID == plan.SessionID {
		t.Fatal("replacement session should have a new ID")
	}
	if replacement.Mode != "TRANSCODE" {
		t.Fatalf("expected TRANSCODE mode for downscaled quality, got %s", replacement.Mode)
	}
	if replacement.TimelineOffsetMS != 25000 {
		t.Fatalf("expected timelineOffsetMs 25000, got %d", replacement.TimelineOffsetMS)
	}
	if replacement.ResumeMS != 0 || replacement.Seekable {
		t.Fatal("transcoded stream must be non-seekable with resumePositionMs: 0")
	}

	// Verify old session still exists until replacement is ready
	s.mu.Lock()
	oldSession := s.sessions[plan.SessionID]
	newSession := s.sessions[replacement.SessionID]
	s.mu.Unlock()
	if oldSession == nil {
		t.Fatal("old session was prematurely deleted")
	}
	if newSession == nil {
		t.Fatal("new session not registered in server")
	}
	if newSession.selection.Quality != "480p" || newSession.selection.PositionMS != 25000 {
		t.Fatalf("unexpected new session state: %+v", newSession.selection)
	}

	// Switching to auto preserves position
	autoCall := call(s, "POST", "/v1/playback/"+replacement.SessionID+"/quality", `{"qualityId":"auto","positionMs":30000}`, "switch-device", token, "")
	if autoCall.Code != 201 {
		t.Fatalf("select auto returned %d: %s", autoCall.Code, autoCall.Body)
	}
	var autoPlan domain.Plan
	json.Unmarshal(autoCall.Body.Bytes(), &autoPlan)
	if autoPlan.Mode == "DIRECT_PLAY" {
		if autoPlan.ResumeMS != 30000 {
			t.Fatalf("expected resumePositionMs 30000 for direct play auto, got %d", autoPlan.ResumeMS)
		}
	} else if autoPlan.Mode == "TRANSCODE" {
		if autoPlan.TimelineOffsetMS != 30000 {
			t.Fatalf("expected timelineOffsetMs 30000 for transcode auto, got %d", autoPlan.TimelineOffsetMS)
		}
	}

	// Invalid quality ID rejected
	invalidCall := call(s, "POST", "/v1/playback/"+replacement.SessionID+"/quality", `{"qualityId":"9999p","positionMs":1000}`, "switch-device", token, "")
	if invalidCall.Code != 409 {
		t.Fatalf("expected 409 for unavailable quality, got %d", invalidCall.Code)
	}

	// Invalid position rejected
	negCall := call(s, "POST", "/v1/playback/"+replacement.SessionID+"/quality", `{"qualityId":"480p","positionMs":-5}`, "switch-device", token, "")
	if negCall.Code != 400 {
		t.Fatalf("expected 400 for negative position, got %d", negCall.Code)
	}
}

func TestSelectQualityRepeatedSwitchBoundedSessions(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Movie.mp4"), []byte("video"), 0600)
	s := testServer(t, nil, dir)
	s.deps.Media = &qualityTestMedia{width: 1920, height: 1080, codec: "h264", profile: "High"}

	token := pair(t, s, "repeated-device")
	setDevicePassingProbes(t, s, "repeated-device", standardCapableProbes()...)

	sources, _ := providers.Local(dir)
	playReq := fmt.Sprintf(`{"itemId":%q}`, sources[0].Item.ID)
	w := call(s, "POST", "/v1/playback", playReq, "repeated-device", token, "")
	if w.Code != 201 {
		t.Fatalf("failed playback: %d %s", w.Code, w.Body)
	}
	var currentPlan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &currentPlan)

	s.mu.Lock()
	initialCount := len(s.sessions)
	s.mu.Unlock()
	if initialCount != 1 {
		t.Fatalf("expected 1 session initially, got %d", initialCount)
	}

	qualities := []string{"720p", "480p", "360p", "240p", "144p", "auto", "1080p"}
	for i := 0; i < 20; i++ {
		targetQ := qualities[i%len(qualities)]
		pos := int64(1000 * (i + 1))
		body := fmt.Sprintf(`{"qualityId":%q,"positionMs":%d}`, targetQ, pos)
		resp := call(s, "POST", "/v1/playback/"+currentPlan.SessionID+"/quality", body, "repeated-device", token, "")
		if resp.Code != 201 {
			t.Fatalf("switch %d to %s returned %d: %s", i, targetQ, resp.Code, resp.Body)
		}
		var nextPlan domain.Plan
		json.Unmarshal(resp.Body.Bytes(), &nextPlan)

		s.mu.Lock()
		count := len(s.sessions)
		s.mu.Unlock()
		// Session count must be strictly bounded (at most 2: active handoff predecessor + new replacement)
		if count > 2 {
			t.Fatalf("iteration %d: session count grew to %d (leak detected)", i, count)
		}
		currentPlan = nextPlan
	}

	// Now client initiates streaming on the final replacement session
	s.mu.Lock()
	finalSess := s.sessions[currentPlan.SessionID]
	s.mu.Unlock()
	if finalSess == nil {
		t.Fatal("final session not found")
	}

	streamReq := call(s, "GET", "/v1/streams/"+currentPlan.SessionID+"?ticket="+finalSess.ticket, "", "repeated-device", token, "")
	if streamReq.Code != 200 {
		t.Fatalf("stream returned %d: %s", streamReq.Code, streamReq.Body)
	}

	// After replacement is ready and streamed, superseded session must be cleaned up!
	s.mu.Lock()
	finalCount := len(s.sessions)
	s.mu.Unlock()
	if finalCount != 1 {
		t.Fatalf("expected exactly 1 active session after streaming handoff, got %d", finalCount)
	}
}

type failingPersistenceWrapper struct {
	Persistence
	failBucket string
}

func (f *failingPersistenceWrapper) Put(ctx context.Context, bucket, id string, val any) error {
	if bucket == f.failBucket {
		return errors.New("simulated_db_error")
	}
	return f.Persistence.Put(ctx, bucket, id, val)
}

func TestSelectQualityStorageFailureSemantics(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Movie.mp4"), []byte("video"), 0600)
	s := testServer(t, nil, dir)
	s.deps.Media = &qualityTestMedia{width: 1920, height: 1080, codec: "h264", profile: "High"}

	token := pair(t, s, "fail-db-device")
	setDevicePassingProbes(t, s, "fail-db-device", standardCapableProbes()...)

	sources, _ := providers.Local(dir)
	playReq := fmt.Sprintf(`{"itemId":%q}`, sources[0].Item.ID)
	w := call(s, "POST", "/v1/playback", playReq, "fail-db-device", token, "")
	var plan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &plan)

	// Wrap db to fail on quality_preferences writes
	s.db = &failingPersistenceWrapper{
		Persistence: s.db,
		failBucket:  qualityPrefBucket + ":fail-db-device",
	}

	// selectQuality must fail with 500 storage_error
	resp := call(s, "POST", "/v1/playback/"+plan.SessionID+"/quality", `{"qualityId":"480p","positionMs":5000}`, "fail-db-device", token, "")
	if resp.Code != 500 {
		t.Fatalf("expected 500 storage_error, got %d: %s", resp.Code, resp.Body)
	}

	// Must not leak replacement session in s.sessions!
	s.mu.Lock()
	count := len(s.sessions)
	sess := s.sessions[plan.SessionID]
	s.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected session count to remain 1 after DB failure, got %d", count)
	}
	if sess == nil || sess.supersededBy != "" {
		t.Fatalf("original session should remain active without superseded marker: %+v", sess)
	}
}

func TestQualityPreferencePersistenceAndReuse(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MovieA.mp4"), []byte("video1"), 0600)
	os.WriteFile(filepath.Join(dir, "MovieB.mp4"), []byte("video2"), 0600)
	s := testServer(t, nil, dir)
	media := &qualityTestMedia{width: 1920, height: 1080, codec: "h264", profile: "High"}
	s.deps.Media = media

	token := pair(t, s, "pref-device")
	setDevicePassingProbes(t, s, "pref-device", standardCapableProbes()...)
	otherToken := pair(t, s, "other-device")
	setDevicePassingProbes(t, s, "other-device", standardCapableProbes()...)

	sources, _ := providers.Local(dir)

	// Play first video and select 480p
	playA := fmt.Sprintf(`{"itemId":%q}`, sources[0].Item.ID)
	wA := call(s, "POST", "/v1/playback", playA, "pref-device", token, "")
	var planA domain.Plan
	json.Unmarshal(wA.Body.Bytes(), &planA)

	qSwitch := call(s, "POST", "/v1/playback/"+planA.SessionID+"/quality", `{"qualityId":"480p","positionMs":1000}`, "pref-device", token, "")
	if qSwitch.Code != 201 {
		t.Fatalf("failed to select 480p: %d %s", qSwitch.Code, qSwitch.Body)
	}

	// Verify preference is persisted in SQLite
	pref := s.getQualityPreference(t.Context(), "pref-device", "video")
	if pref != "480p" {
		t.Fatalf("expected stored preference 480p, got %q", pref)
	}

	// Play second video on SAME client with no quality specified:
	// Server must automatically apply 480p preference!
	playB := fmt.Sprintf(`{"itemId":%q}`, sources[1].Item.ID)
	wB := call(s, "POST", "/v1/playback", playB, "pref-device", token, "")
	if wB.Code != 201 {
		t.Fatalf("failed to play second video: %d %s", wB.Code, wB.Body)
	}
	var planB domain.Plan
	json.Unmarshal(wB.Body.Bytes(), &planB)
	if planB.Mode != "TRANSCODE" {
		t.Fatalf("expected preferred 480p to select TRANSCODE mode, got %s", planB.Mode)
	}
	s.mu.Lock()
	sessB := s.sessions[planB.SessionID]
	s.mu.Unlock()
	if sessB.selection.Quality != "480p" {
		t.Fatalf("expected applied quality 480p, got %s", sessB.selection.Quality)
	}

	// Play on DIFFERENT device: should NOT use pref-device preference (device isolation)
	wOther := call(s, "POST", "/v1/playback", playB, "other-device", otherToken, "")
	var planOther domain.Plan
	json.Unmarshal(wOther.Body.Bytes(), &planOther)
	s.mu.Lock()
	sessOther := s.sessions[planOther.SessionID]
	s.mu.Unlock()
	if sessOther.selection.Quality == "480p" {
		t.Fatal("preference leaked to different device")
	}

	// Start playback on a low-resolution video (e.g. 240p source):
	// 480p is NOT available, so it must fall back to Auto without error!
	media.height = 240
	media.width = 426
	wLow := call(s, "POST", "/v1/playback", playB, "pref-device", token, "")
	if wLow.Code != 201 {
		t.Fatalf("low resolution video should still play: %d %s", wLow.Code, wLow.Body)
	}
	var planLow domain.Plan
	json.Unmarshal(wLow.Body.Bytes(), &planLow)
	s.mu.Lock()
	sessLow := s.sessions[planLow.SessionID]
	s.mu.Unlock()
	// Should fall back to Auto, not 480p
	if sessLow.selection.Quality == "480p" {
		t.Fatal("480p should not be applied when source resolution is only 240p")
	}
}

func TestQualityPreferenceReversionOnFailure(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Movie.mp4"), []byte("video"), 0600)
	s := testServer(t, nil, dir)
	s.deps.Media = &qualityTestMedia{width: 1920, height: 1080, codec: "h264", profile: "High"}

	token := pair(t, s, "fail-device")
	setDevicePassingProbes(t, s, "fail-device", standardCapableProbes()...)

	sources, _ := providers.Local(dir)
	playReq := fmt.Sprintf(`{"itemId":%q}`, sources[0].Item.ID)
	w := call(s, "POST", "/v1/playback", playReq, "fail-device", token, "")
	var plan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &plan)

	// Set 720p quality
	_ = call(s, "POST", "/v1/playback/"+plan.SessionID+"/quality", `{"qualityId":"720p","positionMs":5000}`, "fail-device", token, "")
	if s.getQualityPreference(t.Context(), "fail-device", "video") != "720p" {
		t.Fatal("preference was not set")
	}

	// Client reports playback FAILED
	prog := call(s, "PUT", "/v1/playback/"+plan.SessionID+"/progress", `{"state":"FAILED","positionMs":5000,"durationMs":100000}`, "fail-device", token, "")
	if prog.Code != 200 {
		t.Fatalf("progress failed: %d %s", prog.Code, prog.Body)
	}

	// Preference should now be reverted to auto
	reverted := s.getQualityPreference(t.Context(), "fail-device", "video")
	if reverted != "" && reverted != "auto" {
		t.Fatalf("expected preference reverted to auto, got %q", reverted)
	}
}
