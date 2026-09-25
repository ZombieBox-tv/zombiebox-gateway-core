package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/providers"
)

func TestHTTPConversionProducesPlayableMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg absent")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe absent")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "sample.mkv")
	cmd := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-t", "0.5", "-c:v", "libx264", "-threads", "1", input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	s := testServer(t, nil, dir)
	s.deps.Media = media.New("ffmpeg", "ffprobe")
	token := pair(t, s, "media-device")
	sources, err := providers.Local(dir)
	if err != nil || len(sources) != 1 {
		t.Fatalf("local MKV catalog: %v %d", err, len(sources))
	}
	for _, mode := range []string{"AUTO", "TRANSCODE"} {
		w := call(s, "POST", "/v1/playback", `{"itemId":"`+sources[0].Item.ID+`","mode":"`+mode+`"}`, "media-device", token, "")
		if w.Code != 201 {
			t.Fatalf("plan: %d %s", w.Code, w.Body)
		}
		var plan domain.Plan
		json.Unmarshal(w.Body.Bytes(), &plan)
		expected := mode
		if mode == "AUTO" {
			expected = "REMUX"
		}
		if plan.Mode != expected || plan.Seekable || plan.ResumeMS != 0 || plan.MIME != "video/mp4" {
			t.Fatalf("invalid plan: %+v", plan)
		}
		stream := call(s, "GET", plan.URL, "", "", "", "")
		if stream.Code != 200 {
			t.Fatal(stream.Code, stream.Body)
		}
		output := filepath.Join(t.TempDir(), "output.mp4")
		os.WriteFile(output, stream.Body.Bytes(), 0600)
		metadata, err := s.deps.Media.Probe(t.Context(), output)
		if err != nil || len(metadata.Streams) == 0 || metadata.Streams[0].Codec != "h264" {
			t.Fatalf("bad conversion: %v %+v", err, metadata)
		}
		if mode == "TRANSCODE" && metadata.Streams[0].Profile != "Constrained Baseline" {
			t.Fatal(metadata.Streams[0])
		}
		call(s, "DELETE", "/v1/playback/"+plan.SessionID, "", "media-device", token, "")
	}
}

type stubRemoteMedia struct {
	metadata domain.Metadata
	err      error
}

func (s stubRemoteMedia) ProbeRemote(context.Context, domain.Source) (domain.Metadata, error) {
	return s.metadata, s.err
}

func (stubRemoteMedia) ConvertRemote(context.Context, domain.Source, string, domain.MediaSelection, io.Writer) error {
	return nil
}

func TestHLSPlanningEvidenceSelectsDirectOrFallback(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	now := time.Now().Unix()

	hlsMetadata := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "Main", Width: 1280, Height: 720},
			{Type: "audio", Codec: "aac"},
		},
	}
	hlsMetadata.Format.Name = "hls,applehttp"
	s.deps.RemoteMedia = stubRemoteMedia{metadata: hlsMetadata}

	hlsSource := domain.Source{
		URL:  "https://stream.provider.test/live/master.m3u8",
		MIME: "application/vnd.apple.mpegurl",
		Item: domain.Item{ID: "iptv-channel-1", Provider: "iptv"},
	}

	token := pair(t, s, "hls-client")
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "hls-client", &dev); err != nil {
		t.Fatal(err)
	}
	validCacheKey := devices.ProbeCacheKey(dev)

	// 1. Fresh HLS PASS evidence -> DIRECT_PLAY
	devWithEvidence := dev
	devWithEvidence.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     validCacheKey,
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 800, TestedAt: now},
		},
	}
	decision, err := s.playbackMode(t.Context(), hlsSource, devWithEvidence, "AUTO")
	if err != nil || decision.mode != "DIRECT_PLAY" {
		t.Fatalf("expected DIRECT_PLAY with fresh HLS evidence, got %s err=%v", decision.mode, err)
	}

	// 2. Failed HLS evidence -> REMUX
	devWithFail := dev
	devWithFail.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     validCacheKey,
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "FAIL", TestedAt: now},
		},
	}
	decisionFail, err := s.playbackMode(t.Context(), hlsSource, devWithFail, "AUTO")
	if err != nil || decisionFail.mode != "REMUX" {
		t.Fatalf("expected REMUX with failed HLS evidence, got %s err=%v", decisionFail.mode, err)
	}

	// 3. Missing evidence -> REMUX
	decisionEmpty, err := s.playbackMode(t.Context(), hlsSource, dev, "AUTO")
	if err != nil || decisionEmpty.mode != "REMUX" {
		t.Fatalf("expected REMUX without evidence, got %s err=%v", decisionEmpty.mode, err)
	}

	// 4. Stale cache key -> REMUX
	devStale := devWithEvidence
	devStale.Capabilities.CacheKey = "stale-key-mismatch"
	decisionStale, err := s.playbackMode(t.Context(), hlsSource, devStale, "AUTO")
	if err != nil || decisionStale.mode != "REMUX" {
		t.Fatalf("expected REMUX with stale cache key, got %s err=%v", decisionStale.mode, err)
	}

	// 5. Incompatible video codec (VP9) -> TRANSCODE
	vp9Metadata := hlsMetadata
	vp9Metadata.Streams = []domain.Stream{
		{Type: "video", Codec: "vp9", Width: 1280, Height: 720},
		{Type: "audio", Codec: "aac"},
	}
	s.deps.RemoteMedia = stubRemoteMedia{metadata: vp9Metadata}
	decisionVP9, err := s.playbackMode(t.Context(), hlsSource, devWithEvidence, "AUTO")
	if err != nil || decisionVP9.mode != "TRANSCODE" {
		t.Fatalf("expected TRANSCODE with VP9 in HLS, got %s err=%v", decisionVP9.mode, err)
	}

	// 6. Incompatible audio codec (AC3) -> TRANSCODE
	ac3Metadata := hlsMetadata
	ac3Metadata.Streams = []domain.Stream{
		{Type: "video", Codec: "h264", Width: 1280, Height: 720},
		{Type: "audio", Codec: "ac3"},
	}
	s.deps.RemoteMedia = stubRemoteMedia{metadata: ac3Metadata}
	decisionAC3, err := s.playbackMode(t.Context(), hlsSource, devWithEvidence, "AUTO")
	if err != nil || decisionAC3.mode != "TRANSCODE" {
		t.Fatalf("expected TRANSCODE with AC3 in HLS, got %s err=%v", decisionAC3.mode, err)
	}

	_ = token
}

func TestNativeHLSServesProxiedManifestWithoutExposingOrigin(t *testing.T) {
	s := testServer(t, nil, t.TempDir())

	playlist := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nsegment1.ts\n#EXT-X-ENDLIST\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(playlist))
	}))
	defer upstream.Close()

	s.deps.StreamHTTP = upstream.Client()
	sessID := "hls-direct-sess"
	ticket := "sess-ticket-123"
	s.sessions[sessID] = &session{
		mode:    "DIRECT_PLAY",
		ticket:  ticket,
		expires: time.Now().Add(time.Hour),
		ctx:     t.Context(),
		cancel:  func() {},
		source: domain.Source{
			URL:     upstream.URL + "/live.m3u8",
			MIME:    "application/vnd.apple.mpegurl",
			Headers: http.Header{"Authorization": {"Bearer secret-token"}},
		},
		resources: map[string]string{},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/streams/"+sessID+"?ticket="+ticket, nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("stream handler status %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "#EXTM3U") {
		t.Fatalf("expected HLS manifest, got %s", body)
	}
	if strings.Contains(body, "secret-token") || strings.Contains(body, upstream.URL) {
		t.Fatalf("origin secret or URL leaked in proxied manifest: %s", body)
	}
	if !strings.Contains(body, "/v1/streams/"+sessID+"/") {
		t.Fatalf("expected rewritten proxy segment URL in manifest: %s", body)
	}
}
