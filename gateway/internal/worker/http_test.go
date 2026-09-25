package worker

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
)

func TestSpotifyWorkerHealthPayloadExposesOnlyAudioFlow(t *testing.T) {
	payload := spotifyWorkerHealth{
		spotifyHealthResult: spotifyHealthResult{Ready: true, AuthMode: "zeroconf"},
		Audio:               spotifyAudioDiagnostic{Available: true, EncodedBytes: 1234, LastEncodedAgeMs: 20, Active: true},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["ready"] != true || fields["authMode"] != "zeroconf" || fields["audio"] == nil {
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
