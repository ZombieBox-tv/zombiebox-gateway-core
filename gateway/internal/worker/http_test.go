package worker

import (
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

	// 2. Metadata-only state: metadata.txt exists, but manifest is missing
	if err := os.WriteFile(filepath.Join(dir, "metadata.txt"), []byte("Title: Song\nArtist: Singer\nAlbum: Record\n"), 0600); err != nil {
		t.Fatal(err)
	}
	active, audioActive, meta = getStatus()
	if active || audioActive || meta["title"] != "Song" || meta["artist"] != "Singer" || meta["album"] != "Record" {
		t.Fatalf("expected metadata-only state, got active=%v audioActive=%v meta=%v", active, audioActive, meta)
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
	if meta["title"] != "Song" {
		t.Fatalf("expected metadata preserved, got %v", meta)
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
