package server

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeTicketsAreBoundedToFixedAssets(t *testing.T) {
	s := testServer(t, nil, "")
	s.opt.ProbeDir = t.TempDir()
	os.WriteFile(filepath.Join(s.opt.ProbeDir, "baseline-360.mp4"), []byte("synthetic-fixture"), 0600)
	token := pair(t, s, "probe-device")
	if w := call(s, "GET", "/v1/probes", "", "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := call(s, "GET", "/v1/probes", "", "probe-device", token, "")
	var manifest struct{ Probes []probeAsset }
	if json.Unmarshal(w.Body.Bytes(), &manifest) != nil || len(manifest.Probes) != 1 {
		t.Fatal(w.Body)
	}
	path := manifest.Probes[0].URL
	if w = call(s, "GET", path, "", "", "", ""); w.Code != 200 || w.Body.String() != "synthetic-fixture" {
		t.Fatal(w.Code, w.Body)
	}
	if w = call(s, "GET", strings.Replace(path, "h264-baseline-360", "aac", 1), "", "", "", ""); w.Code != 403 {
		t.Fatal("ticket scope", w.Code)
	}
	u, _ := url.Parse(path)
	q := u.Query()
	q.Set("expires", "1")
	q.Set("ticket", s.probeSignature("h264-baseline-360", "1"))
	u.RawQuery = q.Encode()
	if w = call(s, "GET", u.String(), "", "", "", ""); w.Code != 403 {
		t.Fatal("expired ticket", w.Code)
	}
	os.Remove(filepath.Join(s.opt.ProbeDir, "baseline-360.mp4"))
	os.Symlink("/etc/passwd", filepath.Join(s.opt.ProbeDir, "baseline-360.mp4"))
	if w = call(s, "GET", path, "", "", "", ""); w.Code != 404 {
		t.Fatal("symlink served", w.Code)
	}
}
func TestDuplicateProbeEvidenceRejected(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "probe-device")
	body := `{"capabilitiesVersion":1,"deviceId":"probe-device","probes":[{"id":"aac","status":"PASS","prepareMs":1},{"id":"aac","status":"FAIL","prepareMs":1}]}`
	if w := call(s, "PUT", "/v1/device/capabilities", body, "probe-device", token, ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestChangedPlatformInvalidatesPreviouslyMeasuredCapabilities(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "probe-device")
	body := `{"capabilitiesVersion":1,"deviceId":"probe-device","probes":[{"id":"aac","status":"PASS","prepareMs":10}]}`
	if w := call(s, "PUT", "/v1/device/capabilities", body, "probe-device", token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	registration := `{"clientVersion":"fixture","protocolVersion":1,"installationId":"probe-device","pairingCode":"123456","platform":{"androidApi":30,"release":"changed"}}`
	w := call(s, "POST", "/v1/devices/register", registration, "", "", "")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var credentials struct{ DeviceToken string }
	json.Unmarshal(w.Body.Bytes(), &credentials)
	w = call(s, "GET", "/v1/device", "", "probe-device", credentials.DeviceToken, "")
	var device struct{ Capabilities struct{ Probes []any } }
	json.Unmarshal(w.Body.Bytes(), &device)
	if len(device.Capabilities.Probes) != 0 {
		t.Fatal("stale evidence survived firmware change", w.Body)
	}
}

func TestHLSProbeScopesItsSegmentAndRejectsStaleResults(t *testing.T) {
	s := testServer(t, nil, "")
	s.opt.ProbeDir = t.TempDir()
	os.WriteFile(filepath.Join(s.opt.ProbeDir, "baseline.ts"), []byte("transport fixture"), 0600)
	token := pair(t, s, "probe-device")
	w := call(s, "GET", "/v1/probes?suite=2", "", "probe-device", token, "")
	var manifest struct {
		SuiteVersion int
		CacheKey     string
		Probes       []probeAsset
	}
	if json.Unmarshal(w.Body.Bytes(), &manifest) != nil || manifest.SuiteVersion != 2 || len(manifest.CacheKey) != 64 {
		t.Fatal(w.Body)
	}
	for _, probe := range manifest.Probes {
		if probe.Kind != "hls" {
			continue
		}
		response := call(s, "GET", probe.URL, "", "", "", "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), "#EXT-X-ENDLIST") {
			t.Fatal(response.Body)
		}
		for _, line := range strings.Split(response.Body.String(), "\n") {
			if strings.HasPrefix(line, "/v1/probes/") {
				segment := call(s, "GET", line, "", "", "", "")
				if segment.Code != 200 || segment.Body.String() != "transport fixture" {
					t.Fatal(segment.Body)
				}
			}
		}
	}
	body := `{"capabilitiesVersion":1,"deviceId":"probe-device","suiteVersion":2,"cacheKey":"wrong","probes":[]}`
	if response := call(s, "PUT", "/v1/device/capabilities", body, "probe-device", token, ""); response.Code != 409 {
		t.Fatal(response.Code)
	}
}

func TestHTTPProgressiveAndAACADTSProbes(t *testing.T) {
	s := testServer(t, nil, "")
	s.opt.ProbeDir = t.TempDir()
	mp4Content := []byte("mp4-progressive-synthetic-fixture")
	adtsContent := []byte("adts-audio-synthetic-fixture")
	os.WriteFile(filepath.Join(s.opt.ProbeDir, "baseline-360.mp4"), mp4Content, 0600)
	os.WriteFile(filepath.Join(s.opt.ProbeDir, "aac.adts"), adtsContent, 0600)

	token := pair(t, s, "transport-probe-device")

	// 1. Suite 1: both http-progressive and aac-adts must be absent (backward compatibility)
	wSuite1 := call(s, "GET", "/v1/probes", "", "transport-probe-device", token, "")
	if wSuite1.Code != 200 {
		t.Fatalf("suite 1 GET /v1/probes failed: %d", wSuite1.Code)
	}
	var manifest1 struct {
		SuiteVersion int
		Probes       []probeAsset
	}
	if err := json.Unmarshal(wSuite1.Body.Bytes(), &manifest1); err != nil {
		t.Fatal(err)
	}
	if manifest1.SuiteVersion != 1 {
		t.Fatalf("expected suiteVersion 1, got %d", manifest1.SuiteVersion)
	}
	for _, p := range manifest1.Probes {
		if p.ID == "http-progressive" {
			t.Fatal("http-progressive must NOT be present in suite 1")
		}
		if p.ID == "aac-adts" {
			t.Fatal("aac-adts must NOT be present in suite 1")
		}
	}

	// 2. Suite 2: both http-progressive and aac-adts must be present with valid signed tickets
	wSuite2 := call(s, "GET", "/v1/probes?suite=2", "", "transport-probe-device", token, "")
	if wSuite2.Code != 200 {
		t.Fatalf("suite 2 GET /v1/probes failed: %d", wSuite2.Code)
	}
	var manifest2 struct {
		SuiteVersion int
		CacheKey     string
		Probes       []probeAsset
	}
	if err := json.Unmarshal(wSuite2.Body.Bytes(), &manifest2); err != nil {
		t.Fatal(err)
	}
	if manifest2.SuiteVersion != 2 {
		t.Fatalf("expected suiteVersion 2, got %d", manifest2.SuiteVersion)
	}

	var httpProg, aacAdts *probeAsset
	for i := range manifest2.Probes {
		p := &manifest2.Probes[i]
		if p.ID == "http-progressive" {
			httpProg = p
		}
		if p.ID == "aac-adts" {
			aacAdts = p
		}
	}
	if httpProg == nil {
		t.Fatal("http-progressive probe missing from suite 2 manifest")
	}
	if !httpProg.Video {
		t.Fatalf("expected Video true for http-progressive, got %v", httpProg.Video)
	}
	if httpProg.Kind != "playback" {
		t.Fatalf("expected Kind 'playback' for http-progressive, got %s", httpProg.Kind)
	}
	if aacAdts == nil {
		t.Fatal("aac-adts probe missing from suite 2 manifest")
	}
	if aacAdts.Video {
		t.Fatalf("expected Video false for aac-adts, got %v", aacAdts.Video)
	}
	if aacAdts.Kind != "playback" {
		t.Fatalf("expected Kind 'playback' for aac-adts, got %s", aacAdts.Kind)
	}

	// 3. Ticketed stream delivery for http-progressive
	respProg := call(s, "GET", httpProg.URL, "", "", "", "")
	if respProg.Code != 200 {
		t.Fatalf("expected 200 for http-progressive stream, got %d: %s", respProg.Code, respProg.Body)
	}
	if !strings.HasPrefix(respProg.Header().Get("Content-Type"), "video/mp4") {
		t.Fatalf("expected Content-Type video/mp4, got %s", respProg.Header().Get("Content-Type"))
	}
	if respProg.Body.String() != string(mp4Content) {
		t.Fatalf("http-progressive body mismatch: expected %q, got %q", string(mp4Content), respProg.Body.String())
	}

	// 4. Ticketed stream delivery for aac-adts: Content-Type must be audio/aac for legacy MediaPlayer
	respAdts := call(s, "GET", aacAdts.URL, "", "", "", "")
	if respAdts.Code != 200 {
		t.Fatalf("expected 200 for aac-adts stream, got %d: %s", respAdts.Code, respAdts.Body)
	}
	if respAdts.Header().Get("Content-Type") != "audio/aac" {
		t.Fatalf("expected Content-Type audio/aac, got %s", respAdts.Header().Get("Content-Type"))
	}
	if respAdts.Body.String() != string(adtsContent) {
		t.Fatalf("aac-adts body mismatch: expected %q, got %q", string(adtsContent), respAdts.Body.String())
	}

	// 5. Ticket scope protection: ticket for http-progressive cannot fetch aac-adts
	swappedPath := strings.Replace(httpProg.URL, "http-progressive", "aac-adts", 1)
	if w := call(s, "GET", swappedPath, "", "", "", ""); w.Code != 403 {
		t.Fatalf("expected 403 when swapping ticketed probe ID, got %d", w.Code)
	}

	// 6. Expired and tampered tickets are rejected
	u, _ := url.Parse(httpProg.URL)
	q := u.Query()
	q.Set("expires", "1")
	q.Set("ticket", s.probeSignature("http-progressive", "1"))
	u.RawQuery = q.Encode()
	if w := call(s, "GET", u.String(), "", "", "", ""); w.Code != 403 {
		t.Fatalf("expected 403 for expired ticket, got %d", w.Code)
	}
	q.Set("ticket", "tampered-ticket-hex")
	u.RawQuery = q.Encode()
	if w := call(s, "GET", u.String(), "", "", "", ""); w.Code != 403 {
		t.Fatalf("expected 403 for tampered ticket, got %d", w.Code)
	}
}
