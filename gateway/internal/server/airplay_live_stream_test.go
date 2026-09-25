package server

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
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/receivers/inbox"
)

func TestAirPlayLiveAudioStreamRealFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}

	dir := t.TempDir()

	// Generate 4 AAC 44.1kHz stereo TS segments (~0.975s each, matching AirPlay worker output)
	segNames := []string{"audio0.ts", "audio1.ts", "audio2.ts", "audio3.ts"}
	for i, seg := range segNames {
		segPath := filepath.Join(dir, seg)
		args := []string{
			"-y", "-nostdin", "-v", "error",
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=44100", 440+i*110),
			"-t", "0.975",
			"-c:a", "aac", "-b:a", "128k", "-ac", "2",
			"-f", "mpegts", segPath,
		}
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("failed to generate segment %s: %v %s", seg, err, out)
		}
	}

	manifest := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:1\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:0.975,\n" +
		"audio0.ts\n" +
		"#EXTINF:0.975,\n" +
		"audio1.ts\n" +
		"#EXTINF:0.975,\n" +
		"audio2.ts\n" +
		"#EXTINF:0.975,\n" +
		"audio3.ts\n"

	const token = "airplay-test-secret-token-32chars!"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Upstream request: %s %s", r.Method, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/status":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"active":false,"audioActive":true,"metadata":{"title":"Test Song","artist":"Test Artist","album":"Test Album"}}`)
		case "/stream/audio.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(200)
			w.Write([]byte(manifest))
		default:
			base := filepath.Base(r.URL.Path)
			segPath := filepath.Join(dir, base)
			if data, err := os.ReadFile(segPath); err == nil {
				w.Header().Set("Content-Type", "video/mp2t")
				w.WriteHeader(200)
				w.Write(data)
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{
		"airplay": {Enabled: true, URL: upstream.URL, Token: token},
	}); err != nil {
		t.Fatal(err)
	}

	localMedia := media.New(ffmpeg, ffprobe)
	s.deps.Media = localMedia
	s.deps.RemoteMedia = media.NewRemote(localMedia, upstream.Client())

	deviceToken := pair(t, s, "vizio-tv")
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "vizio-tv", &dev); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "mpegts-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "hls-h264-aac", Status: "UNKNOWN", TestedAt: now},
		},
	}
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}

	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "vizio-tv", deviceToken, ""); w.Code != 200 {
		t.Fatalf("PUT /v1/media-receiver failed: %d %s", w.Code, w.Body)
	}

	recResp := call(s, "GET", "/v1/media-receiver", "", "vizio-tv", deviceToken, "")
	if recResp.Code != 200 {
		t.Fatalf("GET /v1/media-receiver failed: %d %s", recResp.Code, recResp.Body)
	}

	var snapshot inbox.Snapshot
	if err := json.Unmarshal(recResp.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Plan == nil {
		t.Fatal("expected non-nil Plan")
	}
	if snapshot.Plan.Mode != "REMUX" {
		t.Fatalf("expected REMUX mode, got %s", snapshot.Plan.Mode)
	}
	if snapshot.Plan.MIME != "audio/mp4" {
		t.Fatalf("expected audio/mp4 MIME, got %s", snapshot.Plan.MIME)
	}

	gwServer := httptest.NewServer(s)
	defer gwServer.Close()

	clientReq, err := http.NewRequestWithContext(t.Context(), "GET", gwServer.URL+snapshot.Plan.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := gwServer.Client().Do(clientReq)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()

	headerLatency := time.Since(start)
	t.Logf("Response headers received after %v with status %d", headerLatency, resp.StatusCode)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status 200, got %d: %s", resp.StatusCode, body)
	}
	if headerLatency > 3*time.Second {
		t.Fatalf("header latency too slow: %v", headerLatency)
	}
	if ctype := resp.Header.Get("Content-Type"); ctype != "audio/mp4" {
		t.Fatalf("expected Content-Type audio/mp4, got %s", ctype)
	}

	// Read first chunk
	firstBuf := make([]byte, 1024)
	n, err := resp.Body.Read(firstBuf)
	firstByteLatency := time.Since(start)
	t.Logf("First byte read (%d bytes) after %v (err=%v)", n, firstByteLatency, err)
	if n == 0 {
		t.Fatal("read 0 bytes on stream")
	}
	if firstByteLatency > 3*time.Second {
		t.Fatalf("first byte latency too slow: %v", firstByteLatency)
	}

	// Read up to 64KB or for 2 seconds
	var collected bytes.Buffer
	collected.Write(firstBuf[:n])
	readDeadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 4096)
	for collected.Len() < 64*1024 && time.Now().Before(readDeadline) {
		type readResult struct {
			n   int
			err error
		}
		ch := make(chan readResult, 1)
		go func() {
			num, rErr := resp.Body.Read(buf)
			ch <- readResult{n: num, err: rErr}
		}()
		select {
		case res := <-ch:
			if res.n > 0 {
				collected.Write(buf[:res.n])
				t.Logf("Read %d bytes (total=%d)", res.n, collected.Len())
			}
			if res.err != nil {
				t.Logf("Read returned err: %v", res.err)
				goto doneReading
			}
		case <-time.After(500 * time.Millisecond):
			t.Logf("Read timed out waiting for data (total=%d)", collected.Len())
			goto doneReading
		}
	}
doneReading:

	t.Logf("Collected %d bytes of fMP4 in %v", collected.Len(), time.Since(start))
	if collected.Len() < 4096 {
		t.Fatalf("expected at least 4KB of playable fMP4 audio, got %d bytes", collected.Len())
	}

	// Probe collected bytes to verify valid AAC audio in fMP4
	outPath := filepath.Join(dir, "vizio_stream.mp4")
	if err := os.WriteFile(outPath, collected.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer probeCancel()
	meta, err := localMedia.Probe(probeCtx, outPath)
	if err != nil {
		t.Fatalf("failed to probe client stream MP4: %v", err)
	}
	if len(meta.Streams) != 1 || meta.Streams[0].Type != "audio" || meta.Streams[0].Codec != "aac" {
		t.Fatalf("expected 1 AAC audio stream, got %+v", meta.Streams)
	}
	t.Logf("Probe successful: stream is valid AAC fMP4!")
}

func TestAirPlayLiveAudioStreamClientCancellationReleasesSlot(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}

	dir := t.TempDir()
	segPath := filepath.Join(dir, "audio0.ts")
	args := []string{
		"-y", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100",
		"-t", "0.975",
		"-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-f", "mpegts", segPath,
	}
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("failed to generate segment: %v %s", err, out)
	}

	manifest := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:0.975,\naudio0.ts\n"
	const token = "airplay-test-secret-token-32chars!"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"active":false,"audioActive":true,"metadata":{"title":"Cancel Song","artist":"Artist"}}`)
		case "/stream/audio.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(200)
			w.Write([]byte(manifest))
		default:
			data, err := os.ReadFile(segPath)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "video/mp2t")
			w.WriteHeader(200)
			w.Write(data)
		}
	}))
	defer upstream.Close()

	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{
		"airplay": {Enabled: true, URL: upstream.URL, Token: token},
	}); err != nil {
		t.Fatal(err)
	}
	localMedia := media.New(ffmpeg, ffprobe)
	s.deps.Media = localMedia
	s.deps.RemoteMedia = media.NewRemote(localMedia, upstream.Client())

	deviceToken := pair(t, s, "vizio-tv")
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "vizio-tv", &dev); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "mpegts-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "hls-h264-aac", Status: "UNKNOWN", TestedAt: now},
		},
	}
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}

	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "vizio-tv", deviceToken, ""); w.Code != 200 {
		t.Fatalf("PUT failed: %d", w.Code)
	}
	recResp := call(s, "GET", "/v1/media-receiver", "", "vizio-tv", deviceToken, "")
	var snapshot inbox.Snapshot
	if err := json.Unmarshal(recResp.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Plan == nil {
		t.Fatal("expected a receiver playback plan")
	}

	gwServer := httptest.NewServer(s)
	defer gwServer.Close()

	clientCtx, clientCancel := context.WithCancel(t.Context())
	clientReq, err := http.NewRequestWithContext(clientCtx, "GET", gwServer.URL+snapshot.Plan.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := gwServer.Client().Do(clientReq)
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1024)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}

	// Cancel client request while stream is active
	clientCancel()
	resp.Body.Close()

	// Verify stream capacity is freed
	deadline := time.Now().Add(3 * time.Second)
	freed := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		streamCount := len(s.streams)
		s.mu.Unlock()
		if streamCount == 0 {
			freed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !freed {
		t.Fatal("stream capacity was not freed after client cancellation")
	}
}
