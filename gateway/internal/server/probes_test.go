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
