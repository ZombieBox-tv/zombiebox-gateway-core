package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type trackMedia struct{ calls int }

func (m *trackMedia) Probe(context.Context, string) (domain.Metadata, error) {
	m.calls++
	return domain.Metadata{Streams: []domain.Stream{{Index: 1, Type: "audio", Codec: "aac"}, {Index: 2, Type: "subtitle", Codec: "subrip"}, {Index: 3, Type: "subtitle", Codec: "hdmv_pgs_subtitle"}}}, nil
}

func TestProviderAttachmentsExposeOnlyOwnedSemanticTracks(t *testing.T) {
	s := testServer(t, nil, "")
	owner := pair(t, s, "attachment-owner")
	other := pair(t, s, "attachment-other")
	s.deps.RemoteMedia = &remoteTrackMedia{}
	s.deps.RemoteSubtitles = &remoteTrackMedia{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.sessions["attachment"] = &session{device: "attachment-owner", ctx: ctx, cancel: cancel,
		expires: time.Now().Add(time.Hour), metadata: &domain.Metadata{},
		source: domain.Source{URL: "https://provider.test/video.mp4", MIME: "video/mp4", Subtitles: []domain.SubtitleSource{{URL: "https://provider.test/sub.srt", Headers: http.Header{"X-Plex-Token": {"private-secret"}}, Codec: "srt", Language: "es", Title: "Spanish"}}}}
	for _, path := range []string{"tracks", "subtitles/1600000000"} {
		response := call(s, "GET", "/v1/playback/attachment/"+path, "", "attachment-owner", owner, "")
		if response.Code != 200 || strings.Contains(response.Body.String(), "provider.test") || strings.Contains(response.Body.String(), "private-secret") {
			t.Fatalf("unexpected attachment response: %d %s", response.Code, response.Body)
		}
		response = call(s, "GET", "/v1/playback/attachment/"+path, "", "attachment-other", other, "")
		if response.Code != 404 {
			t.Fatal("attachment exposed across devices")
		}
	}
	response := call(s, "GET", "/v1/playback/attachment/subtitles/1600000001", "", "attachment-owner", owner, "")
	if response.Code != 409 {
		t.Fatal("unowned attachment index accepted")
	}
}
func (*trackMedia) Convert(context.Context, string, string, io.Writer) error { return nil }
func (*trackMedia) ConvertSelected(context.Context, string, string, domain.MediaSelection, io.Writer) error {
	return nil
}
func (*trackMedia) Subtitles(context.Context, string, int) ([]domain.SubtitleCue, error) {
	return []domain.SubtitleCue{{StartMS: 1000, EndMS: 2000, Text: "Hello"}}, nil
}

func TestTrackOwnershipSelectionAndUnavailableSources(t *testing.T) {
	s := testServer(t, nil, "")
	media := &trackMedia{}
	s.deps.Media = media
	owner := pair(t, s, "track-owner")
	other := pair(t, s, "track-other")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.sessions["local"] = &session{selection: domain.MediaSelection{Quality: "LOW"}, device: "track-owner", source: domain.Source{Path: "/private/media.mkv"}, ctx: ctx, cancel: cancel, expires: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/tracks", "", 200},
		{"GET", "/subtitles/2", "", 200},
		{"GET", "/subtitles/3", "", 409},
		{"GET", "/subtitles/-1", "", 400},
		{"POST", "/audio", `{"audioId":2,"positionMs":500}`, 409},
		{"POST", "/audio", `{"positionMs":500}`, 400},
		{"POST", "/audio", `{"audioId":1,"positionMs":-1}`, 400},
	} {
		w := call(s, tc.method, "/v1/playback/local"+tc.path, tc.body, "track-owner", owner, "")
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "/private") {
			t.Fatal("local path leaked")
		}
	}
	before := media.calls
	for _, path := range []string{"tracks", "subtitles/2"} {
		w := call(s, "GET", "/v1/playback/local/"+path, "", "track-other", other, "")
		if w.Code != 404 {
			t.Fatal("cross-device access")
		}
	}
	if media.calls != before {
		t.Fatal("unauthorized request started probe")
	}
	w := call(s, "POST", "/v1/playback/local/audio", `{"audioId":1,"positionMs":1250}`, "track-owner", owner, "")
	if w.Code != 201 {
		t.Fatalf("select: %d %s", w.Code, w.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.TimelineOffsetMS != 1250 || plan.ResumeMS != 0 || plan.Seekable || plan.Mode != "TRANSCODE" {
		t.Fatalf("timeline: %+v", plan)
	}
	if s.sessions[plan.SessionID].selection.Quality != "LOW" {
		t.Fatal("audio selection lost bandwidth profile")
	}
	if s.sessions["local"].ctx.Err() != nil {
		t.Fatal("old session cancelled before client adoption")
	}
	if *s.sessions[plan.SessionID].selection.AudioID != 1 {
		t.Fatal("selection not stored")
	}
	call(s, "DELETE", "/v1/playback/local", "", "track-owner", owner, "")
	if s.sessions[plan.SessionID].ctx.Err() != nil {
		t.Fatal("replacement cancelled with old session")
	}
	s.sessions["remote"] = &session{device: "track-owner", source: domain.Source{URL: "https://example.test/media"}, ctx: context.Background(), cancel: func() {}, expires: time.Now().Add(time.Hour)}
	w = call(s, "GET", "/v1/playback/remote/tracks", "", "track-owner", owner, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("remote: %s", w.Body)
	}
}

func TestSubtitleExtractionCancelledWithSession(t *testing.T) {
	// A request that reaches the adapter after stop must inherit cancellation.
	s := testServer(t, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.deps.Media = &cancelledTrackMedia{t: t}
	sess := &session{source: domain.Source{Path: "fixture"}, ctx: ctx}
	_, err := s.sessionTracks(context.Background(), sess)
	if err == nil {
		t.Fatal("cancelled session accepted")
	}
}

type cancelledTrackMedia struct {
	trackMedia
	t *testing.T
}

func (m *cancelledTrackMedia) Probe(ctx context.Context, _ string) (domain.Metadata, error) {
	select {
	case <-ctx.Done():
		return domain.Metadata{}, ctx.Err()
	case <-time.After(time.Second):
		m.t.Fatal("missing cancellation")
		return domain.Metadata{}, nil
	}
}

// Keep the handler signature honest for malformed bodies as well as ownership.
func TestAudioSelectionRejectsMalformedJSON(t *testing.T) {
	s := testServer(t, nil, "")
	w := httptest.NewRecorder()
	s.selectAudio(w, httptest.NewRequest("POST", "/", strings.NewReader("{")), domain.Device{})
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}

type remoteTrackMedia struct{ trackMedia }

func (m *remoteTrackMedia) ProbeRemote(ctx context.Context, source domain.Source) (domain.Metadata, error) {
	return m.Probe(ctx, source.URL)
}
func (m *remoteTrackMedia) SubtitlesRemote(ctx context.Context, source domain.Source, index int) ([]domain.SubtitleCue, error) {
	return m.Subtitles(ctx, source.URL, index)
}
func (*remoteTrackMedia) ConvertRemote(context.Context, domain.Source, string, domain.MediaSelection, io.Writer) error {
	return nil
}

func TestRemoteTrackSelectionUsesOwnedSessionAndPreservesPosition(t *testing.T) {
	s := testServer(t, nil, "")
	adapter := &remoteTrackMedia{}
	s.deps.RemoteMedia, s.deps.RemoteSubtitles = adapter, adapter
	token := pair(t, s, "remote-owner")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.sessions["remote"] = &session{device: "remote-owner", source: domain.Source{URL: "https://private.test/movie.mkv?secret=hidden", MIME: "video/x-matroska"}, ctx: ctx, cancel: cancel, expires: time.Now().Add(time.Hour)}
	for _, suffix := range []string{"tracks", "subtitles/2"} {
		w := call(s, "GET", "/v1/playback/remote/"+suffix, "", "remote-owner", token, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "hidden") {
			t.Fatal(w.Code, w.Body)
		}
	}
	w := call(s, "POST", "/v1/playback/remote/audio", `{"audioId":1,"positionMs":4500}`, "remote-owner", token, "")
	var plan domain.Plan
	_ = json.Unmarshal(w.Body.Bytes(), &plan)
	if w.Code != 201 || plan.TimelineOffsetMS != 4500 || plan.Mode != "TRANSCODE" {
		t.Fatal(w.Code, w.Body)
	}
	if ctx.Err() != nil {
		t.Fatal("old session cancelled before replacement adoption")
	}
}

func TestLanguageDecisionSelectsRequestedAudioAndHonorsFragmentFailure(t *testing.T) {
	first := domain.Stream{Index: 1, Type: "audio", Codec: "aac"}
	first.Tags.Language = "eng"
	second := first
	second.Index, second.Tags.Language = 2, "spa"
	metadata := domain.Metadata{Streams: []domain.Stream{first, second}}
	device := domain.Device{Preferences: domain.Preferences{AudioLanguages: []string{"es-MX"}, SubtitleMode: "off"}}
	decision := trackDecision(metadata, "AUTO", domain.Source{}, device)
	if decision.mode != "REMUX" || decision.audioID == nil || *decision.audioID != 2 {
		t.Fatal(decision)
	}
	device.Capabilities.Probes = []domain.Probe{{ID: "http-fmp4", Status: "FAIL", TestedAt: time.Now().Unix()}}
	if decision := trackDecision(metadata, "AUTO", domain.Source{}, device); decision.mode != "EXTERNAL_PLAYER" || decision.audioID != nil {
		t.Fatal(decision)
	}
}

func TestAudioSelectionRejectsFailedOutputWithoutReplacingSession(t *testing.T) {
	s := testServer(t, nil, "")
	adapter := &trackMedia{}
	s.deps.Media = adapter
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.sessions["original"] = &session{device: "owner", source: domain.Source{Path: "/private/movie.mkv"}, ctx: ctx, cancel: cancel, expires: time.Now().Add(time.Hour)}
	request := httptest.NewRequest("POST", "/", strings.NewReader(`{"audioId":1,"positionMs":1000}`))
	request.SetPathValue("session", "original")
	response := httptest.NewRecorder()
	s.selectAudio(response, request, domain.Device{ID: "owner", Capabilities: domain.Capabilities{Probes: []domain.Probe{{ID: "http-fmp4", Status: "FAIL", TestedAt: time.Now().Unix()}}}})
	if response.Code != 409 || len(s.sessions) != 1 || ctx.Err() != nil || adapter.calls != 1 {
		t.Fatalf("failed output changed playback: %d sessions=%d probes=%d", response.Code, len(s.sessions), adapter.calls)
	}
	request = httptest.NewRequest("POST", "/", strings.NewReader(`{"audioId":1,"positionMs":0}`))
	request.SetPathValue("session", "original")
	response = httptest.NewRecorder()
	s.selectAudio(response, request, domain.Device{ID: "owner"})
	var plan domain.Plan
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil || response.Code != 201 || plan.Mode != "REMUX" || adapter.calls != 2 {
		t.Fatalf("compatible selection: %d %s", response.Code, response.Body)
	}
	s.sessions[plan.SessionID].cancel()
	delete(s.sessions, plan.SessionID)
	// Cached metadata does not manufacture a conversion adapter.
	s.sessions["original"].metadata = &domain.Metadata{Streams: []domain.Stream{{Index: 1, Type: "audio", Codec: "aac"}}}
	s.deps.Media = nil
	request = httptest.NewRequest("POST", "/", strings.NewReader(`{"audioId":1,"positionMs":0}`))
	request.SetPathValue("session", "original")
	response = httptest.NewRecorder()
	s.selectAudio(response, request, domain.Device{ID: "owner"})
	if response.Code != 409 || len(s.sessions) != 1 {
		t.Fatal("missing adapter accepted")
	}
}
