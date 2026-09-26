package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/receivers/inbox"
)

func TestMediaReceiverRoutesOwnedSourceAndAuthorizesSpotifyControls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) {
			t.Error("worker token absent")
		}
		switch r.URL.Path {
		case "/status":
			fmt.Fprint(w, `{"stopped":false,"track":{"name":"Song","artist_names":["Artist"]}}`)
		case "/audio":
			fmt.Fprint(w, "audio-fixture")
		case "/player/pause":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	s := testServer(t, nil, t.TempDir())
	s.SeedProviders(context.Background(), map[string]providers.Config{"spotify": {Enabled: true, URL: upstream.URL, Token: strings.Repeat("s", 32)}})
	token := pair(t, s, "media-owner")
	addLiveMP3DirectEvidence(t, s, "media-owner")
	other := pair(t, s, "media-other")
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"spotify"}`, "media-owner", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "media-other", other, ""); w.Code != 409 {
		t.Fatal("foreign claim", w.Body)
	}
	active := call(s, "GET", "/v1/media-receiver", "", "media-owner", token, "")
	var snapshot inbox.Snapshot
	json.Unmarshal(active.Body.Bytes(), &snapshot)
	if active.Code != 200 || snapshot.Plan == nil || snapshot.Plan.Item.Title != "Song" || strings.Contains(active.Body.String(), upstream.URL) {
		t.Fatal(active.Body)
	}
	stream := call(s, "GET", snapshot.Plan.URL, "", "", "", "")
	if stream.Body.String() != "audio-fixture" {
		t.Fatal(stream.Body)
	}
	if w := call(s, "POST", "/v1/player/spotify", `{"action":"pause"}`, "media-owner", token, ""); w.Code != 200 {
		t.Fatal("owner control denied", w.Body)
	}
	if w := call(s, "POST", "/v1/player/spotify", `{"action":"pause"}`, "media-other", other, ""); w.Code != 403 {
		t.Fatal("foreign control accepted", w.Code)
	}
	if w := call(s, "DELETE", "/v1/playback/"+snapshot.Plan.SessionID, "", "media-owner", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := call(s, "GET", snapshot.Plan.URL, "", "", "", ""); w.Code != 401 {
		t.Fatal("released stream survived", w.Code)
	}
	blocked := call(s, "GET", "/v1/media-receiver", "", "media-owner", token, "")
	var result struct{ Plan *domain.Plan }
	json.Unmarshal(blocked.Body.Bytes(), &result)
	if result.Plan != nil {
		t.Fatal("dismissed source reopened")
	}
}

func addLiveMP3DirectEvidence(t *testing.T, s *Server, deviceID string) {
	t.Helper()
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", deviceID, &device); err != nil {
		t.Fatal(err)
	}
	device.Capabilities.Probes = append(device.Capabilities.Probes, domain.Probe{
		ID: "mp3-chunked", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix(),
	})
	if err := s.db.Put(t.Context(), "devices", deviceID, device); err != nil {
		t.Fatal(err)
	}
}

func TestSpotifyReceiverWithoutMP3PlaybackEvidenceIsUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"stopped":false,"track":{"name":"Song","artist_names":["Artist"]}}`)
	}))
	defer upstream.Close()
	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{
		"spotify": {Enabled: true, URL: upstream.URL, Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "media-no-mp3-evidence")
	if response := call(s, "PUT", "/v1/media-receiver", `{"provider":"spotify"}`, "media-no-mp3-evidence", token, ""); response.Code != http.StatusOK {
		t.Fatalf("Spotify claim: %d %s", response.Code, response.Body)
	}
	response := call(s, "GET", "/v1/media-receiver", "", "media-no-mp3-evidence", token, "")
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"code":"receiver_unavailable"`) {
		t.Fatalf("unprobed Spotify source should remain unavailable: %d %s", response.Code, response.Body)
	}
}

func TestAirPlayPlayerCommandRequiresOwnerOrAdminAndMapsAllowedActions(t *testing.T) {
	privateToken := strings.Repeat("a", 32)
	var requests atomic.Int32
	commands := make(chan string, 4)
	var rejected atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/control" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+privateToken {
			t.Errorf("unexpected worker request: method=%s path=%s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Command string `json:"command"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests.Add(1)
		commands <- body.Command
		if rejected.Load() {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	s := testServer(t, nil, t.TempDir())
	s.opt.PairingCode = "123456"
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"airplay": {Enabled: true, URL: upstream.URL, Token: privateToken},
	}); err != nil {
		t.Fatal(err)
	}
	ownerToken := pair(t, s, "airplay-owner")
	otherToken := pair(t, s, "airplay-other")
	if response := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "airplay-owner", ownerToken, ""); response.Code != http.StatusOK {
		t.Fatalf("AirPlay claim: %d %s", response.Code, response.Body)
	}
	if response := call(s, "POST", "/v1/player/airplay", `{"action":"playpause"}`, "airplay-owner", ownerToken, ""); response.Code != http.StatusOK {
		t.Fatalf("owner command: %d %s", response.Code, response.Body)
	}
	if got := <-commands; got != "playpause" {
		t.Fatalf("private command = %q, want playpause", got)
	}
	if response := call(s, "POST", "/v1/player/airplay", `{"action":"next"}`, "airplay-other", otherToken, ""); response.Code != http.StatusForbidden {
		t.Fatalf("foreign command was accepted: %d %s", response.Code, response.Body)
	}
	if requests.Load() != 1 {
		t.Fatalf("foreign request reached worker: %d", requests.Load())
	}
	if response := call(s, "POST", "/v1/player/airplay", `{"action":"../control"}`, "airplay-owner", ownerToken, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("arbitrary action was accepted: %d %s", response.Code, response.Body)
	}
	if requests.Load() != 1 {
		t.Fatalf("invalid request reached worker: %d", requests.Load())
	}
	if response := call(s, "POST", "/v1/player/airplay", `{"action":"previous"}`, "airplay-other", otherToken, "123456"); response.Code != http.StatusOK {
		t.Fatalf("admin command: %d %s", response.Code, response.Body)
	}
	if got := <-commands; got != "previtem" {
		t.Fatalf("admin private command = %q, want previtem", got)
	}
	if response := call(s, "POST", "/v1/player/airplay", `{"action":"next"}`, "airplay-owner", ownerToken, ""); response.Code != http.StatusOK {
		t.Fatalf("owner next command: %d %s", response.Code, response.Body)
	}
	if got := <-commands; got != "nextitem" {
		t.Fatalf("private next command = %q, want nextitem", got)
	}
	rejected.Store(true)
	failed := call(s, "POST", "/v1/player/airplay", `{"action":"next"}`, "airplay-owner", ownerToken, "")
	if failed.Code != http.StatusBadGateway {
		t.Fatalf("worker failure was not reported: %d %s", failed.Code, failed.Body)
	}
	if strings.Contains(failed.Body.String(), privateToken) {
		t.Fatal("private worker token leaked in public response")
	}
}

func TestAirPlayVideoToAudioReplacesOwnedStreamAndRetiresOldTicket(t *testing.T) {
	var mode atomic.Int32
	mode.Store(1)
	privateToken := strings.Repeat("a", 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+privateToken {
			t.Error("AirPlay worker request lost its private token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/status":
			fmt.Fprintf(w, `{"active":%t,"audioActive":%t,"metadata":{"title":"Track","artist":"Artist","album":"Album"}}`, mode.Load() == 1, mode.Load() == 2)
		case "/stream/index.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nvideo.ts\n")
		case "/stream/audio.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\naudio.ts\n")
		case "/stream/video.ts":
			fmt.Fprint(w, "video-fixture")
		case "/stream/audio.ts":
			fmt.Fprint(w, "audio-fixture")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"airplay": {Enabled: true, URL: upstream.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "airplay-switch")
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "airplay-switch", &dev); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 800, TestedAt: now},
		},
	}
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "airplay-switch", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	read := func() inbox.Snapshot {
		w := call(s, "GET", "/v1/media-receiver", "", "airplay-switch", token, "")
		var snapshot inbox.Snapshot
		if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil || w.Code != 200 {
			t.Fatal(w.Code, w.Body, err)
		}
		return snapshot
	}
	video := read()
	if video.Plan == nil || video.Plan.Item.Kind != "video" || video.Plan.Item.Provider != "airplay" || strings.Contains(video.Plan.URL, privateToken) {
		t.Fatal("video session or semantic boundary missing", video)
	}
	assertStream := func(plan *domain.Plan, expected string) {
		w := call(s, "GET", plan.URL, "", "", "", "")
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "#EXTM3U") || strings.Contains(w.Body.String(), upstream.URL) {
			t.Fatal("manifest unavailable or upstream leaked", w.Code, w.Body)
		}
		lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
		segment := lines[len(lines)-1]
		if !strings.HasPrefix(segment, "/v1/streams/") {
			t.Fatal("segment was not rewritten to the local gateway", segment)
		}
		w = call(s, "GET", segment, "", "", "", "")
		if w.Code != 200 || w.Body.String() != expected {
			t.Fatal("receiver segment unavailable", w.Code, w.Body)
		}
	}
	assertStream(video.Plan, "video-fixture")
	mode.Store(2)
	audio := read()
	if audio.Plan == nil || audio.Plan.Item.Kind != "audio" || audio.Plan.Item.Title != "Track" || audio.Plan.Item.Subtitle != "Artist" || audio.Plan.SessionID == video.Plan.SessionID {
		t.Fatal("audio did not replace video", audio)
	}
	if w := call(s, "GET", video.Plan.URL, "", "", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("old video ticket survived", w.Code)
	}
	assertStream(audio.Plan, "audio-fixture")
	mode.Store(0)
	ended := read()
	if ended.Plan != nil || ended.NowPlaying == nil || ended.NowPlaying.State != "STOPPED" {
		t.Fatal("ended AirPlay stream stayed active", ended)
	}
	if w := call(s, "GET", audio.Plan.URL, "", "", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("ended audio ticket survived", w.Code)
	}
}

func TestSpotifyMetadataPauseAndNaturalEndKeepThenRevokeStream(t *testing.T) {
	var phase atomic.Int32
	phase.Store(1)
	privateToken := strings.Repeat("s", 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+privateToken {
			t.Error("Spotify worker token missing")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/status":
			switch phase.Load() {
			case 1:
				fmt.Fprint(w, `{"stopped":false,"track":{"name":"First","artist_names":["Artist"]}}`)
			case 2:
				fmt.Fprint(w, `{"stopped":false,"paused":true,"track":{"name":"Second","artist_names":["Artist"]}}`)
			default:
				fmt.Fprint(w, `{"stopped":true}`)
			}
		case "/audio":
			fmt.Fprint(w, "audio-fixture")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"spotify": {Enabled: true, URL: upstream.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "spotify-lifecycle")
	addLiveMP3DirectEvidence(t, s, "spotify-lifecycle")
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"spotify"}`, "spotify-lifecycle", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	read := func() inbox.Snapshot {
		w := call(s, "GET", "/v1/media-receiver", "", "spotify-lifecycle", token, "")
		var snapshot inbox.Snapshot
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &snapshot) != nil {
			t.Fatal(w.Code, w.Body)
		}
		return snapshot
	}
	first := read()
	if first.Plan == nil || first.Plan.Item.Title != "First" {
		t.Fatal("missing first track", first)
	}
	phase.Store(2)
	paused := read()
	if paused.Plan == nil || paused.Plan.SessionID != first.Plan.SessionID || paused.Plan.Item.Title != "Second" || paused.NowPlaying == nil || paused.NowPlaying.State != "PAUSED" {
		t.Fatal("metadata/pause restarted or lost stream", paused)
	}
	if w := call(s, "GET", first.Plan.URL, "", "", "", ""); w.Code != 200 || w.Body.String() != "audio-fixture" {
		t.Fatal("paused ticket broke", w.Code, w.Body)
	}
	phase.Store(3)
	ended := read()
	if ended.Plan != nil || ended.NowPlaying == nil || ended.NowPlaying.State != "STOPPED" {
		t.Fatal("natural end remained live", ended)
	}
	if w := call(s, "GET", first.Plan.URL, "", "", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("ended Spotify ticket survived", w.Code)
	}
}

type mockReceiverRemoteMedia struct {
	probeFunc   func(context.Context, domain.Source) (domain.Metadata, error)
	convertFunc func(context.Context, domain.Source, string, domain.MediaSelection, io.Writer) error
}

func (m *mockReceiverRemoteMedia) ProbeRemote(ctx context.Context, s domain.Source) (domain.Metadata, error) {
	if m.probeFunc != nil {
		return m.probeFunc(ctx, s)
	}
	return domain.Metadata{}, errors.New("probe not implemented")
}

func (m *mockReceiverRemoteMedia) ConvertRemote(ctx context.Context, s domain.Source, mode string, sel domain.MediaSelection, out io.Writer) error {
	if m.convertFunc != nil {
		return m.convertFunc(ctx, s, mode, sel, out)
	}
	return errors.New("convert not implemented")
}

func TestAirPlayReceiverVizioFallbackToGatewayRemux(t *testing.T) {
	privateToken := strings.Repeat("k", 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+privateToken {
			t.Error("AirPlay worker request lost its private token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/status":
			fmt.Fprint(w, `{"active":false,"audioActive":true,"metadata":{"title":"Vizio AirPlay Track","artist":"Apple Music","album":"Test Album"}}`)
		case "/stream/audio.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\naudio.ts\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"airplay": {Enabled: true, URL: upstream.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}

	mockRemote := &mockReceiverRemoteMedia{
		probeFunc: func(_ context.Context, src domain.Source) (domain.Metadata, error) {
			meta := domain.Metadata{
				Streams: []domain.Stream{
					{Type: "audio", Codec: "aac", Index: 0},
				},
			}
			meta.Format.Name = "hls,applehttp"
			return meta, nil
		},
		convertFunc: func(_ context.Context, src domain.Source, mode string, _ domain.MediaSelection, out io.Writer) error {
			if mode != "REMUX" {
				return fmt.Errorf("unexpected mode: %s", mode)
			}
			_, err := io.WriteString(out, "adts-audio-fixture-bytes")
			return err
		},
	}
	s.deps.RemoteMedia = mockRemote

	token := pair(t, s, "vizio-tv")
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "vizio-tv", &dev); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	// Stored Vizio probes: AAC PASS, mpegts-h264-aac PASS, http-fmp4 PASS, hls-h264-aac UNKNOWN (or absent)
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "aac-adts", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "mpegts-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "hls-h264-aac", Status: "UNKNOWN", TestedAt: now},
		},
	}
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}

	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "vizio-tv", token, ""); w.Code != 200 {
		t.Fatalf("PUT failed: %d %s", w.Code, w.Body)
	}

	resp := call(s, "GET", "/v1/media-receiver", "", "vizio-tv", token, "")
	if resp.Code != 200 {
		t.Fatalf("GET /v1/media-receiver failed: %d %s", resp.Code, resp.Body)
	}
	if strings.Contains(resp.Body.String(), privateToken) || strings.Contains(resp.Body.String(), upstream.URL) {
		t.Fatal("secret token or upstream URL leaked in response", resp.Body)
	}

	var snapshot inbox.Snapshot
	if err := json.Unmarshal(resp.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Plan == nil {
		t.Fatal("expected non-nil plan in snapshot")
	}
	plan := snapshot.Plan
	if plan.Mode != "REMUX" {
		t.Fatalf("expected REMUX mode for Vizio fallback, got %s", plan.Mode)
	}
	if plan.MIME != "audio/aac" {
		t.Fatalf("expected audio/aac MIME for audio-only plan, got %s", plan.MIME)
	}
	if plan.Item.Kind != "audio" {
		t.Fatalf("expected audio kind, got %s", plan.Item.Kind)
	}
	if plan.Item.Title != "Vizio AirPlay Track" {
		t.Fatalf("expected title 'Vizio AirPlay Track', got %s", plan.Item.Title)
	}
	if plan.Live != true || plan.Seekable != false {
		t.Fatalf("expected live and non-seekable plan, got live=%v seekable=%v", plan.Live, plan.Seekable)
	}
	if !strings.HasPrefix(plan.URL, "/v1/streams/") {
		t.Fatalf("expected stream URL starting with /v1/streams/, got %s", plan.URL)
	}

	// Stream serving test
	streamResp := call(s, "GET", plan.URL, "", "", "", "")
	if streamResp.Code != 200 {
		t.Fatalf("stream serving failed: %d %s", streamResp.Code, streamResp.Body)
	}
	if streamResp.Header().Get("Content-Type") != "audio/aac" {
		t.Fatalf("expected Content-Type audio/aac, got %s", streamResp.Header().Get("Content-Type"))
	}
	if streamResp.Body.String() != "adts-audio-fixture-bytes" {
		t.Fatalf("expected fixture bytes, got %s", streamResp.Body.String())
	}
	if strings.Contains(streamResp.Body.String(), privateToken) || strings.Contains(streamResp.Body.String(), upstream.URL) {
		t.Fatal("secret leaked in stream response")
	}
}

func TestAirPlayReceiverUsesProbedPCMWhenCompressedLiveRoutesAreUnknown(t *testing.T) {
	privateToken := strings.Repeat("p", 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+privateToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/status":
			fmt.Fprint(w, `{"active":false,"audioActive":true,"metadata":{"title":"PCM Test","artist":"Sender"}}`)
		case "/stream/audio.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\naudio.ts\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"airplay": {Enabled: true, URL: upstream.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}
	s.deps.RemoteMedia = &mockReceiverRemoteMedia{
		probeFunc: func(_ context.Context, _ domain.Source) (domain.Metadata, error) {
			meta := domain.Metadata{Streams: []domain.Stream{{Type: "audio", Codec: "aac", Index: 0}}}
			meta.Format.Name = "hls,applehttp"
			return meta, nil
		},
		convertFunc: func(_ context.Context, _ domain.Source, mode string, _ domain.MediaSelection, out io.Writer) error {
			if mode != "PCM_STREAM" {
				t.Errorf("conversion mode = %s, want PCM_STREAM", mode)
			}
			_, err := out.Write([]byte{0, 0, 1, 0})
			return err
		},
	}
	token := pair(t, s, "pcm-audio-tv")
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "pcm-audio-tv", &dev); err != nil {
		t.Fatal(err)
	}
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "FAIL", TestedAt: time.Now().Unix()},
			{ID: "aac-adts", Status: "UNKNOWN", TestedAt: time.Now().Unix()},
			{ID: "audio-track-pcm-stream", Status: "PASS", PositionMS: 700, TestedAt: time.Now().Unix()},
		},
	}
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, dev.ID, token, ""); w.Code != 200 {
		t.Fatalf("PUT failed: %d %s", w.Code, w.Body)
	}
	resp := call(s, "GET", "/v1/media-receiver", "", dev.ID, token, "")
	if resp.Code != 200 {
		t.Fatalf("GET failed: %d %s", resp.Code, resp.Body)
	}
	var snapshot inbox.Snapshot
	if err := json.Unmarshal(resp.Body.Bytes(), &snapshot); err != nil || snapshot.Plan == nil {
		t.Fatalf("missing PCM plan: %s: %v", resp.Body, err)
	}
	if snapshot.Plan.Mode != "TRANSCODE" || snapshot.Plan.MIME != media.PCMStreamMIME || !snapshot.Plan.Live || snapshot.Plan.Seekable {
		t.Fatalf("wrong PCM plan: %+v", snapshot.Plan)
	}
	stream := call(s, "GET", snapshot.Plan.URL, "", "", "", "")
	if stream.Code != 200 || stream.Header().Get("Content-Type") != media.PCMStreamMIME || stream.Body.Len() != 4 {
		t.Fatalf("wrong PCM stream: %d, %q, %d", stream.Code, stream.Header().Get("Content-Type"), stream.Body.Len())
	}
}

func TestAirPlayReceiverFreshHLSPASSDirect(t *testing.T) {
	privateToken := strings.Repeat("h", 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+privateToken {
			t.Error("AirPlay worker request lost its private token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/status":
			fmt.Fprint(w, `{"active":false,"audioActive":true,"metadata":{"title":"Direct HLS Track","artist":"Direct Artist"}}`)
		case "/stream/audio.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\naudio.ts\n")
		case "/stream/audio.ts":
			fmt.Fprint(w, "direct-hls-ts-data")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"airplay": {Enabled: true, URL: upstream.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}

	s.deps.RemoteMedia = &mockReceiverRemoteMedia{
		probeFunc: func(_ context.Context, src domain.Source) (domain.Metadata, error) {
			meta := domain.Metadata{
				Streams: []domain.Stream{
					{Type: "audio", Codec: "aac", Index: 0},
				},
			}
			meta.Format.Name = "hls,applehttp"
			return meta, nil
		},
	}

	token := pair(t, s, "direct-hls-tv")
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "direct-hls-tv", &dev); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 800, TestedAt: now},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 800, TestedAt: now},
		},
	}
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}

	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "direct-hls-tv", token, ""); w.Code != 200 {
		t.Fatalf("PUT failed: %d %s", w.Code, w.Body)
	}

	resp := call(s, "GET", "/v1/media-receiver", "", "direct-hls-tv", token, "")
	if resp.Code != 200 {
		t.Fatalf("GET failed: %d %s", resp.Code, resp.Body)
	}
	if strings.Contains(resp.Body.String(), privateToken) || strings.Contains(resp.Body.String(), upstream.URL) {
		t.Fatal("secret leaked in response", resp.Body)
	}

	var snapshot inbox.Snapshot
	if err := json.Unmarshal(resp.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Plan == nil {
		t.Fatal("expected non-nil plan")
	}
	plan := snapshot.Plan
	if plan.Mode != "DIRECT_PLAY" {
		t.Fatalf("expected DIRECT_PLAY mode for fresh HLS PASS, got %s", plan.Mode)
	}
	if plan.MIME != "application/vnd.apple.mpegurl" {
		t.Fatalf("expected application/vnd.apple.mpegurl MIME, got %s", plan.MIME)
	}

	// Stream serving should serve rewritten HLS playlist
	streamResp := call(s, "GET", plan.URL, "", "", "", "")
	if streamResp.Code != 200 {
		t.Fatalf("stream serving failed: %d %s", streamResp.Code, streamResp.Body)
	}
	if streamResp.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("expected mpegurl Content-Type, got %s", streamResp.Header().Get("Content-Type"))
	}
	if strings.Contains(streamResp.Body.String(), upstream.URL) || strings.Contains(streamResp.Body.String(), privateToken) {
		t.Fatal("upstream URL or private token leaked in playlist", streamResp.Body)
	}

	// Read segment
	lines := strings.Split(strings.TrimSpace(streamResp.Body.String()), "\n")
	segmentPath := lines[len(lines)-1]
	segResp := call(s, "GET", segmentPath, "", "", "", "")
	if segResp.Code != 200 || segResp.Body.String() != "direct-hls-ts-data" {
		t.Fatalf("segment fetch failed: %d %s", segResp.Code, segResp.Body)
	}
}

func TestAirPlayReceiverProbeFailureUnavailableAndPreservesSession(t *testing.T) {
	privateToken := strings.Repeat("p", 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			fmt.Fprint(w, `{"active":false,"audioActive":true,"metadata":{"title":"Failing Probe"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"airplay": {Enabled: true, URL: upstream.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}

	s.deps.RemoteMedia = &mockReceiverRemoteMedia{
		probeFunc: func(_ context.Context, _ domain.Source) (domain.Metadata, error) {
			return domain.Metadata{}, errors.New("upstream worker probe error")
		},
	}

	token := pair(t, s, "probe-fail-tv")
	// Device with Vizio-like probes (no HLS PASS)
	var dev domain.Device
	s.db.Get(t.Context(), "devices", "probe-fail-tv", &dev)
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		},
	}
	s.db.Put(t.Context(), "devices", dev.ID, dev)

	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "probe-fail-tv", token, ""); w.Code != 200 {
		t.Fatalf("PUT failed: %d %s", w.Code, w.Body)
	}

	resp := call(s, "GET", "/v1/media-receiver", "", "probe-fail-tv", token, "")
	if resp.Code != 502 {
		t.Fatalf("expected 502 receiver_unavailable on probe failure, got %d: %s", resp.Code, resp.Body)
	}
	if strings.Contains(resp.Body.String(), privateToken) || strings.Contains(resp.Body.String(), upstream.URL) {
		t.Fatal("secret leaked in error response", resp.Body)
	}
}

func TestAirPlayReceiverADTSFallbackWhenFMP4Fails(t *testing.T) {
	privateToken := strings.Repeat("c", 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			fmt.Fprint(w, `{"active":false,"audioActive":true,"metadata":{"title":"Unsupported Track"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"airplay": {Enabled: true, URL: upstream.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}

	s.deps.RemoteMedia = &mockReceiverRemoteMedia{
		probeFunc: func(_ context.Context, _ domain.Source) (domain.Metadata, error) {
			meta := domain.Metadata{
				Streams: []domain.Stream{
					{Type: "audio", Codec: "aac", Index: 0},
				},
			}
			meta.Format.Name = "hls,applehttp"
			return meta, nil
		},
	}

	token := pair(t, s, "fmp4-fail-tv")
	// The progressive ADTS fallback does not depend on fragmented MP4 playback.
	var dev domain.Device
	s.db.Get(t.Context(), "devices", "fmp4-fail-tv", &dev)
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
			{ID: "aac-adts", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
			{ID: "http-fmp4", Status: "FAIL", PositionMS: 0, TestedAt: time.Now().Unix()},
		},
	}
	s.db.Put(t.Context(), "devices", dev.ID, dev)

	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "fmp4-fail-tv", token, ""); w.Code != 200 {
		t.Fatalf("PUT failed: %d %s", w.Code, w.Body)
	}

	resp := call(s, "GET", "/v1/media-receiver", "", "fmp4-fail-tv", token, "")
	if resp.Code != 200 {
		t.Fatalf("expected ADTS fallback when fragmented MP4 is unsupported, got %d: %s", resp.Code, resp.Body)
	}
	var snapshot inbox.Snapshot
	if err := json.Unmarshal(resp.Body.Bytes(), &snapshot); err != nil || snapshot.Plan == nil || snapshot.Plan.Mode != "REMUX" || snapshot.Plan.MIME != "audio/aac" {
		t.Fatalf("expected audio/aac REMUX plan, got %s: %v", resp.Body, err)
	}
	if strings.Contains(resp.Body.String(), privateToken) || strings.Contains(resp.Body.String(), upstream.URL) {
		t.Fatal("secret leaked in error response", resp.Body)
	}

	// When aac-adts has failed on a newly connected receiver, it must NOT produce an ADTS remux plan:
	tokenFailed := pair(t, s, "fmp4-fail-adts-fail-tv")
	var devFailed domain.Device
	s.db.Get(t.Context(), "devices", "fmp4-fail-adts-fail-tv", &devFailed)
	devFailed.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(devFailed),
		Probes: []domain.Probe{
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
			{ID: "aac-adts", Status: "FAIL", PositionMS: 0, TestedAt: time.Now().Unix()},
			{ID: "http-fmp4", Status: "FAIL", PositionMS: 0, TestedAt: time.Now().Unix()},
		},
	}
	s.db.Put(t.Context(), "devices", devFailed.ID, devFailed)
	call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay","replaceExisting":true}`, "fmp4-fail-adts-fail-tv", tokenFailed, "")
	resp = call(s, "GET", "/v1/media-receiver", "", "fmp4-fail-adts-fail-tv", tokenFailed, "")
	var snapFailed inbox.Snapshot
	if err := json.Unmarshal(resp.Body.Bytes(), &snapFailed); err != nil {
		t.Fatal(err)
	}
	if snapFailed.Plan != nil {
		t.Fatalf("expected nil plan when ADTS probe failed, got %+v", snapFailed.Plan)
	}

	// Container-agnostic M4A aac alone (without aac-adts) must NOT produce ADTS plan:
	tokenMissing := pair(t, s, "fmp4-fail-no-adts-tv")
	var devMissing domain.Device
	s.db.Get(t.Context(), "devices", "fmp4-fail-no-adts-tv", &devMissing)
	devMissing.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(devMissing),
		Probes: []domain.Probe{
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
			{ID: "http-fmp4", Status: "FAIL", PositionMS: 0, TestedAt: time.Now().Unix()},
		},
	}
	s.db.Put(t.Context(), "devices", devMissing.ID, devMissing)
	call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay","replaceExisting":true}`, "fmp4-fail-no-adts-tv", tokenMissing, "")
	resp = call(s, "GET", "/v1/media-receiver", "", "fmp4-fail-no-adts-tv", tokenMissing, "")
	var snapMissing inbox.Snapshot
	if err := json.Unmarshal(resp.Body.Bytes(), &snapMissing); err != nil {
		t.Fatal(err)
	}
	if snapMissing.Plan != nil {
		t.Fatalf("expected nil plan when aac-adts probe is absent, got %+v", snapMissing.Plan)
	}
}

func TestAirPlayReceiverPreservesPreviousWorkingSessionOnSubsequentFailure(t *testing.T) {
	privateToken := strings.Repeat("w", 32)
	isAudio := atomic.Bool{}
	isAudio.Store(false)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			if !isAudio.Load() {
				fmt.Fprint(w, `{"active":true,"audioActive":false,"metadata":{"title":"Working Screen Video"}}`)
			} else {
				fmt.Fprint(w, `{"active":false,"audioActive":true,"metadata":{"title":"Broken Audio Track"}}`)
			}
		case "/stream/index.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\nvideo.ts\n")
		case "/stream/audio.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\naudio.ts\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"airplay": {Enabled: true, URL: upstream.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}

	probeFailing := atomic.Bool{}
	s.deps.RemoteMedia = &mockReceiverRemoteMedia{
		probeFunc: func(_ context.Context, src domain.Source) (domain.Metadata, error) {
			if probeFailing.Load() {
				return domain.Metadata{}, errors.New("probe failure on track 2")
			}
			meta := domain.Metadata{
				Streams: []domain.Stream{
					{Type: "video", Codec: "h264", Profile: "Main", Width: 1280, Height: 720},
					{Type: "audio", Codec: "aac", Index: 1},
				},
			}
			meta.Format.Name = "hls,applehttp"
			return meta, nil
		},
		convertFunc: func(_ context.Context, _ domain.Source, _ string, _ domain.MediaSelection, out io.Writer) error {
			_, err := io.WriteString(out, "fmp4-working-track-1")
			return err
		},
	}

	token := pair(t, s, "preserve-tv")
	var dev domain.Device
	s.db.Get(t.Context(), "devices", "preserve-tv", &dev)
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		},
	}
	s.db.Put(t.Context(), "devices", dev.ID, dev)

	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "preserve-tv", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}

	// Stream 1 (video) succeeds and creates session
	r1 := call(s, "GET", "/v1/media-receiver", "", "preserve-tv", token, "")
	if r1.Code != 200 {
		t.Fatalf("first stream failed: %d %s", r1.Code, r1.Body)
	}
	var snap1 inbox.Snapshot
	json.Unmarshal(r1.Body.Bytes(), &snap1)
	if snap1.Plan == nil || snap1.Plan.Item.ID != "airplay-live" {
		t.Fatalf("unexpected snapshot 1: %+v", snap1)
	}

	// Verify stream 1 plays
	streamR1 := call(s, "GET", snap1.Plan.URL, "", "", "", "")
	if streamR1.Code != 200 || streamR1.Body.String() != "fmp4-working-track-1" {
		t.Fatalf("stream 1 failed: %d %s", streamR1.Code, streamR1.Body)
	}

	// Now source 2 (audio) arrives, but its probe fails
	isAudio.Store(true)
	probeFailing.Store(true)

	r2 := call(s, "GET", "/v1/media-receiver", "", "preserve-tv", token, "")
	if r2.Code != 502 {
		t.Fatalf("expected 502 on broken stream 2, got %d: %s", r2.Code, r2.Body)
	}

	// Working stream 1 must still be alive and accessible (session was preserved)
	streamR1Again := call(s, "GET", snap1.Plan.URL, "", "", "", "")
	if streamR1Again.Code != 200 || streamR1Again.Body.String() != "fmp4-working-track-1" {
		t.Fatalf("stream 1 was terminated when stream 2 failed: %d %s", streamR1Again.Code, streamR1Again.Body)
	}
}
