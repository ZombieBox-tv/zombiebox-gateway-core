package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/playback"
	"zombiebox.local/gateway/internal/providers"
)

type qualityTestMedia struct {
	width, height      int
	codec              string
	profile            string
	remoteProbeFailURL string
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
func (m *qualityTestMedia) ProbeRemote(ctx context.Context, s domain.Source) (domain.Metadata, error) {
	if m.remoteProbeFailURL != "" && strings.Contains(s.URL, m.remoteProbeFailURL) {
		return domain.Metadata{}, errors.New("remote quality probe failed")
	}
	return m.Probe(ctx, s.URL)
}
func (*qualityTestMedia) ConvertRemote(context.Context, domain.Source, string, domain.MediaSelection, io.Writer) error {
	return nil
}

func setDevicePassingProbes(t *testing.T, s *Server, deviceID string, probeIDs ...string) {
	t.Helper()
	var dev domain.Device
	_ = s.db.Get(t.Context(), "devices", deviceID, &dev)
	dev.Capabilities.Probes = nil
	for _, p := range probeIDs {
		dev.Capabilities.Probes = append(dev.Capabilities.Probes, domain.Probe{
			ID:         p,
			Status:     "PASS",
			Completed:  true,
			PositionMS: 1000,
			TestedAt:   time.Now().Unix(),
		})
	}
	_ = s.db.Put(t.Context(), "devices", deviceID, dev)
}

func assertQualitySelectionFailurePreservesSession(t *testing.T, s *Server, deviceID, provider, token, sessionID, qualityID string, positionMS int64) {
	t.Helper()
	preferenceBefore := s.getQualityPreference(t.Context(), deviceID, provider, "video")
	s.mu.Lock()
	sessionBefore := s.sessions[sessionID]
	sessionCountBefore := len(s.sessions)
	s.mu.Unlock()
	if sessionBefore == nil {
		t.Fatal("expected the active session before manual quality selection")
	}

	requestBody := fmt.Sprintf(`{"qualityId":%q,"positionMs":%d}`, qualityID, positionMS)
	response := call(s, "POST", "/v1/playback/"+sessionID+"/quality", requestBody, deviceID, token, "")
	if response.Code != 502 {
		t.Fatalf("expected 502 for failed manual quality selection, got %d: %s", response.Code, response.Body)
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error.Code != "quality_resolution_failed" {
		t.Fatalf("expected typed quality_resolution_failed response, got %q (decode error %v)", response.Body.String(), err)
	}

	s.mu.Lock()
	sessionAfter := s.sessions[sessionID]
	sessionCountAfter := len(s.sessions)
	s.mu.Unlock()
	if sessionAfter != sessionBefore || sessionAfter.ctx.Err() != nil || sessionAfter.supersededBy != "" {
		t.Fatalf("failed manual selection changed or retired the active session: before=%+v after=%+v", sessionBefore, sessionAfter)
	}
	if sessionCountAfter != sessionCountBefore {
		t.Fatalf("failed manual selection created a replacement session: before=%d after=%d", sessionCountBefore, sessionCountAfter)
	}
	if preferenceAfter := s.getQualityPreference(t.Context(), deviceID, provider, "video"); preferenceAfter != preferenceBefore {
		t.Fatalf("failed manual selection changed stored preference from %q to %q", preferenceBefore, preferenceAfter)
	}
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
	pref := s.getQualityPreference(t.Context(), "pref-device", "local", "video")
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

func TestQualityPreferenceIsProviderScoped(t *testing.T) {
	s := testServer(t, nil, "")
	ctx := t.Context()
	deviceID := "provider-scope-device"

	if err := s.setQualityPreference(ctx, deviceID, "youtube", "video", "720p"); err != nil {
		t.Fatal(err)
	}
	if err := s.setQualityPreference(ctx, deviceID, "local", "video", "480p"); err != nil {
		t.Fatal(err)
	}
	if got := s.getQualityPreference(ctx, deviceID, "youtube", "video"); got != "720p" {
		t.Fatalf("expected YouTube preference 720p, got %q", got)
	}
	if got := s.getQualityPreference(ctx, deviceID, "local", "video"); got != "480p" {
		t.Fatalf("expected local preference 480p, got %q", got)
	}

	if err := s.revertQualityPreference(ctx, deviceID, "youtube", "video"); err != nil {
		t.Fatal(err)
	}
	if got := s.getQualityPreference(ctx, deviceID, "youtube", "video"); got != "auto" {
		t.Fatalf("expected YouTube preference reverted to auto, got %q", got)
	}
	if got := s.getQualityPreference(ctx, deviceID, "local", "video"); got != "480p" {
		t.Fatalf("reverting YouTube preference changed local preference to %q", got)
	}

	legacyDeviceID := "legacy-unattributed-device"
	if err := s.db.Put(ctx, qualityPrefBucket+":"+legacyDeviceID, "video", qualityPreference{
		QualityID: "1080p",
		Kind:      "video",
		UpdatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.getQualityPreference(ctx, legacyDeviceID, "youtube", "video"); got != "" {
		t.Fatalf("unattributed kind-only preference was used for YouTube: %q", got)
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

	// Set 720p quality, creating a replacement session.
	quality := call(s, "POST", "/v1/playback/"+plan.SessionID+"/quality", `{"qualityId":"720p","positionMs":5000}`, "fail-device", token, "")
	if quality.Code != 201 {
		t.Fatalf("quality selection failed: %d %s", quality.Code, quality.Body)
	}
	var replacement domain.Plan
	if err := json.Unmarshal(quality.Body.Bytes(), &replacement); err != nil {
		t.Fatal(err)
	}
	if s.getQualityPreference(t.Context(), "fail-device", "local", "video") != "720p" {
		t.Fatal("preference was not set")
	}
	if err := s.setQualityPreference(t.Context(), "fail-device", "youtube", "video", "1080p"); err != nil {
		t.Fatal(err)
	}

	// A late failure from the superseded session must not erase the new choice.
	prog := call(s, "PUT", "/v1/playback/"+plan.SessionID+"/progress", `{"state":"FAILED","positionMs":5000,"durationMs":100000}`, "fail-device", token, "")
	if prog.Code != 200 {
		t.Fatalf("progress failed: %d %s", prog.Code, prog.Body)
	}
	if got := s.getQualityPreference(t.Context(), "fail-device", "local", "video"); got != "720p" {
		t.Fatalf("superseded session failure erased 720p preference: %q", got)
	}
	if got := s.getQualityPreference(t.Context(), "fail-device", "youtube", "video"); got != "1080p" {
		t.Fatalf("superseded local session changed YouTube preference: %q", got)
	}

	// Failure of the session using 720p must revert to Auto.
	prog = call(s, "PUT", "/v1/playback/"+replacement.SessionID+"/progress", `{"state":"FAILED","positionMs":5000,"durationMs":100000}`, "fail-device", token, "")
	if prog.Code != 200 {
		t.Fatalf("replacement progress failed: %d %s", prog.Code, prog.Body)
	}
	reverted := s.getQualityPreference(t.Context(), "fail-device", "local", "video")
	if reverted != "auto" {
		t.Fatalf("expected active local preference reverted to auto, got %q", reverted)
	}
	if got := s.getQualityPreference(t.Context(), "fail-device", "youtube", "video"); got != "1080p" {
		t.Fatalf("active local failure changed YouTube preference: %q", got)
	}
}

type mockYouTubeResolver struct {
	resolveFn func(context.Context, domain.Source) (domain.Source, error)
}

func (m *mockYouTubeResolver) Resolve(ctx context.Context, s domain.Source) (domain.Source, error) {
	if m.resolveFn != nil {
		return m.resolveFn(ctx, s)
	}
	return s, nil
}

type transientYouTubeProbeMedia struct {
	*qualityTestMedia
	probeCalls int
}

func (m *transientYouTubeProbeMedia) ProbeRemote(ctx context.Context, source domain.Source) (domain.Metadata, error) {
	m.probeCalls++
	if m.probeCalls == 1 {
		return domain.Metadata{}, errors.New("temporary remote range timeout")
	}
	return m.qualityTestMedia.ProbeRemote(ctx, source)
}

func TestResolveAndProbeYouTubeSourceRetriesTransientManualQualityFailure(t *testing.T) {
	s := testServer(t, nil, "")
	s.deps.RemoteMedia = &qualityTestMedia{width: 1280, height: 720, codec: "h264", profile: "Main"}

	base := domain.Source{
		Item:           domain.Item{ID: "youtube-retryVid123", Provider: "youtube", Kind: "video"},
		URL:            "https://r1.googlevideo.com/video-expired?signature=old",
		Headers:        http.Header{"Authorization": {"stale-token"}},
		AudioURL:       "https://r2.googlevideo.com/audio-expired?signature=old",
		AudioHeaders:   http.Header{"Authorization": {"stale-token"}},
		ResolveURL:     "http://wrapper.local/resolve/retryVid123",
		ResolveHeaders: http.Header{"Authorization": {"wrapper-token"}},
		ResolveQuality: "1080p",
	}

	var requests []domain.Source
	s.deps.Resolver = &mockYouTubeResolver{
		resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
			requests = append(requests, request)
			if len(requests) == 1 {
				return domain.Source{}, errors.New("provider HTTP 502")
			}
			resolved := request
			resolved.URL = "https://r1.googlevideo.com/video-720?signature=fresh"
			resolved.AudioURL = "https://r2.googlevideo.com/audio-140?signature=fresh"
			resolved.MIME = "video/mp4"
			return resolved, nil
		},
	}

	resolved, metadata, ok := s.resolveAndProbeYouTubeSource(context.Background(), base, "720p")
	if !ok {
		t.Fatal("expected the fresh 720p resolution and probe to succeed")
	}
	if len(requests) != 2 {
		t.Fatalf("resolver attempts = %d, want 2", len(requests))
	}
	for attempt, request := range requests {
		if request.ResolveQuality != "720p" {
			t.Errorf("attempt %d quality = %q, want 720p", attempt+1, request.ResolveQuality)
		}
		if request.URL != base.ResolveURL {
			t.Errorf("attempt %d URL = %q, want stable resolver endpoint", attempt+1, request.URL)
		}
		if request.Headers.Get("Authorization") != "wrapper-token" {
			t.Errorf("attempt %d did not restore resolver authorization headers", attempt+1)
		}
		if request.AudioURL != "" || request.AudioHeaders != nil {
			t.Errorf("attempt %d reused stale audio URL or headers", attempt+1)
		}
	}
	if resolved.URL != "https://r1.googlevideo.com/video-720?signature=fresh" || resolved.AudioURL != "https://r2.googlevideo.com/audio-140?signature=fresh" {
		t.Fatalf("returned source is not the fresh exact-tier pair: %+v", resolved)
	}
	if metadata.Streams[0].Height != 720 {
		t.Fatalf("probed height = %d, want 720", metadata.Streams[0].Height)
	}
}

func TestResolveAndProbeYouTubeSourceRetriesTransientProbeFailure(t *testing.T) {
	s := testServer(t, nil, "")
	probeMedia := &transientYouTubeProbeMedia{
		qualityTestMedia: &qualityTestMedia{width: 1280, height: 720, codec: "h264", profile: "Main"},
	}
	s.deps.RemoteMedia = probeMedia

	base := domain.Source{
		Item:       domain.Item{ID: "youtube-probeRetry", Provider: "youtube", Kind: "video"},
		URL:        "http://wrapper.local/resolve/probeRetry",
		ResolveURL: "http://wrapper.local/resolve/probeRetry",
		Variants:   []string{"720p"},
	}
	resolveCalls := 0
	s.deps.Resolver = &mockYouTubeResolver{
		resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
			resolveCalls++
			resolved := request
			resolved.URL = fmt.Sprintf("https://r1.googlevideo.com/video-720-attempt-%d", resolveCalls)
			resolved.AudioURL = fmt.Sprintf("https://r2.googlevideo.com/audio-attempt-%d", resolveCalls)
			resolved.MIME = "video/mp4"
			return resolved, nil
		},
	}

	resolved, _, ok := s.resolveAndProbeYouTubeSource(context.Background(), base, "720p")
	if !ok {
		t.Fatal("expected the fresh second resolve and probe to succeed")
	}
	if resolveCalls != 2 || probeMedia.probeCalls != 2 {
		t.Fatalf("resolve/probe calls = %d/%d, want 2/2", resolveCalls, probeMedia.probeCalls)
	}
	if !strings.Contains(resolved.URL, "attempt-2") || !strings.Contains(resolved.AudioURL, "attempt-2") {
		t.Fatalf("returned a stale source from the failed probe attempt: %+v", resolved)
	}
}

func TestResolveAndProbeYouTubeSourceDoesNotRetryDeterministicFailure(t *testing.T) {
	for _, failure := range []string{"provider HTTP 404", "provider HTTP 501", "video_unavailable"} {
		t.Run(failure, func(t *testing.T) {
			s := testServer(t, nil, "")
			s.deps.RemoteMedia = &qualityTestMedia{width: 1280, height: 720, codec: "h264", profile: "Main"}
			base := domain.Source{
				Item:       domain.Item{ID: "youtube-unavailable", Provider: "youtube", Kind: "video"},
				URL:        "http://wrapper.local/resolve/unavailable",
				ResolveURL: "http://wrapper.local/resolve/unavailable",
			}
			resolveCalls := 0
			s.deps.Resolver = &mockYouTubeResolver{
				resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
					resolveCalls++
					return domain.Source{}, errors.New(failure)
				},
			}

			if _, _, ok := s.resolveAndProbeYouTubeSource(context.Background(), base, "720p"); ok {
				t.Fatal("deterministic quality failure unexpectedly succeeded")
			}
			if resolveCalls != 1 {
				t.Fatalf("resolver calls = %d, want 1 for deterministic failure", resolveCalls)
			}
		})
	}
}

func TestResolveAndProbeYouTubeSourceStopsRetryOnCancellation(t *testing.T) {
	s := testServer(t, nil, "")
	s.deps.RemoteMedia = &qualityTestMedia{width: 1280, height: 720, codec: "h264", profile: "Main"}
	base := domain.Source{
		Item:       domain.Item{ID: "youtube-cancelRetry", Provider: "youtube", Kind: "video"},
		URL:        "http://wrapper.local/resolve/cancelRetry",
		ResolveURL: "http://wrapper.local/resolve/cancelRetry",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolveCalls := 0
	s.deps.Resolver = &mockYouTubeResolver{
		resolveFn: func(_ context.Context, _ domain.Source) (domain.Source, error) {
			resolveCalls++
			cancel()
			return domain.Source{}, errors.New("provider HTTP 502")
		},
	}

	if _, _, ok := s.resolveAndProbeYouTubeSource(ctx, base, "720p"); ok {
		t.Fatal("cancelled resolution unexpectedly succeeded")
	}
	if resolveCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1 after cancellation", resolveCalls)
	}
}

type mockYouTubeCatalog struct {
	Catalog
	source domain.Source
}

func (m *mockYouTubeCatalog) HasCatalog(id string) bool {
	if id == "youtube" {
		return true
	}
	return m.Catalog.HasCatalog(id)
}

func (m *mockYouTubeCatalog) Fetch(ctx context.Context, id string, c domain.Config, mediaDir string) ([]domain.Source, error) {
	if id == "youtube" {
		return []domain.Source{m.source}, nil
	}
	return m.Catalog.Fetch(ctx, id, c, mediaDir)
}

func TestYouTubeHDQualitySelectionAndSwitching(t *testing.T) {
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "http://wrapper.local", Token: strings.Repeat("s", 32)}}); err != nil {
		t.Fatal(err)
	}
	setDeviceSource := func(dev string, src domain.Source) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.searchResults[dev] = searchResult{
			query:    "test",
			revision: s.configRevision["youtube"],
			fetched:  time.Now(),
			sources:  []domain.Source{src},
		}
	}
	mediaStub := &dynamicRemoteMedia{qualityTestMedia: qualityTestMedia{width: 640, height: 360, codec: "h264", profile: "Baseline"}}
	s.deps.Media = mediaStub
	s.deps.RemoteMedia = mediaStub

	ytSource := domain.Source{
		Item:       domain.Item{ID: "youtube-dQw4w9WgXcQ", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/dQw4w9WgXcQ",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"1080p", "720p", "360p"},
		ResolveURL: "http://wrapper.local/resolve/dQw4w9WgXcQ",
	}
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: ytSource}

	mockRes := &mockYouTubeResolver{
		resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
			res := src
			if len(res.Variants) == 0 {
				res.Variants = []string{"1080p", "720p", "360p"}
			}
			if src.ResolveQuality == "720p" {
				res.URL = "https://r1.googlevideo.com/video-720"
				res.AudioURL = "https://r2.googlevideo.com/audio-140"
				res.MIME = "video/mp4"
				return res, nil
			}
			if src.ResolveQuality == "1080p" {
				res.URL = "https://r1.googlevideo.com/video-1080"
				res.AudioURL = "https://r2.googlevideo.com/audio-140"
				res.MIME = "video/mp4"
				return res, nil
			}
			res.URL = "https://r1.googlevideo.com/video-360"
			res.AudioURL = ""
			res.MIME = "video/mp4"
			return res, nil
		},
	}
	s.deps.Resolver = mockRes

	token := pair(t, s, "yt-device")
	setDevicePassingProbes(t, s, "yt-device", append(standardCapableProbes(), "http-progressive")...)
	setDeviceSource("yt-device", ytSource)

	// 1. Valid HD: Initial playback starts in Auto (360p progressive stream)
	playReq := `{"itemId":"youtube-dQw4w9WgXcQ"}`
	w := call(s, "POST", "/v1/playback", playReq, "yt-device", token, "")
	if w.Code != 201 {
		t.Fatalf("playback creation failed: %d %s", w.Code, w.Body)
	}
	var plan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &plan)
	if plan.Mode != "DIRECT_PLAY" || plan.PrepareBeforePlayback {
		t.Fatalf("initial Auto mode should be unprepared DIRECT_PLAY for 360p progressive stream, got %+v", plan)
	}

	// GET /v1/playback/{session}/qualities lists available validated variants intersected with device probes
	qResp := call(s, "GET", "/v1/playback/"+plan.SessionID+"/qualities", "", "yt-device", token, "")
	if qResp.Code != 200 {
		t.Fatalf("qualities returned %d: %s", qResp.Code, qResp.Body)
	}
	var inv domain.QualityInventory
	json.Unmarshal(qResp.Body.Bytes(), &inv)
	if !playback.HasQuality(inv, "1080p") || !playback.HasQuality(inv, "720p") || !playback.HasQuality(inv, "360p") {
		t.Fatalf("expected 1080p, 720p, 360p in inventory, got: %+v", inv.Options)
	}

	// Select 720p at position 25000ms: switches to H.264/AAC pair at current position
	switchBody := `{"qualityId":"720p","positionMs":25000}`
	switchCall := call(s, "POST", "/v1/playback/"+plan.SessionID+"/quality", switchBody, "yt-device", token, "")
	if switchCall.Code != 201 {
		t.Fatalf("select 720p returned %d: %s", switchCall.Code, switchCall.Body)
	}
	var plan720 domain.Plan
	json.Unmarshal(switchCall.Body.Bytes(), &plan720)
	if plan720.SessionID == plan.SessionID {
		t.Fatal("expected new session ID for replacement")
	}
	if plan720.Mode != "TRANSCODE" {
		t.Fatalf("expected TRANSCODE for seeking H.264/AAC separate inputs, got %s", plan720.Mode)
	}
	if plan720.TimelineOffsetMS != 25000 {
		t.Fatalf("expected timelineOffsetMs 25000, got %d", plan720.TimelineOffsetMS)
	}

	s.mu.Lock()
	sess720 := s.sessions[plan720.SessionID]
	s.mu.Unlock()
	if sess720 == nil || sess720.source.AudioURL != "https://r2.googlevideo.com/audio-140" || sess720.source.URL != "https://r1.googlevideo.com/video-720" {
		t.Fatalf("replacement session does not hold resolved H.264/AAC pair: %+v", sess720)
	}
	if sess720.selection.Quality != "720p" || s.getQualityPreference(t.Context(), "yt-device", "youtube", "video") != "720p" {
		t.Fatalf("successful manual quality was not retained: session=%+v preference=%q", sess720.selection, s.getQualityPreference(t.Context(), "yt-device", "youtube", "video"))
	}

	// 2. Source lacking HD: only 360p variant
	ytSDSource := domain.Source{
		Item:       domain.Item{ID: "youtube-sdOnly", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/sdOnly",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"360p"},
		ResolveURL: "http://wrapper.local/resolve/sdOnly",
	}
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: ytSDSource}
	setDeviceSource("yt-device", ytSDSource)
	wSD := call(s, "POST", "/v1/playback", `{"itemId":"youtube-sdOnly"}`, "yt-device", token, "")
	if wSD.Code != 201 {
		t.Fatalf("sd playback failed: %d %s", wSD.Code, wSD.Body)
	}
	var planSD domain.Plan
	json.Unmarshal(wSD.Body.Bytes(), &planSD)

	qSDResp := call(s, "GET", "/v1/playback/"+planSD.SessionID+"/qualities", "", "yt-device", token, "")
	var invSD domain.QualityInventory
	json.Unmarshal(qSDResp.Body.Bytes(), &invSD)
	if playback.HasQuality(invSD, "720p") || playback.HasQuality(invSD, "1080p") {
		t.Fatalf("source lacking HD must not offer 720p/1080p: %+v", invSD.Options)
	}

	// Attempting to select 720p on SD-only source returns 409
	badSwitch := call(s, "POST", "/v1/playback/"+planSD.SessionID+"/quality", `{"qualityId":"720p","positionMs":5000}`, "yt-device", token, "")
	if badSwitch.Code != 409 {
		t.Fatalf("expected 409 for selecting unavailable quality on SD source, got %d", badSwitch.Code)
	}

	// 3. Stale/failed probe on device: 1080p probe fails
	failDevToken := pair(t, s, "fail-probe-dev")
	setDevicePassingProbes(t, s, "fail-probe-dev",
		"http-fmp4", "aac", "h264-baseline-360", "h264-720-main",
	) // no 1080p probe
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: ytSource}
	setDeviceSource("fail-probe-dev", ytSource)
	wHD := call(s, "POST", "/v1/playback", playReq, "fail-probe-dev", failDevToken, "")
	if wHD.Code != 201 {
		t.Fatalf("fail-probe-dev playback failed: %d %s", wHD.Code, wHD.Body)
	}
	var planHD domain.Plan
	json.Unmarshal(wHD.Body.Bytes(), &planHD)

	qFailResp := call(s, "GET", "/v1/playback/"+planHD.SessionID+"/qualities", "", "fail-probe-dev", failDevToken, "")
	var invFail domain.QualityInventory
	json.Unmarshal(qFailResp.Body.Bytes(), &invFail)
	if playback.HasQuality(invFail, "1080p") {
		t.Fatalf("device without 1080p passing probe must not see 1080p: %+v", invFail.Options)
	}
	if !playback.HasQuality(invFail, "720p") {
		t.Fatalf("device with 720p probe should see 720p: %+v", invFail.Options)
	}

	// 4. Missing/failed audio probe prevents offering adaptive HD
	noAudioDevToken := pair(t, s, "no-audio-dev")
	setDevicePassingProbes(t, s, "no-audio-dev",
		"http-fmp4", "h264-baseline-360", "h264-720-main", "h264-1080-high",
	) // missing aac probe
	setDeviceSource("no-audio-dev", ytSource)
	wNoAudio := call(s, "POST", "/v1/playback", playReq, "no-audio-dev", noAudioDevToken, "")
	if wNoAudio.Code != 201 {
		t.Fatalf("no-audio-dev playback failed: %d %s", wNoAudio.Code, wNoAudio.Body)
	}
	var planNoAudio domain.Plan
	json.Unmarshal(wNoAudio.Body.Bytes(), &planNoAudio)
	qNoAudioResp := call(s, "GET", "/v1/playback/"+planNoAudio.SessionID+"/qualities", "", "no-audio-dev", noAudioDevToken, "")
	var invNoAudio domain.QualityInventory
	json.Unmarshal(qNoAudioResp.Body.Bytes(), &invNoAudio)
	if playback.HasQuality(invNoAudio, "720p") || playback.HasQuality(invNoAudio, "1080p") {
		t.Fatalf("device without aac probe must not be offered adaptive HD: %+v", invNoAudio.Options)
	}

	// 5. A failed manual tier resolution leaves the current session and preference intact.
	failRes := &mockYouTubeResolver{
		resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
			if src.ResolveQuality == "720p" {
				return src, errors.New("upstream wrapper timeout / 502")
			}
			res := src
			res.URL = "https://r1.googlevideo.com/video-360"
			res.MIME = "video/mp4"
			res.Variants = []string{"1080p", "720p", "360p"}
			res.ResolveURL = "http://wrapper.local/resolve/dQw4w9WgXcQ"
			return res, nil
		},
	}
	s.deps.Resolver = failRes
	setDeviceSource("yt-device", ytSource)
	wFallback := call(s, "POST", "/v1/playback", playReq, "yt-device", token, "")
	if wFallback.Code != 201 {
		t.Fatalf("fallback playback failed: %d %s", wFallback.Code, wFallback.Body)
	}
	var planFB domain.Plan
	json.Unmarshal(wFallback.Body.Bytes(), &planFB)

	if err := s.setQualityPreference(t.Context(), "yt-device", "youtube", "video", "1080p"); err != nil {
		t.Fatal(err)
	}
	assertQualitySelectionFailurePreservesSession(t, s, "yt-device", "youtube", token, planFB.SessionID, "720p", 12000)

	// 6. In-place switch: switch to 720p and back to Auto at preserved positions
	s.deps.Resolver = mockRes // restore working resolver
	setDeviceSource("yt-device", ytSource)
	wSwitch := call(s, "POST", "/v1/playback", playReq, "yt-device", token, "")
	if wSwitch.Code != 201 {
		t.Fatalf("switch playback failed: %d %s", wSwitch.Code, wSwitch.Body)
	}
	var planOrig domain.Plan
	json.Unmarshal(wSwitch.Body.Bytes(), &planOrig)

	to720 := call(s, "POST", "/v1/playback/"+planOrig.SessionID+"/quality", `{"qualityId":"720p","positionMs":30000}`, "yt-device", token, "")
	if to720.Code != 201 {
		t.Fatalf("switch to 720p failed: %d %s", to720.Code, to720.Body)
	}
	var planSwitched720 domain.Plan
	json.Unmarshal(to720.Body.Bytes(), &planSwitched720)
	if planSwitched720.TimelineOffsetMS != 30000 || planSwitched720.Mode != "TRANSCODE" {
		t.Fatalf("unexpected 720p plan: %+v", planSwitched720)
	}

	// Switch from 720p back to Auto at position 45000ms:
	toAuto := call(s, "POST", "/v1/playback/"+planSwitched720.SessionID+"/quality", `{"qualityId":"auto","positionMs":45000}`, "yt-device", token, "")
	if toAuto.Code != 201 {
		t.Fatalf("switch back to auto failed: %d %s", toAuto.Code, toAuto.Body)
	}
	var planBackAuto domain.Plan
	json.Unmarshal(toAuto.Body.Bytes(), &planBackAuto)
	if planBackAuto.Mode != "DIRECT_PLAY" || planBackAuto.ResumeMS != 45000 || !planBackAuto.Seekable {
		t.Fatalf("expected DIRECT_PLAY at resume 45000ms for Auto progressive stream, got: %+v", planBackAuto)
	}

	// 7. Initial playback request with explicit HD quality
	setDeviceSource("yt-device", ytSource)
	wInit720 := call(s, "POST", "/v1/playback", `{"itemId":"youtube-dQw4w9WgXcQ","quality":"720p"}`, "yt-device", token, "")
	if wInit720.Code != 201 {
		t.Fatalf("explicit 720p playback creation failed: %d %s", wInit720.Code, wInit720.Body)
	}
	var planInit720 domain.Plan
	json.Unmarshal(wInit720.Body.Bytes(), &planInit720)
	if planInit720.Mode != "REMUX" && planInit720.Mode != "TRANSCODE" {
		t.Fatalf("expected REMUX or TRANSCODE for adaptive YouTube pair, got %s", planInit720.Mode)
	}
	s.mu.Lock()
	sessInit720 := s.sessions[planInit720.SessionID]
	s.mu.Unlock()
	if sessInit720 == nil || sessInit720.source.URL != "https://r1.googlevideo.com/video-720" {
		t.Fatalf("expected resolved 720p stream on initial creation, got: %+v", sessInit720)
	}
}

func TestSelectYouTubeQualityRemoteProbeFailurePreservesSession(t *testing.T) {
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "http://wrapper.local", Token: strings.Repeat("p", 32)}}); err != nil {
		t.Fatal(err)
	}

	deviceID := "probe-failure-device"
	token := pair(t, s, deviceID)
	setDevicePassingProbes(t, s, deviceID, standardCapableProbes()...)
	source := domain.Source{
		Item:       domain.Item{ID: "youtube-probe-failure", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/probe-failure",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"1080p", "720p", "360p"},
		ResolveURL: "http://wrapper.local/resolve/probe-failure",
	}
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: source}
	setDeviceSource := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.searchResults[deviceID] = searchResult{
			query:    "probe failure",
			revision: s.configRevision["youtube"],
			fetched:  time.Now(),
			sources:  []domain.Source{source},
		}
	}
	setDeviceSource()

	mediaStub := &qualityTestMedia{width: 640, height: 360, codec: "h264", profile: "Baseline"}
	s.deps.Media = mediaStub
	s.deps.RemoteMedia = mediaStub
	s.deps.Resolver = &mockYouTubeResolver{
		resolveFn: func(_ context.Context, src domain.Source) (domain.Source, error) {
			resolved := src
			resolved.MIME = "video/mp4"
			if src.ResolveQuality == "720p" {
				resolved.URL = "https://r1.googlevideo.com/video-720"
				resolved.AudioURL = "https://r2.googlevideo.com/audio-140"
			} else {
				resolved.URL = "https://r1.googlevideo.com/video-360"
				resolved.AudioURL = ""
			}
			return resolved, nil
		},
	}

	start := call(s, "POST", "/v1/playback", `{"itemId":"youtube-probe-failure"}`, deviceID, token, "")
	if start.Code != 201 {
		t.Fatalf("initial playback returned %d: %s", start.Code, start.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(start.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}

	// Fail the selected tier's required media probe after the current Auto session exists.
	s.deps.RemoteMedia = &qualityTestMedia{
		width:              640,
		height:             360,
		codec:              "h264",
		profile:            "Baseline",
		remoteProbeFailURL: "video-720",
	}
	if err := s.setQualityPreference(t.Context(), deviceID, "youtube", "video", "1080p"); err != nil {
		t.Fatal(err)
	}
	assertQualitySelectionFailurePreservesSession(t, s, deviceID, "youtube", token, plan.SessionID, "720p", 12000)
}

type dynamicRemoteMedia struct {
	qualityTestMedia
}

func (d *dynamicRemoteMedia) ProbeRemote(ctx context.Context, src domain.Source) (domain.Metadata, error) {
	if strings.Contains(src.URL, "video-720") {
		meta := domain.Metadata{
			Streams: []domain.Stream{
				{Index: 0, Type: "video", Codec: "h264", Profile: "Main", Width: 1280, Height: 720},
				{Index: 1, Type: "audio", Codec: "aac"},
			},
		}
		meta.Format.Name = "mp4"
		return meta, nil
	}
	meta := domain.Metadata{
		Streams: []domain.Stream{
			{Index: 0, Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
			{Index: 1, Type: "audio", Codec: "aac"},
		},
	}
	meta.Format.Name = "mp4"
	return meta, nil
}

func TestYouTubeFailedHDResolveOnInitialAndInPlaceSessions(t *testing.T) {
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "http://wrapper.local", Token: strings.Repeat("s", 32)}}); err != nil {
		t.Fatal(err)
	}

	dynMedia := &dynamicRemoteMedia{}
	s.deps.Media = dynMedia
	s.deps.RemoteMedia = dynMedia

	ytSource := domain.Source{
		Item:       domain.Item{ID: "youtube-testVideo1", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/testVideo1",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"1080p", "720p", "360p"},
		ResolveURL: "http://wrapper.local/resolve/testVideo1",
	}
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: ytSource}

	setDeviceSource := func(dev string, src domain.Source) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.searchResults[dev] = searchResult{
			query:    "test",
			revision: s.configRevision["youtube"],
			fetched:  time.Now(),
			sources:  []domain.Source{src},
		}
	}

	token := pair(t, s, "fail-hd-device")
	setDevicePassingProbes(t, s, "fail-hd-device", append(standardCapableProbes(), "http-progressive")...)
	setDeviceSource("fail-hd-device", ytSource)

	// Resolver that fails on 720p/1080p, but succeeds on Auto (360p)
	failingHDResolver := &mockYouTubeResolver{
		resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
			if src.ResolveQuality == "720p" || src.ResolveQuality == "1080p" {
				return src, errors.New("upstream wrapper 502: audio track failed")
			}
			res := src
			res.URL = "https://r1.googlevideo.com/video-360"
			res.AudioURL = ""
			res.MIME = "video/mp4"
			res.Variants = []string{"1080p", "720p", "360p"}
			res.ResolveURL = "http://wrapper.local/resolve/testVideo1"
			return res, nil
		},
	}
	s.deps.Resolver = failingHDResolver

	// 1. Initial playback with stored preferred quality "720p":
	// When HD resolve fails, gateway must genuinely fall back to Auto (DIRECT_PLAY 360p plan),
	// revert preference to Auto, and NOT report a 720p plan!
	_ = s.setQualityPreference(context.Background(), "fail-hd-device", "youtube", "video", "720p")

	wPref := call(s, "POST", "/v1/playback", `{"itemId":"youtube-testVideo1"}`, "fail-hd-device", token, "")
	if wPref.Code != 201 {
		t.Fatalf("initial playback with failing preferred HD failed: %d %s", wPref.Code, wPref.Body)
	}
	var planPref domain.Plan
	json.Unmarshal(wPref.Body.Bytes(), &planPref)

	if planPref.Mode != "DIRECT_PLAY" {
		t.Fatalf("expected fallback to DIRECT_PLAY on failed preferred HD resolve, got mode %s", planPref.Mode)
	}
	s.mu.Lock()
	sessPref := s.sessions[planPref.SessionID]
	s.mu.Unlock()
	if sessPref.selection.Quality != "" && sessPref.selection.Quality != "auto" {
		t.Fatalf("session quality should be empty/auto on fallback, got %q", sessPref.selection.Quality)
	}
	prefAfter := s.getQualityPreference(context.Background(), "fail-hd-device", "youtube", "video")
	if prefAfter != "auto" && prefAfter != "" {
		t.Fatalf("expected preference reverted to auto, got %q", prefAfter)
	}

	// 2. An explicit manual quality failure must not silently start Auto.
	wExplicit := call(s, "POST", "/v1/playback", `{"itemId":"youtube-testVideo1","quality":"720p"}`, "fail-hd-device", token, "")
	if wExplicit.Code != 502 || !strings.Contains(wExplicit.Body.String(), "quality_resolution_failed") {
		t.Fatalf("expected initial manual quality failure, got %d %s", wExplicit.Code, wExplicit.Body)
	}
	s.mu.Lock()
	sessExplicit := s.sessions[planPref.SessionID]
	sessionCount := len(s.sessions)
	s.mu.Unlock()
	if sessExplicit == nil || sessExplicit.ctx.Err() != nil || sessionCount != 1 {
		t.Fatalf("failed initial manual tier changed the active session set: session=%+v count=%d", sessExplicit, sessionCount)
	}

	// 3. A failed in-place manual switch returns an error and leaves the current
	// Auto playback active. Auto fallback is reserved for explicit Auto selection.
	if err := s.setQualityPreference(t.Context(), "fail-hd-device", "youtube", "video", "1080p"); err != nil {
		t.Fatal(err)
	}
	assertQualitySelectionFailurePreservesSession(t, s, "fail-hd-device", "youtube", token, planPref.SessionID, "720p", 15000)

	// 4. In-place switch succeeds: resolver returns 720p, metadata is updated coherently
	workingResolver := &mockYouTubeResolver{
		resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
			res := src
			if src.ResolveQuality == "720p" {
				res.URL = "https://r1.googlevideo.com/video-720"
				res.AudioURL = "https://r2.googlevideo.com/audio-aac"
				res.MIME = "video/mp4"
				return res, nil
			}
			res.URL = "https://r1.googlevideo.com/video-360"
			res.AudioURL = ""
			res.MIME = "video/mp4"
			return res, nil
		},
	}
	s.deps.Resolver = workingResolver

	wOK := call(s, "POST", "/v1/playback/"+planPref.SessionID+"/quality", `{"qualityId":"720p","positionMs":22000}`, "fail-hd-device", token, "")
	if wOK.Code != 201 {
		t.Fatalf("working switch to 720p failed: %d %s", wOK.Code, wOK.Body)
	}
	var planOK domain.Plan
	json.Unmarshal(wOK.Body.Bytes(), &planOK)
	if planOK.Mode != "TRANSCODE" || planOK.TimelineOffsetMS != 22000 {
		t.Fatalf("expected TRANSCODE at 22000ms for seeking split pair, got %+v", planOK)
	}

	s.mu.Lock()
	sessOK := s.sessions[planOK.SessionID]
	s.mu.Unlock()
	if sessOK.metadata == nil {
		t.Fatal("session metadata must not be nil")
	}
	var vidHeight int
	for _, stream := range sessOK.metadata.Streams {
		if stream.Type == "video" {
			vidHeight = stream.Height
		}
	}
	if vidHeight != 720 {
		t.Fatalf("expected coherent probed 720p metadata height, got %d", vidHeight)
	}

	// 5. In-place switch from 720p back to Auto at 35000ms
	wBack := call(s, "POST", "/v1/playback/"+planOK.SessionID+"/quality", `{"qualityId":"auto","positionMs":35000}`, "fail-hd-device", token, "")
	if wBack.Code != 201 {
		t.Fatalf("switch back to auto failed: %d %s", wBack.Code, wBack.Body)
	}
	var planBack domain.Plan
	json.Unmarshal(wBack.Body.Bytes(), &planBack)
	if planBack.Mode != "DIRECT_PLAY" || planBack.ResumeMS != 35000 || !planBack.Seekable {
		t.Fatalf("expected DIRECT_PLAY at 35000ms for Auto progressive stream, got %+v", planBack)
	}
	s.mu.Lock()
	sessBack := s.sessions[planBack.SessionID]
	s.mu.Unlock()
	if sessBack.source.AudioURL != "" {
		t.Fatalf("expected Auto progressive stream without audioUrl, got %s", sessBack.source.AudioURL)
	}
}

func TestInitialManualYouTubeQualityResolutionFailureReturnsError(t *testing.T) {
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "http://wrapper.local", Token: strings.Repeat("s", 32)}}); err != nil {
		t.Fatal(err)
	}
	deviceID := "initial-quality-resolution-failure"
	token := pair(t, s, deviceID)
	setDevicePassingProbes(t, s, deviceID, standardCapableProbes()...)
	source := domain.Source{
		Item:       domain.Item{ID: "youtube-initial-quality-failure", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/initial-quality-failure",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"1080p", "720p", "360p"},
		ResolveURL: "http://wrapper.local/resolve/initial-quality-failure",
	}
	s.mu.Lock()
	s.searchResults[deviceID] = searchResult{
		revision: s.configRevision["youtube"],
		fetched:  time.Now(),
		sources:  []domain.Source{source},
	}
	s.mu.Unlock()
	mediaStub := &qualityTestMedia{width: 1920, height: 1080, codec: "h264", profile: "High"}
	s.deps.Media = mediaStub
	s.deps.RemoteMedia = mediaStub
	s.deps.Resolver = &mockYouTubeResolver{
		resolveFn: func(_ context.Context, src domain.Source) (domain.Source, error) {
			if src.ResolveQuality == "720p" {
				return domain.Source{}, errors.New("upstream quality resolution failed")
			}
			resolved := src
			resolved.URL = "https://r1.googlevideo.com/video-360"
			resolved.AudioURL = ""
			resolved.MIME = "video/mp4"
			return resolved, nil
		},
	}

	response := call(s, "POST", "/v1/playback", `{"itemId":"youtube-initial-quality-failure","quality":"720p"}`, deviceID, token, "")
	if response.Code != 502 {
		t.Fatalf("expected 502 for failed explicit initial quality, got %d: %s", response.Code, response.Body)
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error.Code != "quality_resolution_failed" {
		t.Fatalf("expected quality_resolution_failed, got %q (decode error %v)", response.Body.String(), err)
	}
	s.mu.Lock()
	count := len(s.sessions)
	s.mu.Unlock()
	if count != 0 {
		t.Fatalf("failed initial manual selection created %d playback sessions", count)
	}
}

type probeFailingRemoteMedia struct {
	dynamicRemoteMedia
	failHD  bool
	failAll bool
}

func (m *probeFailingRemoteMedia) ProbeRemote(ctx context.Context, src domain.Source) (domain.Metadata, error) {
	if m.failAll {
		return domain.Metadata{}, errors.New("remote probe failure: all failed")
	}
	if m.failHD && (strings.Contains(src.URL, "video-720") || strings.Contains(src.URL, "video-1080")) {
		return domain.Metadata{}, errors.New("remote probe failure: HD stream corrupt")
	}
	meta := domain.Metadata{
		Streams: []domain.Stream{
			{Index: 0, Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
			{Index: 1, Type: "audio", Codec: "aac"},
		},
	}
	meta.Format.Name = "mp4"
	return meta, nil
}

func TestYouTubeRemoteMediaProbeFailureOnHDQualitySelection(t *testing.T) {
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "http://wrapper.local", Token: strings.Repeat("s", 32)}}); err != nil {
		t.Fatal(err)
	}

	failingMedia := &probeFailingRemoteMedia{failHD: true}
	s.deps.Media = failingMedia
	s.deps.RemoteMedia = failingMedia

	ytSource := domain.Source{
		Item:       domain.Item{ID: "youtube-probeFailVid", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/probeFailVid",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"1080p", "720p", "360p"},
		ResolveURL: "http://wrapper.local/resolve/probeFailVid",
	}
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: ytSource}

	token := pair(t, s, "probe-fail-device")
	setDevicePassingProbes(t, s, "probe-fail-device", append(standardCapableProbes(), "http-progressive")...)

	s.mu.Lock()
	s.searchResults["probe-fail-device"] = searchResult{
		query:    "test",
		revision: s.configRevision["youtube"],
		fetched:  time.Now(),
		sources:  []domain.Source{ytSource},
	}
	s.mu.Unlock()

	// Resolver returns valid URLs for 720p, but RemoteMedia probe will fail on them!
	resolver := &mockYouTubeResolver{
		resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
			res := src
			if src.ResolveQuality == "720p" {
				res.URL = "https://r1.googlevideo.com/video-720"
				res.AudioURL = "https://r2.googlevideo.com/audio-aac"
				res.MIME = "video/mp4"
				return res, nil
			}
			res.URL = "https://r1.googlevideo.com/video-360"
			res.AudioURL = ""
			res.MIME = "video/mp4"
			return res, nil
		},
	}
	s.deps.Resolver = resolver

	// 1. Initial playback with stored preferred HD ("720p"):
	// Upstream resolve succeeds, but RemoteMedia probe fails on the 720p stream.
	// Server must revert preference, restore original 360p source, and return DIRECT_PLAY 360p.
	_ = s.setQualityPreference(context.Background(), "probe-fail-device", "youtube", "video", "720p")

	wPref := call(s, "POST", "/v1/playback", `{"itemId":"youtube-probeFailVid"}`, "probe-fail-device", token, "")
	if wPref.Code != 201 {
		t.Fatalf("expected 201 on fallback, got %d %s", wPref.Code, wPref.Body)
	}
	var planPref domain.Plan
	json.Unmarshal(wPref.Body.Bytes(), &planPref)
	if planPref.Mode != "DIRECT_PLAY" {
		t.Fatalf("expected fallback to DIRECT_PLAY on probe failure, got %s", planPref.Mode)
	}
	prefAfter := s.getQualityPreference(context.Background(), "probe-fail-device", "youtube", "video")
	if prefAfter != "auto" && prefAfter != "" {
		t.Fatalf("expected preference reverted to auto, got %q", prefAfter)
	}

	// 2. An in-place manual selection whose target probe fails must preserve the
	// active session and preference rather than silently replacing it with Auto.
	if err := s.setQualityPreference(t.Context(), "probe-fail-device", "youtube", "video", "1080p"); err != nil {
		t.Fatal(err)
	}
	assertQualitySelectionFailurePreservesSession(t, s, "probe-fail-device", "youtube", token, planPref.SessionID, "720p", 12000)

	// 3. If target and Auto probes would both fail, the manual request still
	// returns the target resolution error without mutating the active session.
	failingMedia.failAll = true
	assertQualitySelectionFailurePreservesSession(t, s, "probe-fail-device", "youtube", token, planPref.SessionID, "1080p", 15000)
}

type metadataVaryingRemoteMedia struct {
	dynamicRemoteMedia
	emptyHD bool
	errHD   bool
}

func (m *metadataVaryingRemoteMedia) ProbeRemote(ctx context.Context, src domain.Source) (domain.Metadata, error) {
	if strings.Contains(src.URL, "video-720") || strings.Contains(src.URL, "video-1080") {
		if m.errHD {
			return domain.Metadata{}, errors.New("explicit probe error: HD probe failed")
		}
		if m.emptyHD {
			return domain.Metadata{}, nil
		}
	}
	return m.dynamicRemoteMedia.ProbeRemote(ctx, src)
}

func TestYouTubeHDProbeValidationVariants(t *testing.T) {
	setupServer := func() (*Server, string, domain.Source) {
		s := testServer(t, nil, "")
		if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "http://wrapper.local", Token: strings.Repeat("s", 32)}}); err != nil {
			t.Fatal(err)
		}
		ytSource := domain.Source{
			Item:       domain.Item{ID: "youtube-probeVarVid", Provider: "youtube", Kind: "video", Playable: true},
			URL:        "http://wrapper.local/resolve/probeVarVid",
			MIME:       "application/x-zombie-youtube",
			Variants:   []string{"1080p", "720p", "360p"},
			ResolveURL: "http://wrapper.local/resolve/probeVarVid",
		}
		s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: ytSource}
		token := pair(t, s, "probe-var-device")
		setDevicePassingProbes(t, s, "probe-var-device", append(standardCapableProbes(), "http-progressive")...)
		s.mu.Lock()
		s.searchResults["probe-var-device"] = searchResult{
			query:    "test",
			revision: s.configRevision["youtube"],
			fetched:  time.Now(),
			sources:  []domain.Source{ytSource},
		}
		s.mu.Unlock()
		return s, token, ytSource
	}

	t.Run("nil adapter rejects manual tier without replacing Auto", func(t *testing.T) {
		s, token, _ := setupServer()
		mediaStub := &dynamicRemoteMedia{}
		s.deps.Media = mediaStub
		s.deps.RemoteMedia = mediaStub
		s.deps.Resolver = &mockYouTubeResolver{
			resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
				res := src
				if src.ResolveQuality == "720p" {
					res.URL = "https://r1.googlevideo.com/video-720"
					res.AudioURL = "https://r2.googlevideo.com/audio-aac"
					res.MIME = "video/mp4"
					return res, nil
				}
				res.URL = "https://r1.googlevideo.com/video-360"
				res.AudioURL = ""
				res.MIME = "video/mp4"
				return res, nil
			},
		}

		// Initial playback in Auto
		w := call(s, "POST", "/v1/playback", `{"itemId":"youtube-probeVarVid"}`, "probe-var-device", token, "")
		if w.Code != 201 {
			t.Fatalf("playback failed: %d %s", w.Code, w.Body)
		}
		var plan domain.Plan
		json.Unmarshal(w.Body.Bytes(), &plan)

		// Set RemoteMedia adapter to nil
		s.deps.RemoteMedia = nil

		if err := s.setQualityPreference(t.Context(), "probe-var-device", "youtube", "video", "1080p"); err != nil {
			t.Fatal(err)
		}
		assertQualitySelectionFailurePreservesSession(t, s, "probe-var-device", "youtube", token, plan.SessionID, "720p", 8000)
	})

	t.Run("ineligible remote candidate rejects manual tier", func(t *testing.T) {
		s, token, _ := setupServer()
		mediaStub := &dynamicRemoteMedia{}
		s.deps.Media = mediaStub
		s.deps.RemoteMedia = mediaStub
		s.deps.Resolver = &mockYouTubeResolver{
			resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
				res := src
				if src.ResolveQuality == "720p" {
					res.URL = "ftp://ineligible.origin/video-720"
					res.AudioURL = ""
					res.MIME = "video/mp4"
					return res, nil
				}
				res.URL = "https://r1.googlevideo.com/video-360"
				res.AudioURL = ""
				res.MIME = "video/mp4"
				return res, nil
			},
		}

		w := call(s, "POST", "/v1/playback", `{"itemId":"youtube-probeVarVid"}`, "probe-var-device", token, "")
		if w.Code != 201 {
			t.Fatalf("initial playback failed: %d %s", w.Code, w.Body)
		}
		var plan domain.Plan
		json.Unmarshal(w.Body.Bytes(), &plan)

		if err := s.setQualityPreference(t.Context(), "probe-var-device", "youtube", "video", "1080p"); err != nil {
			t.Fatal(err)
		}
		assertQualitySelectionFailurePreservesSession(t, s, "probe-var-device", "youtube", token, plan.SessionID, "720p", 9000)
	})

	t.Run("empty metadata rejects manual tier", func(t *testing.T) {
		s, token, _ := setupServer()
		varyingMedia := &metadataVaryingRemoteMedia{emptyHD: true}
		s.deps.Media = varyingMedia
		s.deps.RemoteMedia = varyingMedia
		s.deps.Resolver = &mockYouTubeResolver{
			resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
				res := src
				if src.ResolveQuality == "720p" {
					res.URL = "https://r1.googlevideo.com/video-720"
					res.AudioURL = "https://r2.googlevideo.com/audio-aac"
					res.MIME = "video/mp4"
					return res, nil
				}
				res.URL = "https://r1.googlevideo.com/video-360"
				res.AudioURL = ""
				res.MIME = "video/mp4"
				return res, nil
			},
		}

		w := call(s, "POST", "/v1/playback", `{"itemId":"youtube-probeVarVid"}`, "probe-var-device", token, "")
		if w.Code != 201 {
			t.Fatalf("initial playback failed: %d %s", w.Code, w.Body)
		}
		var plan domain.Plan
		json.Unmarshal(w.Body.Bytes(), &plan)

		if err := s.setQualityPreference(t.Context(), "probe-var-device", "youtube", "video", "1080p"); err != nil {
			t.Fatal(err)
		}
		assertQualitySelectionFailurePreservesSession(t, s, "probe-var-device", "youtube", token, plan.SessionID, "720p", 10000)
	})

	t.Run("explicit probe error rejects manual tier", func(t *testing.T) {
		s, token, _ := setupServer()
		varyingMedia := &metadataVaryingRemoteMedia{errHD: true}
		s.deps.Media = varyingMedia
		s.deps.RemoteMedia = varyingMedia
		s.deps.Resolver = &mockYouTubeResolver{
			resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
				res := src
				if src.ResolveQuality == "720p" {
					res.URL = "https://r1.googlevideo.com/video-720"
					res.AudioURL = "https://r2.googlevideo.com/audio-aac"
					res.MIME = "video/mp4"
					return res, nil
				}
				res.URL = "https://r1.googlevideo.com/video-360"
				res.AudioURL = ""
				res.MIME = "video/mp4"
				return res, nil
			},
		}

		w := call(s, "POST", "/v1/playback", `{"itemId":"youtube-probeVarVid"}`, "probe-var-device", token, "")
		if w.Code != 201 {
			t.Fatalf("initial playback failed: %d %s", w.Code, w.Body)
		}
		var plan domain.Plan
		json.Unmarshal(w.Body.Bytes(), &plan)

		if err := s.setQualityPreference(t.Context(), "probe-var-device", "youtube", "video", "1080p"); err != nil {
			t.Fatal(err)
		}
		assertQualitySelectionFailurePreservesSession(t, s, "probe-var-device", "youtube", token, plan.SessionID, "720p", 11000)
	})
}

type failingAutoRemoteMedia struct {
	dynamicRemoteMedia
	failAuto bool
}

func (m *failingAutoRemoteMedia) ProbeRemote(ctx context.Context, src domain.Source) (domain.Metadata, error) {
	if m.failAuto && strings.Contains(src.URL, "video-360") {
		return domain.Metadata{}, errors.New("auto probe failed")
	}
	return m.dynamicRemoteMedia.ProbeRemote(ctx, src)
}

func TestYouTubeCombinedHDSessionSwitchToAuto(t *testing.T) {
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "http://wrapper.local", Token: strings.Repeat("s", 32)}}); err != nil {
		t.Fatal(err)
	}

	mediaStub := &failingAutoRemoteMedia{}
	s.deps.Media = mediaStub
	s.deps.RemoteMedia = mediaStub

	ytSource := domain.Source{
		Item:       domain.Item{ID: "youtube-combinedHD", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/combinedHD",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"1080p", "720p", "360p"},
		ResolveURL: "http://wrapper.local/resolve/combinedHD",
	}
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: ytSource}

	token := pair(t, s, "combined-hd-device")
	setDevicePassingProbes(t, s, "combined-hd-device", append(standardCapableProbes(), "http-progressive")...)

	s.mu.Lock()
	s.searchResults["combined-hd-device"] = searchResult{
		query:    "test",
		revision: s.configRevision["youtube"],
		fetched:  time.Now(),
		sources:  []domain.Source{ytSource},
	}
	s.mu.Unlock()

	// Resolver returns COMBINED HD for 720p (AudioURL is empty!)
	s.deps.Resolver = &mockYouTubeResolver{
		resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
			res := src
			if src.ResolveQuality == "720p" {
				res.URL = "https://r1.googlevideo.com/video-720"
				res.AudioURL = "" // COMBINED HD
				res.MIME = "video/mp4"
				return res, nil
			}
			res.URL = "https://r1.googlevideo.com/video-360"
			res.AudioURL = ""
			res.MIME = "video/mp4"
			return res, nil
		},
	}

	// 1. Start playback and switch to 720p combined
	w := call(s, "POST", "/v1/playback", `{"itemId":"youtube-combinedHD"}`, "combined-hd-device", token, "")
	if w.Code != 201 {
		t.Fatalf("playback creation failed: %d %s", w.Code, w.Body)
	}
	var initPlan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &initPlan)

	w720 := call(s, "POST", "/v1/playback/"+initPlan.SessionID+"/quality", `{"qualityId":"720p","positionMs":20000}`, "combined-hd-device", token, "")
	if w720.Code != 201 {
		t.Fatalf("switch to 720p failed: %d %s", w720.Code, w720.Body)
	}
	var plan720 domain.Plan
	json.Unmarshal(w720.Body.Bytes(), &plan720)

	s.mu.Lock()
	sess720 := s.sessions[plan720.SessionID]
	s.mu.Unlock()
	if sess720 == nil || sess720.source.AudioURL != "" || sess720.source.URL != "https://r1.googlevideo.com/video-720" || sess720.selection.Quality != "720p" {
		t.Fatalf("expected combined 720p session with empty AudioURL: %+v", sess720)
	}

	// 2. Switch combined HD back to Auto when Auto probe FAILS:
	// Server must return 502 and leave existing combined HD session active!
	mediaStub.failAuto = true
	failAutoResp := call(s, "POST", "/v1/playback/"+plan720.SessionID+"/quality", `{"qualityId":"auto","positionMs":25000}`, "combined-hd-device", token, "")
	if failAutoResp.Code != 502 {
		t.Fatalf("expected 502 when Auto probe fails for combined HD, got %d: %s", failAutoResp.Code, failAutoResp.Body)
	}
	// Verify existing session remains intact and playing 720p
	s.mu.Lock()
	sessAfterFail := s.sessions[plan720.SessionID]
	s.mu.Unlock()
	if sessAfterFail == nil || sessAfterFail.ctx.Err() != nil {
		t.Fatal("existing combined HD session was destroyed when switch to Auto failed")
	}
	if sessAfterFail.source.URL != "https://r1.googlevideo.com/video-720" {
		t.Fatalf("expected existing 720p URL preserved, got %s", sessAfterFail.source.URL)
	}

	// 3. Switch combined HD back to Auto when Auto probe SUCCEEDS:
	mediaStub.failAuto = false
	succAutoResp := call(s, "POST", "/v1/playback/"+plan720.SessionID+"/quality", `{"qualityId":"auto","positionMs":30000}`, "combined-hd-device", token, "")
	if succAutoResp.Code != 201 {
		t.Fatalf("expected 201 on successful Auto switch, got %d: %s", succAutoResp.Code, succAutoResp.Body)
	}
	var autoPlan domain.Plan
	json.Unmarshal(succAutoResp.Body.Bytes(), &autoPlan)
	if autoPlan.Mode != "DIRECT_PLAY" {
		t.Fatalf("expected DIRECT_PLAY for Auto progressive stream, got %s", autoPlan.Mode)
	}
	if autoPlan.ResumeMS != 30000 {
		t.Fatalf("expected preserved position 30000ms, got %d", autoPlan.ResumeMS)
	}
	s.mu.Lock()
	autoSess := s.sessions[autoPlan.SessionID]
	s.mu.Unlock()
	if autoSess.source.URL != "https://r1.googlevideo.com/video-360" {
		t.Fatalf("expected replacement session to hold resolved Auto 360p URL, got %s", autoSess.source.URL)
	}
	if autoSess.selection.Quality != "" && autoSess.selection.Quality != "auto" {
		t.Fatalf("expected empty/auto quality on replacement session, got %q", autoSess.selection.Quality)
	}
}

func TestExplicitUnsupportedQualityRejected(t *testing.T) {
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "http://wrapper.local", Token: strings.Repeat("s", 32)}}); err != nil {
		t.Fatal(err)
	}
	mediaStub := &qualityTestMedia{width: 640, height: 360, codec: "h264", profile: "Baseline"}
	s.deps.Media = mediaStub
	s.deps.RemoteMedia = mediaStub

	ytSDSource := domain.Source{
		Item:       domain.Item{ID: "youtube-unsupportedSD", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/unsupportedSD",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"360p"},
		ResolveURL: "http://wrapper.local/resolve/unsupportedSD",
	}
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: ytSDSource}

	token := pair(t, s, "unsupported-q-device")
	setDevicePassingProbes(t, s, "unsupported-q-device", append(standardCapableProbes(), "http-progressive")...)

	s.mu.Lock()
	s.searchResults["unsupported-q-device"] = searchResult{
		query:    "test",
		revision: s.configRevision["youtube"],
		fetched:  time.Now(),
		sources:  []domain.Source{ytSDSource},
	}
	s.mu.Unlock()

	s.deps.Resolver = &mockYouTubeResolver{
		resolveFn: func(ctx context.Context, src domain.Source) (domain.Source, error) {
			res := src
			res.URL = "https://r1.googlevideo.com/video-360"
			res.MIME = "video/mp4"
			return res, nil
		},
	}

	// 1. Explicit 720p requested on SD-only source: must be rejected with 409
	wSD := call(s, "POST", "/v1/playback", `{"itemId":"youtube-unsupportedSD","quality":"720p"}`, "unsupported-q-device", token, "")
	if wSD.Code != 409 {
		t.Fatalf("expected 409 for explicit quality exceeding source inventory, got %d: %s", wSD.Code, wSD.Body)
	}
	s.mu.Lock()
	count := len(s.sessions)
	s.mu.Unlock()
	if count != 0 {
		t.Fatalf("no session should be created on rejected quality, got %d", count)
	}

	// 2. Explicit 1080p requested on device without 1080p probe: must be rejected with 409
	ytHDSource := domain.Source{
		Item:       domain.Item{ID: "youtube-hdVid", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/hdVid",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"1080p", "720p", "360p"},
		ResolveURL: "http://wrapper.local/resolve/hdVid",
	}
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: ytHDSource}
	devNo1080Token := pair(t, s, "dev-no-1080")
	setDevicePassingProbes(t, s, "dev-no-1080", "http-fmp4", "aac", "h264-baseline-360", "h264-720-main", "http-progressive")
	s.mu.Lock()
	s.searchResults["dev-no-1080"] = searchResult{
		query:    "test",
		revision: s.configRevision["youtube"],
		fetched:  time.Now(),
		sources:  []domain.Source{ytHDSource},
	}
	s.mu.Unlock()

	wNo1080 := call(s, "POST", "/v1/playback", `{"itemId":"youtube-hdVid","quality":"1080p"}`, "dev-no-1080", devNo1080Token, "")
	if wNo1080.Code != 409 {
		t.Fatalf("expected 409 for explicit quality exceeding device probe capability, got %d: %s", wNo1080.Code, wNo1080.Body)
	}
	s.mu.Lock()
	countNo1080 := len(s.sessions)
	s.mu.Unlock()
	if countNo1080 != 0 {
		t.Fatalf("no session should be created on rejected quality, got %d", countNo1080)
	}
}

type delayedDynamicRemoteMedia struct {
	*dynamicRemoteMedia
	delay time.Duration
	calls int
}

func (m *delayedDynamicRemoteMedia) ProbeRemote(ctx context.Context, source domain.Source) (domain.Metadata, error) {
	m.calls++
	if m.delay > 0 {
		timer := time.NewTimer(m.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return domain.Metadata{}, ctx.Err()
		case <-timer.C:
		}
	}
	return m.dynamicRemoteMedia.ProbeRemote(ctx, source)
}

func startYouTubeQualityRefreshSession(t *testing.T, deviceID string, media Media, remoteMedia RemoteMedia, resolveFn func(context.Context, domain.Source) (domain.Source, error)) (*Server, string, domain.Plan) {
	t.Helper()
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "http://wrapper.local", Token: strings.Repeat("q", 32)}}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, deviceID)
	setDevicePassingProbes(t, s, deviceID, append(standardCapableProbes(), "http-progressive")...)
	source := domain.Source{
		Item:       domain.Item{ID: "youtube-quality-refresh", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "http://wrapper.local/resolve/quality-refresh",
		MIME:       "application/x-zombie-youtube",
		Variants:   []string{"360p"},
		ResolveURL: "http://wrapper.local/resolve/quality-refresh",
	}
	s.deps.Catalog = &mockYouTubeCatalog{Catalog: s.deps.Catalog, source: source}
	s.deps.Media = media
	s.deps.RemoteMedia = remoteMedia
	s.deps.Resolver = &mockYouTubeResolver{resolveFn: resolveFn}
	s.mu.Lock()
	s.searchResults[deviceID] = searchResult{
		query:    "quality refresh",
		revision: s.configRevision["youtube"],
		fetched:  time.Now(),
		sources:  []domain.Source{source},
	}
	s.mu.Unlock()

	response := call(s, http.MethodPost, "/v1/playback", `{"itemId":"youtube-quality-refresh"}`, deviceID, token, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("initial playback returned %d: %s", response.Code, response.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	return s, token, plan
}

func TestYouTubeQualityMenuRefreshesAutoVariantsAndRequiresExactManualTier(t *testing.T) {
	remoteMedia := &delayedDynamicRemoteMedia{dynamicRemoteMedia: &dynamicRemoteMedia{}}
	var refreshDeadlineRemaining time.Duration
	resolveFn := func(ctx context.Context, source domain.Source) (domain.Source, error) {
		resolved := source
		resolved.MIME = "video/mp4"
		switch source.ResolveQuality {
		case "":
			resolved.URL = "https://r1.googlevideo.com/video-360-current"
			resolved.Variants = []string{"360p"}
		case "auto":
			deadline, ok := ctx.Deadline()
			if !ok {
				return domain.Source{}, errors.New("quality inventory refresh has no deadline")
			}
			refreshDeadlineRemaining = time.Until(deadline)
			resolved.URL = "https://r1.googlevideo.com/video-360-refreshed"
			resolved.Variants = []string{"1080p", "720p", "360p", "9999p"}
		case "720p":
			resolved.URL = "https://r1.googlevideo.com/video-720-selected"
			resolved.AudioURL = "https://r2.googlevideo.com/audio-720-selected"
			resolved.Variants = []string{"1080p", "720p", "360p"}
		case "1080p":
			// Simulate the wrapper advertising an available tier but resolving a
			// lower source after the user explicitly asks for 1080p.
			resolved.URL = "https://r1.googlevideo.com/video-720-fallback"
			resolved.AudioURL = "https://r2.googlevideo.com/audio-720-fallback"
			resolved.Variants = []string{"1080p", "720p", "360p"}
		default:
			return domain.Source{}, errors.New("unexpected quality request")
		}
		return resolved, nil
	}

	s, token, plan := startYouTubeQualityRefreshSession(t, "quality-refresh-device", remoteMedia, remoteMedia, resolveFn)
	s.mu.Lock()
	active := s.sessions[plan.SessionID]
	active.metadata = nil
	activeURL := active.source.URL
	activeQuality := active.selection.Quality
	s.mu.Unlock()
	if err := s.setQualityPreference(t.Context(), "quality-refresh-device", "youtube", "video", "1080p"); err != nil {
		t.Fatal(err)
	}
	remoteMedia.delay = 75 * time.Millisecond

	response := call(s, http.MethodGet, "/v1/playback/"+plan.SessionID+"/qualities", "", "quality-refresh-device", token, "")
	if response.Code != http.StatusOK {
		t.Fatalf("qualities returned %d: %s", response.Code, response.Body)
	}
	var inventory domain.QualityInventory
	if err := json.Unmarshal(response.Body.Bytes(), &inventory); err != nil {
		t.Fatal(err)
	}
	if !playback.HasQuality(inventory, "720p") || !playback.HasQuality(inventory, "1080p") {
		t.Fatalf("fresh Auto variants did not expose validated HD options: %+v", inventory.Options)
	}
	if playback.HasQuality(inventory, "9999p") {
		t.Fatalf("resolver returned an unsupported variant that was advertised: %+v", inventory.Options)
	}
	if remoteMedia.calls < 2 {
		t.Fatalf("expected metadata probing before refresh; ProbeRemote calls=%d", remoteMedia.calls)
	}
	if refreshDeadlineRemaining <= 0 || refreshDeadlineRemaining > youtubeQualityInventoryRefreshTimeout-50*time.Millisecond {
		t.Fatalf("refresh did not inherit the remaining menu request budget after probing: %s", refreshDeadlineRemaining)
	}
	if got := s.getQualityPreference(t.Context(), "quality-refresh-device", "youtube", "video"); got != "1080p" {
		t.Fatalf("inventory refresh changed the stored preference to %q", got)
	}
	s.mu.Lock()
	stillActive := s.sessions[plan.SessionID]
	unchanged := stillActive == active && stillActive.source.URL == activeURL && stillActive.selection.Quality == activeQuality && stillActive.supersededBy == "" && stillActive.ctx.Err() == nil
	s.mu.Unlock()
	if !unchanged {
		t.Fatal("inventory refresh changed or replaced the active playback URL/session/quality")
	}

	selected := call(s, http.MethodPost, "/v1/playback/"+plan.SessionID+"/quality", `{"qualityId":"720p","positionMs":12000}`, "quality-refresh-device", token, "")
	if selected.Code != http.StatusCreated {
		t.Fatalf("manual 720p switch returned %d: %s", selected.Code, selected.Body)
	}
	var selectedPlan domain.Plan
	if err := json.Unmarshal(selected.Body.Bytes(), &selectedPlan); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	selectedSession := s.sessions[selectedPlan.SessionID]
	s.mu.Unlock()
	if selectedSession == nil || selectedSession.selection.Quality != "720p" || !hasExactYouTubeQuality(*selectedSession.metadata, "720p") {
		t.Fatalf("manual 720p did not retain its exact resolved tier: session=%+v", selectedSession)
	}
	if got := s.getQualityPreference(t.Context(), "quality-refresh-device", "youtube", "video"); got != "720p" {
		t.Fatalf("successful manual tier did not update preference: %q", got)
	}

	assertQualitySelectionFailurePreservesSession(t, s, "quality-refresh-device", "youtube", token, selectedPlan.SessionID, "1080p", 15000)
}

func TestYouTubeQualityInventoryRefreshIsCancellationAwareAndBackedOff(t *testing.T) {
	remoteMedia := &dynamicRemoteMedia{}
	refreshEntered := make(chan struct{})
	var refreshCalls int
	resolveFn := func(ctx context.Context, source domain.Source) (domain.Source, error) {
		resolved := source
		resolved.MIME = "video/mp4"
		if source.ResolveQuality == "" {
			resolved.URL = "https://r1.googlevideo.com/video-360-current"
			resolved.Variants = []string{"360p"}
			return resolved, nil
		}
		if source.ResolveQuality == "auto" {
			refreshCalls++
			close(refreshEntered)
			<-ctx.Done()
			return domain.Source{}, ctx.Err()
		}
		return domain.Source{}, errors.New("unexpected quality request")
	}
	s, token, plan := startYouTubeQualityRefreshSession(t, "quality-refresh-cancel-device", remoteMedia, remoteMedia, resolveFn)
	s.mu.Lock()
	active := s.sessions[plan.SessionID]
	activeURL := active.source.URL
	activeQuality := active.selection.Quality
	s.mu.Unlock()

	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/playback/"+plan.SessionID+"/qualities", nil).WithContext(requestCtx)
	request.Header.Set("X-Zombie-Device", "quality-refresh-cancel-device")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	completed := make(chan struct{})
	go func() {
		s.ServeHTTP(response, request)
		close(completed)
	}()
	select {
	case <-refreshEntered:
	case <-time.After(time.Second):
		t.Fatal("quality inventory refresh did not start")
	}
	cancelAt := time.Now()
	cancel()
	select {
	case <-completed:
		if elapsed := time.Since(cancelAt); elapsed > 250*time.Millisecond {
			t.Fatalf("cancelled resolver took %s to return", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("quality inventory refresh did not stop after request cancellation")
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh resolver calls = %d, want one cancelled attempt", refreshCalls)
	}

	second := call(s, http.MethodGet, "/v1/playback/"+plan.SessionID+"/qualities", "", "quality-refresh-cancel-device", token, "")
	if second.Code != http.StatusOK {
		t.Fatalf("cached qualities after cancellation returned %d: %s", second.Code, second.Body)
	}
	if refreshCalls != 1 {
		t.Fatalf("cancelled refresh was retried inside its backoff window: %d calls", refreshCalls)
	}
	s.mu.Lock()
	unchanged := s.sessions[plan.SessionID] == active && active.source.URL == activeURL && active.selection.Quality == activeQuality && active.supersededBy == "" && active.ctx.Err() == nil
	s.mu.Unlock()
	if !unchanged {
		t.Fatal("cancelled inventory refresh changed or replaced active playback")
	}
}
