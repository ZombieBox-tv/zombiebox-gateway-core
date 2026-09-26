package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpotifyWorkerHealthPayloadExposesOnlyAudioFlow(t *testing.T) {
	payload := spotifyWorkerHealth{
		spotifyHealthResult: spotifyHealthResult{Ready: true, AuthMode: "zeroconf"},
		Audio:               spotifyAudioDiagnostic{Available: true, EncodedBytes: 1234, LastEncodedAgeMs: 20, Active: true},
		Daemon:              &spotifyDaemonHealth{FailureCounts: map[string]uint64{"audioKeyRefused": 1}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["ready"] != true || fields["authMode"] != "zeroconf" || fields["audio"] == nil || fields["daemon"] == nil {
		t.Fatalf("health compatibility or audio flow missing: %s", raw)
	}
	for _, forbidden := range []string{"username", "uri", "title", "token", "artwork", "password"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("private field %q appeared in health payload", forbidden)
		}
	}
}

func TestReceiverFilesAndPrivateBoundary(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "hls"), 0700)
	os.WriteFile(filepath.Join(dir, "hls", "index.m3u8"), []byte("#EXTM3U\n"), 0600)
	os.WriteFile(filepath.Join(dir, "secret.json"), []byte("credentials"), 0600)
	c := Config{Mode: "airplay", Token: strings.Repeat("t", 32), StateDir: dir, Pin: "0427"}
	h := Handler(context.Background(), c)
	for _, tc := range []struct {
		path, token string
		code        int
	}{{"/status", "", 401}, {"/status", c.Token, 200}, {"/pairing", "", 401}, {"/pairing", c.Token, 200}, {"/stream/index.m3u8", c.Token, 200}, {"/stream/secret.json", c.Token, 404}, {"/token", c.Token, 404}} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
		if strings.Contains(w.Body.String(), "credentials") {
			t.Fatal("secret file exposed")
		}
		if strings.Contains(w.Body.String(), c.Pin) != (tc.path == "/pairing" && tc.token == c.Token) {
			t.Fatal("AirPlay PIN exposed outside authenticated pairing route")
		}
	}
}

func TestAirPlayControlRequiresAuthenticationCredentialsAndFiniteCommand(t *testing.T) {
	dir := t.TempDir()
	config := Config{Mode: "airplay", Token: strings.Repeat("t", 32), StateDir: dir, Pin: "0427"}
	resolver := &fakeDACPResolver{services: []DACPService{matchingDACPService("192.168.1.42")}}
	var requestHost, requestPath, activeRemote string
	dacpHTTP := &http.Client{Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
		requestHost = request.URL.Host
		requestPath = request.URL.Path
		activeRemote = request.Header.Get("Active-Remote")
		return response(http.StatusNoContent, "", request), nil
	})}
	handler := handlerWithAirPlayDACP(context.Background(), config, nil, nil, resolver, dacpHTTP)
	request := func(body, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/control", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		return result
	}
	commandBody := `{"command":"previtem"}`
	if result := request(commandBody, ""); result.Code != http.StatusUnauthorized || resolver.calls != 0 {
		t.Fatalf("unauthenticated control: status=%d lookups=%d", result.Code, resolver.calls)
	}
	if result := request(commandBody, config.Token); result.Code != http.StatusConflict || resolver.calls != 0 {
		t.Fatalf("disconnected control: status=%d lookups=%d", result.Code, resolver.calls)
	}
	if result := request(`{"command":"volumeup"}`, config.Token); result.Code != http.StatusBadRequest || resolver.calls != 0 {
		t.Fatalf("unlisted control: status=%d lookups=%d", result.Code, resolver.calls)
	}
	if result := request(`{"command":"nextitem","extra":true}`, config.Token); result.Code != http.StatusBadRequest || resolver.calls != 0 {
		t.Fatalf("unknown JSON field: status=%d lookups=%d", result.Code, resolver.calls)
	}
	if result := request(`{"command":"nextitem"} {}`, config.Token); result.Code != http.StatusBadRequest || resolver.calls != 0 {
		t.Fatalf("trailing JSON value: status=%d lookups=%d", result.Code, resolver.calls)
	}
	if result := request(`{"command":"nextitem","padding":"`+strings.Repeat("x", 1024)+`"}`, config.Token); result.Code != http.StatusBadRequest || resolver.calls != 0 {
		t.Fatalf("oversized JSON body: status=%d lookups=%d", result.Code, resolver.calls)
	}
	if err := os.WriteFile(filepath.Join(dir, "receiver.dacp"), []byte(testDACPIdentifier+"\n"+testActiveRemote+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result := request(commandBody, config.Token)
	if result.Code != http.StatusNoContent || resolver.calls != 1 {
		t.Fatalf("valid control: status=%d lookups=%d body=%q", result.Code, resolver.calls, result.Body.String())
	}
	if requestHost != "192.168.1.42:50200" || requestPath != "/ctrl-int/1/previtem" || activeRemote != testActiveRemote {
		t.Fatalf("DACP request target or command incorrect: host=%q path=%q tokenPresent=%t", requestHost, requestPath, activeRemote == testActiveRemote)
	}
	if strings.Contains(result.Body.String(), testActiveRemote) {
		t.Fatal("Active-Remote leaked through the worker response")
	}
}

func TestPCMBridgeProducesMP3(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "audio.pcm"), make([]byte, 44100*4), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Mode: "spotify", Token: strings.Repeat("t", 32), StateDir: dir}
	s := httptest.NewServer(Handler(context.Background(), c))
	defer s.Close()
	r, _ := http.NewRequest("GET", s.URL+"/audio", nil)
	r.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != 200 || len(body) < 1000 || res.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("audio: %d bytes %d %v", res.StatusCode, len(body), err)
	}
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", "pipe:0", "-f", "null", "-")
	cmd.Stdin = strings.NewReader(string(body))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("invalid MP3: %s %v", out, err)
	}
}

func TestAirplayHLSReadinessAndMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	hlsDir := filepath.Join(dir, "hls")
	if err := os.MkdirAll(hlsDir, 0700); err != nil {
		t.Fatal(err)
	}

	c := Config{Mode: "airplay", Token: strings.Repeat("t", 32), StateDir: dir, Pin: "1234"}
	h := Handler(context.Background(), c)

	getStatus := func() (bool, bool, map[string]string) {
		r := httptest.NewRequest("GET", "/status", nil)
		r.Header.Set("Authorization", "Bearer "+c.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("status code %d", w.Code)
		}
		var st struct {
			Active      bool              `json:"active"`
			AudioActive bool              `json:"audioActive"`
			Metadata    map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		return st.Active, st.AudioActive, st.Metadata
	}

	// 1. Initial state: no manifest, no metadata
	active, audioActive, meta := getStatus()
	if active || audioActive || len(meta) != 0 {
		t.Fatalf("expected inactive and empty, got active=%v audioActive=%v meta=%v", active, audioActive, meta)
	}

	// 2. Metadata-only state: metadata.txt exists, but neither stream is active.
	if err := os.WriteFile(filepath.Join(dir, "metadata.txt"), []byte("Title: Song\nArtist: Singer\nAlbum: Record\n"), 0600); err != nil {
		t.Fatal(err)
	}
	active, audioActive, meta = getStatus()
	if active || audioActive || len(meta) != 0 {
		t.Fatalf("expected inactive state without stale metadata, got active=%v audioActive=%v meta=%v", active, audioActive, meta)
	}

	// 3. Empty manifest: audio.m3u8 is 0 bytes
	manifestPath := filepath.Join(hlsDir, "audio.m3u8")
	if err := os.WriteFile(manifestPath, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	if airplayHLSPlayable(hlsDir, "audio.m3u8") {
		t.Fatal("empty manifest must not be playable")
	}
	_, audioActive, _ = getStatus()
	if audioActive {
		t.Fatal("status audioActive must be false on empty manifest")
	}

	// 4. Tiny manifest (exact physical failure observed on Vizio / Apple Music):
	// TARGETDURATION: 0 and 4 EXTINF entries of ~0.000011s with 1128-byte segments
	tinyManifest := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:0\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:0.000011,\naudio0.ts\n" +
		"#EXTINF:0.000011,\naudio1.ts\n" +
		"#EXTINF:0.000011,\naudio2.ts\n" +
		"#EXTINF:0.000011,\naudio3.ts\n"
	if err := os.WriteFile(manifestPath, []byte(tinyManifest), 0600); err != nil {
		t.Fatal(err)
	}
	tinySegBytes := make([]byte, 1128)
	for i := 0; i < 4; i++ {
		if err := os.WriteFile(filepath.Join(hlsDir, fmt.Sprintf("audio%d.ts", i)), tinySegBytes, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if airplayHLSPlayable(hlsDir, "audio.m3u8") {
		t.Fatal("tiny segments with TARGETDURATION 0 must not be playable")
	}
	_, audioActive, meta = getStatus()
	if audioActive {
		t.Fatal("status audioActive must be false on tiny manifest")
	}
	if len(meta) != 0 {
		t.Fatalf("inactive status must omit metadata, got %v", meta)
	}

	// 5. Atomic manifests:
	// 5a. Manifest references a segment that does not exist yet (atomic write in flight)
	atomicManifest := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:1.000000,\naudio0.ts\n" +
		"#EXTINF:1.000000,\naudio_atomic_missing.ts\n"
	validSegBytes := make([]byte, 16384)
	if err := os.WriteFile(filepath.Join(hlsDir, "audio0.ts"), validSegBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(atomicManifest), 0600); err != nil {
		t.Fatal(err)
	}
	if airplayHLSPlayable(hlsDir, "audio.m3u8") {
		t.Fatal("atomic manifest with missing in-flight segment must not be playable")
	}

	// 6. Stale manifest & stale segments:
	// 6a. Stale segment: segment file mtime > 15s ago
	validManifest := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:1.000000,\naudio0.ts\n" +
		"#EXTINF:1.000000,\naudio1.ts\n"
	if err := os.WriteFile(filepath.Join(hlsDir, "audio1.ts"), validSegBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(validManifest), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(filepath.Join(hlsDir, "audio0.ts"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if airplayHLSPlayable(hlsDir, "audio.m3u8") {
		t.Fatal("manifest with stale segment (>15s) must not be playable")
	}

	// 6b. Stale manifest: manifest mtime > 15s ago
	now := time.Now()
	if err := os.Chtimes(filepath.Join(hlsDir, "audio0.ts"), now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(manifestPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if airplayHLSPlayable(hlsDir, "audio.m3u8") {
		t.Fatal("stale manifest file (>15s) must not be playable")
	}

	// 7. Valid manifest: fresh, target duration >= 1, multiple seconds of continuous media (> 4KB segments, dur >= 1s)
	if err := os.Chtimes(manifestPath, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(hlsDir, "audio0.ts"), now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(hlsDir, "audio1.ts"), now, now); err != nil {
		t.Fatal(err)
	}
	if !airplayHLSPlayable(hlsDir, "audio.m3u8") {
		t.Fatal("valid manifest with fresh segments must be playable")
	}
	active, audioActive, meta = getStatus()
	if !audioActive || meta["title"] != "Song" {
		t.Fatalf("expected audioActive=true and metadata populated, got audioActive=%v meta=%v", audioActive, meta)
	}
}

func TestAirPlayVideoRequiresReadySegmentNotJustManifest(t *testing.T) {
	dir := t.TempDir()
	hlsDir := filepath.Join(dir, "hls")
	if err := os.MkdirAll(hlsDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\nsegment0.ts\n"
	if err := os.WriteFile(filepath.Join(hlsDir, "index.m3u8"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if video, _ := airplayStreamActivity(dir); video {
		t.Fatal("fresh playlist without a completed segment reported video")
	}
	segment := filepath.Join(hlsDir, "segment0.ts")
	if err := os.WriteFile(segment, make([]byte, 16<<10), 0600); err != nil {
		t.Fatal(err)
	}
	if video, _ := airplayStreamActivity(dir); !video {
		t.Fatal("fresh playlist with a complete segment did not report video")
	}
	old := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(segment, old, old); err != nil {
		t.Fatal(err)
	}
	if video, _ := airplayStreamActivity(dir); video {
		t.Fatal("stale segment reported live video")
	}
}

func TestAirplayStatusAndArtworkClearWhenStreamBecomesIdle(t *testing.T) {
	dir := t.TempDir()
	hlsDir := filepath.Join(dir, "hls")
	if err := os.MkdirAll(hlsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.txt"), []byte("Title: Current Song\nArtist: Current Artist\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cover := []byte("\x89PNG\r\n\x1a\n")
	if err := os.WriteFile(filepath.Join(dir, "coverart"), cover, 0600); err != nil {
		t.Fatal(err)
	}
	metadataTime := time.Now().Add(-2 * time.Second)
	coverTime := metadataTime.Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "metadata.txt"), metadataTime, metadataTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "coverart"), coverTime, coverTime); err != nil {
		t.Fatal(err)
	}
	manifest := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.000000,\naudio0.ts\n"
	if err := os.WriteFile(filepath.Join(hlsDir, "audio.m3u8"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hlsDir, "audio0.ts"), make([]byte, 16384), 0600); err != nil {
		t.Fatal(err)
	}

	c := Config{Mode: "airplay", Token: strings.Repeat("t", 32), StateDir: dir, Pin: "1234"}
	h := Handler(context.Background(), c)
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+c.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	readMetadata := func(response *httptest.ResponseRecorder) (bool, bool, map[string]string) {
		t.Helper()
		var status struct {
			Active      bool              `json:"active"`
			AudioActive bool              `json:"audioActive"`
			Metadata    map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		return status.Active, status.AudioActive, status.Metadata
	}

	active, audioActive, metadata := readMetadata(request("/status"))
	if active || !audioActive || metadata["title"] != "Current Song" {
		t.Fatalf("active audio status lost current metadata: active=%v audioActive=%v metadata=%v", active, audioActive, metadata)
	}
	artworkRevision := airplayArtworkRevision(map[string]string{
		"title": "Current Song", "artist": "Current Artist",
	})
	artwork := request("/artwork?rev=" + artworkRevision)
	if artwork.Code != http.StatusNotFound {
		t.Fatalf("unassociated artwork was served: code=%d body=%x", artwork.Code, artwork.Body.Bytes())
	}

	if err := os.Remove(filepath.Join(hlsDir, "audio.m3u8")); err != nil {
		t.Fatal(err)
	}
	active, audioActive, metadata = readMetadata(request("/status"))
	if active || audioActive || len(metadata) != 0 {
		t.Fatalf("idle transition retained track metadata: active=%v audioActive=%v metadata=%v", active, audioActive, metadata)
	}
	artwork = request("/artwork?rev=" + artworkRevision)
	if artwork.Code != http.StatusNotFound {
		t.Fatalf("idle stream served stale artwork: code=%d body=%x", artwork.Code, artwork.Body.Bytes())
	}
}

func TestAirplayStatusReturnsOnlyOpaqueConnectionEvidence(t *testing.T) {
	dir := t.TempDir()
	privateData := []byte{0x51, '\n', 0x62, '\n'}
	if err := os.WriteFile(filepath.Join(dir, "receiver.dacp"), privateData, 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Mode: "airplay", Token: strings.Repeat("t", 32), StateDir: dir, Pin: "1234"}
	h := Handler(context.Background(), c)
	r := httptest.NewRequest("GET", "/status", nil)
	r.Header.Set("Authorization", "Bearer "+c.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status returned %d", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), privateData) {
		t.Fatal("private connection file bytes escaped through status")
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	var connected bool
	if err := json.Unmarshal(status["connected"], &connected); err != nil || !connected {
		t.Fatalf("connection presence missing: %s", w.Body.Bytes())
	}
	var known bool
	if err := json.Unmarshal(status["connectionKnown"], &known); err != nil || !known {
		t.Fatalf("connection evidence state missing: %s", w.Body.Bytes())
	}
	var revision string
	if err := json.Unmarshal(status["connectionRevision"], &revision); err != nil || len(revision) != 16 {
		t.Fatalf("opaque revision missing or unbounded: %s", w.Body.Bytes())
	}
	if len(status) != 7 {
		t.Fatalf("unexpected private status fields: %s", w.Body.Bytes())
	}
	var artworkRevision string
	if err := json.Unmarshal(status["artworkRevision"], &artworkRevision); err != nil || artworkRevision != "" {
		t.Fatalf("unassociated artwork exposed a revision: %s", w.Body.Bytes())
	}
	var progressKnown bool
	if err := json.Unmarshal(status["progressKnown"], &progressKnown); err != nil || progressKnown {
		t.Fatalf("absent sender progress must remain unknown: %s", w.Body.Bytes())
	}
	for _, field := range []string{"positionMs", "durationMs", "positionAgeMs"} {
		if _, exists := status[field]; exists {
			t.Fatalf("unknown sender position included %q: %s", field, w.Body.Bytes())
		}
	}
}

func TestAirplayMetadataWaitsForNewConnectionAndClearsAfterDisconnect(t *testing.T) {
	dir := t.TempDir()
	hlsDir := filepath.Join(dir, "hls")
	if err := os.MkdirAll(hlsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hlsDir, "index.m3u8"), []byte("#EXTM3U\n"), 0600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(dir, "metadata.txt")
	connectionPath := filepath.Join(dir, "receiver.dacp")
	if err := os.WriteFile(metadataPath, []byte("Title: Old Track\nArtist: Artist\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connectionPath, []byte("x\ny\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(metadataPath, now.Add(-time.Second), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(connectionPath, now, now); err != nil {
		t.Fatal(err)
	}
	c := Config{Mode: "airplay", Token: strings.Repeat("t", 32), StateDir: dir, Pin: "1234"}
	h := Handler(context.Background(), c)
	request := func() map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", "/status", nil)
		r.Header.Set("Authorization", "Bearer "+c.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status returned %d", w.Code)
		}
		var status map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	if status := request(); status["metadata"] != nil {
		t.Fatalf("previous connection metadata leaked into a new connection: %+v", status)
	}
	if err := os.WriteFile(metadataPath, []byte("Title: New Track\nArtist: Artist\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metadataPath, now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	status := request()
	metadata, ok := status["metadata"].(map[string]any)
	if !ok || metadata["title"] != "New Track" {
		t.Fatalf("metadata written after the connection was not exposed: %+v", status)
	}
	if err := os.Remove(connectionPath); err != nil {
		t.Fatal(err)
	}
	status = request()
	if status["metadata"] != nil {
		t.Fatalf("metadata remained exposed after connection evidence disappeared: %+v", status)
	}
}

func TestSpotifyStatusAugmentsDaemonDiagnosticsWithoutSecrets(t *testing.T) {
	var stopCalled atomic.Bool
	var exposeTrack atomic.Bool
	stopCh := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/player/stop" {
			stopCalled.Store(true)
			select {
			case stopCh <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"stopped":true}`)
			return
		}
		if r.URL.Path == "/status" {
			w.WriteHeader(http.StatusOK)
			if exposeTrack.Load() {
				_, _ = io.WriteString(w, `{"stopped":false,"track":{"uri":"spotify:track:private-uri","name":"Visible song","artist_names":["Artist"],"duration":1234,"position":56},"username":"secret-account-name","context_uri":"spotify:playlist:private-context","device_id":"private-device"}`)
				return
			}
			_, _ = io.WriteString(w, `{"stopped":false,"buffering":true,"track":null,"volume":75,"volume_steps":100,"username":"secret-account-name"}`)
			return
		}
		if r.URL.Path == "/player/resume" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"stopped":false,"paused":false}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	t.Setenv("ZOMBIE_SPOTIFY_DAEMON_URL", upstream.URL)

	dir := t.TempDir()
	c := Config{Mode: "spotify", Token: strings.Repeat("k", 32), StateDir: dir}
	diagnostics := NewSpotifyDaemonDiagnostics()
	handler := HandlerWithSpotifyDiagnostics(context.Background(), c, diagnostics)

	// Call GET /status through the worker
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 from status proxy, got %d: %s", rec.Code, rec.Body.String())
	}
	bodyStr := rec.Body.String()
	if strings.Contains(bodyStr, "secret-account-name") {
		t.Fatalf("private username leaked through /status proxy: %s", bodyStr)
	}
	var statusMap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusMap); err != nil {
		t.Fatalf("invalid json from /status proxy: %v", err)
	}
	if statusMap["daemon"] == nil {
		t.Fatalf("daemon diagnostics missing from augmented /status response: %s", bodyStr)
	}
	exposeTrack.Store(true)
	req = httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Visible song") {
		t.Fatalf("expected safe track metadata, got %d: %s", rec.Code, rec.Body.String())
	}
	for _, forbidden := range []string{"private-uri", "private-context", "private-device", "secret-account-name"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("private daemon field leaked through /status: %s", rec.Body.String())
		}
	}
	exposeTrack.Store(false)

	// Now trigger 3 audio key refusals
	private := "spotify:track:private-uri secret-token"
	for i := 0; i < 3; i++ {
		diagnostics.Write([]byte("skipping track: Spotify refused the audio key (code 1) for this playback context: " + private + "\n"))
	}

	select {
	case <-stopCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for worker to send /player/stop on refusal limit")
	}

	if !stopCalled.Load() {
		t.Fatal("expected /player/stop to be called when skip storm refusal limit was hit")
	}

	// Verify augmented /status reflects RefusalLimited
	req = httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), private) || strings.Contains(rec.Body.String(), "secret-account-name") {
		t.Fatalf("secrets leaked in /status: %s", rec.Body.String())
	}
	var updatedMap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &updatedMap); err != nil {
		t.Fatal(err)
	}
	daemonField, ok := updatedMap["daemon"].(map[string]any)
	if !ok || daemonField["refusalLimited"] != true {
		t.Fatalf("refusalLimited missing or false in /status daemon field: %+v", updatedMap)
	}
	if daemonField["stopAttempts"] != float64(1) || daemonField["lastStopResult"] != "ok" || daemonField["lastStopStatus"] != float64(200) {
		t.Fatalf("expected recorded stop diagnostics in daemon field: %+v", daemonField)
	}

	// Fresh auto-reconnect attempt while circuit open (cooldown window active):
	// /status reflects that refusal limit remains active.
	req = httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updatedMap); err != nil {
		t.Fatal(err)
	}
	daemonField = updatedMap["daemon"].(map[string]any)
	if daemonField["refusalLimited"] != true {
		t.Fatalf("refusalLimited must remain true during auto-reconnect cooldown: %+v", updatedMap)
	}

	// Manual retry via explicit user action: POST /player/resume
	req = httptest.NewRequest("POST", "/player/resume", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 from /player/resume, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify /status after resume shows refusal limit cleared
	req = httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updatedMap); err != nil {
		t.Fatal(err)
	}
	daemonField = updatedMap["daemon"].(map[string]any)
	if daemonField["refusalLimited"] == true || (daemonField["consecutiveRefusals"] != nil && daemonField["consecutiveRefusals"] != float64(0)) {
		t.Fatalf("refusalLimited and consecutiveRefusals must clear after manual resume: %+v", updatedMap)
	}
}

func TestSpotifyStopCallbackHTTPTransportFailureAndResults(t *testing.T) {
	stopFailed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/player/stop" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"internal error"}`)
			select {
			case stopFailed <- struct{}{}:
			default:
			}
			return
		}
		if r.URL.Path == "/status" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"stopped":false,"buffering":true,"track":null,"username":"secret-account"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	t.Setenv("ZOMBIE_SPOTIFY_DAEMON_URL", upstream.URL)

	dir := t.TempDir()
	c := Config{Mode: "spotify", Token: strings.Repeat("k", 32), StateDir: dir}
	diagnostics := NewSpotifyDaemonDiagnostics()
	handler := HandlerWithSpotifyDiagnostics(context.Background(), c, diagnostics)

	// Trigger 3 key refusals to hit limit
	private := "spotify:track:private-uri secret-token"
	for i := 0; i < 3; i++ {
		diagnostics.Write([]byte("skipping track: Spotify refused the audio key (code 1) for this playback context: " + private + "\n"))
	}

	select {
	case <-stopFailed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for worker to attempt /player/stop")
	}

	// Verify status records http_error without crashing or leaking secrets
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	bodyStr := rec.Body.String()
	for _, forbidden := range []string{private, "secret-account", "secret-token", "internal error"} {
		if strings.Contains(bodyStr, forbidden) {
			t.Fatalf("leaked secret or raw error in /status: %s", bodyStr)
		}
	}
	var resMap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resMap); err != nil {
		t.Fatal(err)
	}
	daemonField, ok := resMap["daemon"].(map[string]any)
	if !ok || daemonField["refusalLimited"] != true {
		t.Fatalf("refusalLimited must be true despite transport failure: %+v", resMap)
	}
	if daemonField["lastStopResult"] != "http_error" || daemonField["lastStopStatus"] != float64(500) {
		t.Fatalf("expected http_error status 500 recorded: %+v", daemonField)
	}
}

func TestSpotifyStatusAndHealthDoNotClaimPlaybackOnKeyRefusalWithZeroEncodedBytes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/code" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/status" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"stopped":false,"buffering":true,"track":null,"volume":80,"volume_steps":100,"username":"secret-account"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	t.Setenv("ZOMBIE_SPOTIFY_DAEMON_URL", upstream.URL)

	dir := t.TempDir()
	c := Config{Mode: "spotify", Token: strings.Repeat("k", 32), StateDir: dir}
	diagnostics := NewSpotifyDaemonDiagnostics()
	handler := HandlerWithSpotifyDiagnostics(context.Background(), c, diagnostics)

	diagnostics.Write([]byte("playback unavailable: Spotify refused the audio key (code 1) for this playback context: spotify:track:private-uri\n"))

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /status, got %d: %s", rec.Code, rec.Body.String())
	}
	var statusMap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusMap); err != nil {
		t.Fatalf("invalid json from /status: %v", err)
	}
	if statusMap["stopped"] != true {
		t.Fatalf("expected stopped=true after key refusal with zero encoded bytes, got: %+v", statusMap)
	}
	if statusMap["buffering"] != false {
		t.Fatalf("expected buffering=false after key refusal with zero encoded bytes, got: %+v", statusMap)
	}

	req = httptest.NewRequest("GET", "/health", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from /health, got %d: %s", rec.Code, rec.Body.String())
	}
	var healthMap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &healthMap); err != nil {
		t.Fatalf("invalid json from /health: %v", err)
	}
	if healthMap["stopped"] != true {
		t.Fatalf("expected health stopped=true after key refusal with zero encoded bytes, got: %+v", healthMap)
	}
	if healthMap["bufferingWithoutTrack"] == true {
		t.Fatalf("expected health bufferingWithoutTrack=false after key refusal with zero encoded bytes, got: %+v", healthMap)
	}
	for _, forbidden := range []string{"private-uri", "secret-account"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("secret leaked in /health response: %s", rec.Body.String())
		}
	}
}
