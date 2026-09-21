package server

import (
	"context"
	"encoding/json"
	"io"
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
	s.sessions["local"] = &session{device: "track-owner", source: domain.Source{Path: "/private/media.mkv"}, ctx: ctx, cancel: cancel, expires: time.Now().Add(time.Hour)}
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
