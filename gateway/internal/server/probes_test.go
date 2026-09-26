package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
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

func TestProbeFailureDetailBoundedAndExported(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "detail-probe-device")

	// 1. Valid bounded failure detail (<= 120 chars) is accepted
	validBody := `{"capabilitiesVersion":1,"deviceId":"detail-probe-device","probes":[{"id":"aac-adts","status":"UNKNOWN","prepareMs":0,"detail":"what=1,extra=-1004@prepare http=200,audio/aac"}]}`
	if w := call(s, "PUT", "/v1/device/capabilities", validBody, "detail-probe-device", token, ""); w.Code != 200 {
		t.Fatalf("expected 200 for valid probe detail, got %d: %s", w.Code, w.Body)
	}

	// 2. Detail is exported in /v1/diagnostics
	diag := call(s, "GET", "/v1/diagnostics", "", "detail-probe-device", token, "")
	if diag.Code != 200 {
		t.Fatalf("expected 200 from diagnostics, got %d", diag.Code)
	}
	var report struct {
		Probes []domain.Probe `json:"probes"`
	}
	if err := json.Unmarshal(diag.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Probes) != 1 || report.Probes[0].Detail != "what=1,extra=-1004@prepare http=200,audio/aac" {
		t.Fatalf("unexpected probe in diagnostics: %+v", report.Probes)
	}

	// 3. Excessively long detail (> 120 chars) is rejected with 400
	oversizedDetail := strings.Repeat("x", 121)
	invalidBody := `{"capabilitiesVersion":1,"deviceId":"detail-probe-device","probes":[{"id":"aac-adts","status":"UNKNOWN","detail":"` + oversizedDetail + `"}]}`
	if w := call(s, "PUT", "/v1/device/capabilities", invalidBody, "detail-probe-device", token, ""); w.Code != 400 {
		t.Fatalf("expected 400 for oversized probe detail, got %d", w.Code)
	}
}

func TestProbeFailureDetailDoesNotEchoUntrustedText(t *testing.T) {
	if got := safeProbeDetail("what=1,extra=-1004@prepare http=200,token/secret"); got != "what=1,extra=-1004@prepare http=200" {
		t.Fatalf("unexpected MIME-like detail: %q", got)
	}
	s := testServer(t, nil, "")
	token := pair(t, s, "private-probe-device")
	body := `{"capabilitiesVersion":1,"deviceId":"private-probe-device","probes":[{"id":"aac-adts","status":"UNKNOWN","detail":"https://example.invalid/?token=private-value"}]}`
	if w := call(s, "PUT", "/v1/device/capabilities", body, "private-probe-device", token, ""); w.Code != 200 {
		t.Fatalf("expected 200 with sanitized detail, got %d: %s", w.Code, w.Body)
	}
	diag := call(s, "GET", "/v1/diagnostics", "", "private-probe-device", token, "")
	if diag.Code != 200 {
		t.Fatalf("expected 200 from diagnostics, got %d", diag.Code)
	}
	if strings.Contains(diag.Body.String(), "private-value") || strings.Contains(diag.Body.String(), "example.invalid") {
		t.Fatal("diagnostics echoed untrusted probe text")
	}
}

func TestAudioOnlyMPEGTSProbeIsExtendedAndTyped(t *testing.T) {
	s := testServer(t, nil, "")
	s.opt.ProbeDir = t.TempDir()
	fixture := []byte("audio-only-mpegts-synthetic-fixture")
	if err := os.WriteFile(filepath.Join(s.opt.ProbeDir, "mpegts-aac.ts"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "mpegts-aac-probe-device")

	legacy := call(s, "GET", "/v1/probes", "", "mpegts-aac-probe-device", token, "")
	if legacy.Code != 200 {
		t.Fatalf("expected 200 for suite 1 manifest, got %d", legacy.Code)
	}
	var legacyManifest struct {
		SuiteVersion int
		Probes       []probeAsset
	}
	if err := json.Unmarshal(legacy.Body.Bytes(), &legacyManifest); err != nil {
		t.Fatal(err)
	}
	if legacyManifest.SuiteVersion != 1 {
		t.Fatalf("expected suiteVersion 1, got %d", legacyManifest.SuiteVersion)
	}
	for _, probe := range legacyManifest.Probes {
		if probe.ID == "mpegts-aac" {
			t.Fatal("mpegts-aac playback probe must be absent from suite 1")
		}
	}

	extended := call(s, "GET", "/v1/probes?suite=2", "", "mpegts-aac-probe-device", token, "")
	if extended.Code != 200 {
		t.Fatalf("expected 200 for suite 2 manifest, got %d", extended.Code)
	}
	var extendedManifest struct {
		SuiteVersion int
		Probes       []probeAsset
	}
	if err := json.Unmarshal(extended.Body.Bytes(), &extendedManifest); err != nil {
		t.Fatal(err)
	}
	if extendedManifest.SuiteVersion != 2 {
		t.Fatalf("expected suiteVersion 2, got %d", extendedManifest.SuiteVersion)
	}
	var audioOnly *probeAsset
	for i := range extendedManifest.Probes {
		if extendedManifest.Probes[i].ID == "mpegts-aac" {
			audioOnly = &extendedManifest.Probes[i]
			break
		}
	}
	if audioOnly == nil {
		t.Fatal("mpegts-aac probe missing from suite 2 manifest")
	}
	if audioOnly.Video || audioOnly.Kind != "playback" {
		t.Fatalf("expected audio-only playback probe, got %+v", *audioOnly)
	}

	response := call(s, "GET", audioOnly.URL, "", "", "", "")
	if response.Code != 200 {
		t.Fatalf("expected 200 for mpegts-aac stream, got %d: %s", response.Code, response.Body)
	}
	if got := response.Header().Get("Content-Type"); got != "video/mp2t" {
		t.Fatalf("expected Content-Type video/mp2t, got %s", got)
	}
	if response.Body.String() != string(fixture) {
		t.Fatalf("mpegts-aac body mismatch: expected %q, got %q", string(fixture), response.Body.String())
	}
}

func TestPacedMPEGTSProbeUsesSignedKnownLengthResponse(t *testing.T) {
	s := testServer(t, nil, "")
	s.opt.ProbeDir = t.TempDir()
	fixture := bytes.Repeat([]byte("paced-mpegts-h264-aac-fixture-"), 15000)
	if err := os.WriteFile(filepath.Join(s.opt.ProbeDir, "baseline.ts"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "paced-mpegts-probe-device")

	legacy := call(s, http.MethodGet, "/v1/probes", "", "paced-mpegts-probe-device", token, "")
	if legacy.Code != http.StatusOK {
		t.Fatalf("expected suite 1 manifest, got %d", legacy.Code)
	}
	var legacyManifest struct{ Probes []probeAsset }
	if err := json.Unmarshal(legacy.Body.Bytes(), &legacyManifest); err != nil {
		t.Fatal(err)
	}
	for _, asset := range legacyManifest.Probes {
		if asset.ID == "mpegts-h264-aac-paced" {
			t.Fatal("paced MPEG-TS playback probe must be absent from suite 1")
		}
	}

	manifest := call(s, http.MethodGet, "/v1/probes?suite=2", "", "paced-mpegts-probe-device", token, "")
	if manifest.Code != http.StatusOK {
		t.Fatalf("expected suite 2 manifest, got %d", manifest.Code)
	}
	var listing struct {
		SuiteVersion int
		Probes       []probeAsset
	}
	if err := json.Unmarshal(manifest.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	var paced *probeAsset
	for i := range listing.Probes {
		if listing.Probes[i].ID == "mpegts-h264-aac-paced" {
			paced = &listing.Probes[i]
			break
		}
	}
	if listing.SuiteVersion != 2 || paced == nil || !paced.Video || paced.Kind != "playback" {
		t.Fatalf("suite 2 must expose the paced H.264/AAC video probe: suite=%d asset=%+v", listing.SuiteVersion, paced)
	}

	server := httptest.NewServer(s)
	defer server.Close()
	started := time.Now()
	response, err := http.Get(server.URL + paced.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "video/mp2t" {
		t.Fatalf("unexpected paced MPEG-TS response: status=%d mime=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Content-Length") != strconv.Itoa(len(fixture)) || response.ContentLength != int64(len(fixture)) {
		t.Fatalf("expected exact Content-Length %d, header=%q parsed=%d", len(fixture), response.Header.Get("Content-Length"), response.ContentLength)
	}
	if len(response.TransferEncoding) != 0 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("expected non-chunked no-store response, transfer=%v cache=%q", response.TransferEncoding, response.Header.Get("Cache-Control"))
	}
	if !bytes.Equal(body, fixture) {
		t.Fatal("paced MPEG-TS response bytes differ from the fixed fixture")
	}
	if elapsed < pacedMPEGTSProbeDuration-150*time.Millisecond || elapsed > pacedMPEGTSProbeDuration+2*time.Second {
		t.Fatalf("paced response took %s, expected approximately %s and below the bounded window", elapsed, pacedMPEGTSProbeDuration)
	}

	badURL, err := url.Parse(paced.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := badURL.Query()
	query.Set("ticket", "tampered")
	badURL.RawQuery = query.Encode()
	if rejected := call(s, http.MethodGet, badURL.String(), "", "", "", ""); rejected.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a tampered paced-probe ticket, got %d", rejected.Code)
	}
}

func TestPacedMPEGTSProbeStopsAfterRequestCancellation(t *testing.T) {
	fixture := bytes.Repeat([]byte("paced-mpegts-cancel-fixture-"), 2400)
	path := filepath.Join(t.TempDir(), "baseline.ts")
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &cancelOnProbeWrite{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	started := time.Now()
	streamPacedProbe(ctx, recorder, f, "video/mp2t", 400*time.Millisecond)
	elapsed := time.Since(started)
	if recorder.writes != 1 || recorder.Body.Len() != pacedProbeChunkSize {
		t.Fatalf("expected one first chunk before cancellation, writes=%d bytes=%d", recorder.writes, recorder.Body.Len())
	}
	if recorder.Header().Get("Content-Length") != strconv.Itoa(len(fixture)) || recorder.Header().Get("Content-Type") != "video/mp2t" {
		t.Fatalf("paced cancellation lost response framing: headers=%v", recorder.Header())
	}
	if !bytes.Equal(recorder.Body.Bytes(), fixture[:pacedProbeChunkSize]) {
		t.Fatal("paced cancellation wrote bytes beyond the first chunk")
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("paced stream did not stop promptly after cancellation: %s", elapsed)
	}
}

type flushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushCount int
}

func (w *flushCountingRecorder) Flush() {
	w.flushCount++
	w.ResponseRecorder.Flush()
}

func TestChunkedMPEGTSAACProbeStreamsWithScopedTicket(t *testing.T) {
	s := testServer(t, nil, "")
	s.opt.ProbeDir = t.TempDir()
	fixture := bytes.Repeat([]byte("mpegts-aac-chunk-"), 4000)
	if err := os.WriteFile(filepath.Join(s.opt.ProbeDir, "mpegts-aac.ts"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "mpegts-aac-chunked-device")

	legacy := call(s, "GET", "/v1/probes", "", "mpegts-aac-chunked-device", token, "")
	if legacy.Code != http.StatusOK {
		t.Fatalf("expected 200 for suite 1 manifest, got %d", legacy.Code)
	}
	var legacyManifest struct {
		Probes []probeAsset
	}
	if err := json.Unmarshal(legacy.Body.Bytes(), &legacyManifest); err != nil {
		t.Fatal(err)
	}
	for _, probe := range legacyManifest.Probes {
		if probe.ID == "mpegts-aac-chunked" {
			t.Fatal("chunked playback probe must be absent from suite 1")
		}
	}

	extended := call(s, "GET", "/v1/probes?suite=2", "", "mpegts-aac-chunked-device", token, "")
	if extended.Code != http.StatusOK {
		t.Fatalf("expected 200 for suite 2 manifest, got %d", extended.Code)
	}
	var extendedManifest struct {
		Probes []probeAsset
	}
	if err := json.Unmarshal(extended.Body.Bytes(), &extendedManifest); err != nil {
		t.Fatal(err)
	}
	var chunked *probeAsset
	for i := range extendedManifest.Probes {
		if extendedManifest.Probes[i].ID == "mpegts-aac-chunked" {
			chunked = &extendedManifest.Probes[i]
			break
		}
	}
	if chunked == nil {
		t.Fatal("chunked MPEG-TS AAC probe missing from suite 2 manifest")
	}
	if chunked.Video || chunked.Kind != "playback" {
		t.Fatalf("expected audio-only playback probe, got %+v", *chunked)
	}

	request := httptest.NewRequest(http.MethodGet, chunked.URL, nil)
	recorder := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 for chunked probe, got %d: %s", recorder.Code, recorder.Body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "video/mp2t" {
		t.Fatalf("expected Content-Type video/mp2t, got %s", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "" {
		t.Fatalf("expected no Content-Length, got %s", got)
	}
	if recorder.flushCount < 2 {
		t.Fatalf("expected multiple stream flushes, got %d", recorder.flushCount)
	}
	if !bytes.Equal(recorder.Body.Bytes(), fixture) {
		t.Fatal("chunked probe body did not match fixture")
	}

	server := httptest.NewServer(s)
	defer server.Close()
	response, err := http.Get(server.URL + chunked.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 over HTTP, got %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "video/mp2t" {
		t.Fatalf("expected HTTP Content-Type video/mp2t, got %s", got)
	}
	if response.ProtoMajor != 1 {
		t.Fatalf("expected HTTP/1.x chunked transfer test, got %s", response.Proto)
	}
	if got := response.Header.Get("Content-Length"); got != "" || response.ContentLength >= 0 {
		t.Fatalf("expected unknown Content-Length, header=%q length=%d", got, response.ContentLength)
	}
	if len(response.TransferEncoding) != 1 || response.TransferEncoding[0] != "chunked" {
		t.Fatalf("expected HTTP chunked transfer encoding, got %v", response.TransferEncoding)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, fixture) {
		t.Fatal("HTTP chunked probe body did not match fixture")
	}

	wrongAssetURL := strings.Replace(chunked.URL, "mpegts-aac-chunked", "mpegts-aac", 1)
	for _, invalidURL := range []string{
		wrongAssetURL,
		strings.Split(chunked.URL, "&ticket=")[0],
	} {
		invalid, err := http.Get(server.URL + invalidURL)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, invalid.Body)
		invalid.Body.Close()
		if invalid.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for invalid or out-of-scope ticket, got %d", invalid.StatusCode)
		}
	}
}

func TestChunkedMP3ProbeUsesAudioMIMEAndUnknownLength(t *testing.T) {
	s := testServer(t, nil, "")
	s.opt.ProbeDir = t.TempDir()
	fixture := bytes.Repeat([]byte("synthetic-mp3-chunk"), 4000)
	if err := os.WriteFile(filepath.Join(s.opt.ProbeDir, "mp3.mp3"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "mp3-chunked-device")
	manifest := call(s, http.MethodGet, "/v1/probes?suite=2", "", "mp3-chunked-device", token, "")
	if manifest.Code != http.StatusOK {
		t.Fatalf("expected probe manifest, got %d", manifest.Code)
	}
	var listing struct{ Probes []probeAsset }
	if err := json.Unmarshal(manifest.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	var path string
	for _, asset := range listing.Probes {
		if asset.ID == "mp3-chunked" {
			if asset.Video || asset.Kind != "playback" {
				t.Fatalf("expected audio playback probe, got %+v", asset)
			}
			path = asset.URL
		}
	}
	if path == "" {
		t.Fatal("mp3-chunked probe missing")
	}
	server := httptest.NewServer(s)
	defer server.Close()
	response, err := http.Get(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "audio/mpeg" || response.ContentLength >= 0 {
		t.Fatalf("unexpected chunked MP3 response: status=%d mime=%s length=%d", response.StatusCode, response.Header.Get("Content-Type"), response.ContentLength)
	}
	if len(response.TransferEncoding) != 1 || response.TransferEncoding[0] != "chunked" {
		t.Fatalf("expected HTTP chunked encoding, got %v", response.TransferEncoding)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(body, fixture) {
		t.Fatalf("chunked MP3 body mismatch: %v", err)
	}
}

func TestChunkedFMP4ProbeUsesSameSignedAssetAndHTTPFraming(t *testing.T) {
	s := testServer(t, nil, "")
	s.opt.ProbeDir = t.TempDir()
	fixture := bytes.Repeat([]byte("fragmented-mp4-probe-fixture"), 1200)
	if err := os.WriteFile(filepath.Join(s.opt.ProbeDir, "fragmented.mp4"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "fmp4-chunked-device")

	legacy := call(s, http.MethodGet, "/v1/probes", "", "fmp4-chunked-device", token, "")
	if legacy.Code != http.StatusOK {
		t.Fatalf("expected suite 1 manifest, got %d", legacy.Code)
	}
	var legacyManifest struct{ Probes []probeAsset }
	if err := json.Unmarshal(legacy.Body.Bytes(), &legacyManifest); err != nil {
		t.Fatal(err)
	}
	for _, asset := range legacyManifest.Probes {
		if asset.ID == "http-fmp4-chunked" || asset.ID == "http-fmp4-seek" {
			t.Fatal("extended fMP4 probes must be absent from suite 1")
		}
	}

	manifest := call(s, http.MethodGet, "/v1/probes?suite=2", "", "fmp4-chunked-device", token, "")
	if manifest.Code != http.StatusOK {
		t.Fatalf("expected suite 2 manifest, got %d", manifest.Code)
	}
	var listing struct {
		SuiteVersion int
		Probes       []probeAsset
	}
	if err := json.Unmarshal(manifest.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	var knownLength, chunked, seek *probeAsset
	for i := range listing.Probes {
		switch listing.Probes[i].ID {
		case "http-fmp4":
			knownLength = &listing.Probes[i]
		case "http-fmp4-chunked":
			chunked = &listing.Probes[i]
		case "http-fmp4-seek":
			seek = &listing.Probes[i]
		}
	}
	if listing.SuiteVersion != 2 || knownLength == nil || chunked == nil || seek == nil || seek.Kind != "seek-midstream" {
		t.Fatalf("suite 2 must contain fMP4 transport and midstream seek probes: %+v", listing)
	}
	var knownDefinition, chunkedDefinition probeAsset
	for _, asset := range probeAssets {
		switch asset.ID {
		case "http-fmp4":
			knownDefinition = asset
		case "http-fmp4-chunked":
			chunkedDefinition = asset
		}
	}
	if knownDefinition.File != "fragmented.mp4" || chunkedDefinition.File != knownDefinition.File || !knownLength.Video || !chunked.Video || chunked.Kind != "playback" {
		t.Fatalf("fMP4 probes must use the same video source and run as playback: known=%+v chunked=%+v", *knownLength, *chunked)
	}

	server := httptest.NewServer(s)
	defer server.Close()
	get := func(path string) *http.Response {
		t.Helper()
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	knownResponse := get(knownLength.URL)
	defer knownResponse.Body.Close()
	knownBytes, err := io.ReadAll(knownResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if knownResponse.StatusCode != http.StatusOK || knownResponse.ProtoMajor != 1 || knownResponse.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("unexpected known-length fMP4 response: status=%d proto=%s mime=%q", knownResponse.StatusCode, knownResponse.Proto, knownResponse.Header.Get("Content-Type"))
	}
	if knownResponse.ContentLength != int64(len(fixture)) || knownResponse.Header.Get("Content-Length") == "" || len(knownResponse.TransferEncoding) != 0 {
		t.Fatalf("expected known Content-Length, got header=%q parsed=%d transfer=%v", knownResponse.Header.Get("Content-Length"), knownResponse.ContentLength, knownResponse.TransferEncoding)
	}
	if !bytes.Equal(knownBytes, fixture) {
		t.Fatal("known-length fMP4 response did not match fragmented.mp4 fixture")
	}

	chunkedResponse := get(chunked.URL)
	defer chunkedResponse.Body.Close()
	chunkedBytes, err := io.ReadAll(chunkedResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if chunkedResponse.StatusCode != http.StatusOK || chunkedResponse.ProtoMajor != 1 || chunkedResponse.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("unexpected chunked fMP4 response: status=%d proto=%s mime=%q", chunkedResponse.StatusCode, chunkedResponse.Proto, chunkedResponse.Header.Get("Content-Type"))
	}
	if chunkedResponse.ContentLength >= 0 || chunkedResponse.Header.Get("Content-Length") != "" || len(chunkedResponse.TransferEncoding) != 1 || chunkedResponse.TransferEncoding[0] != "chunked" {
		t.Fatalf("expected HTTP/1.1 chunked response with unknown length, header=%q parsed=%d transfer=%v", chunkedResponse.Header.Get("Content-Length"), chunkedResponse.ContentLength, chunkedResponse.TransferEncoding)
	}
	if !bytes.Equal(chunkedBytes, knownBytes) {
		t.Fatal("chunked fMP4 response bytes differ from http-fmp4")
	}

	wrongAssetURL := strings.Replace(chunked.URL, "http-fmp4-chunked", "http-fmp4", 1)
	invalid, err := http.Get(server.URL + wrongAssetURL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, invalid.Body)
	invalid.Body.Close()
	if invalid.StatusCode != http.StatusForbidden {
		t.Fatalf("expected a ticket scoped to http-fmp4-chunked, got %d", invalid.StatusCode)
	}
}

type cancelOnProbeWrite struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	writes int
}

func (w *cancelOnProbeWrite) Write(p []byte) (int, error) {
	w.writes++
	n, err := w.ResponseRecorder.Write(p)
	w.cancel()
	return n, err
}

func TestChunkedProbeStopsReadingAfterRequestCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fragmented.mp4")
	fixture := bytes.Repeat([]byte("fragmented-mp4-probe-fixture"), 1200)
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &cancelOnProbeWrite{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	streamChunkedProbe(ctx, recorder, f, "video/mp4")
	if recorder.writes != 1 || recorder.Body.Len() != 16<<10 {
		t.Fatalf("expected exactly the first 16 KiB chunk before cancellation, writes=%d bytes=%d", recorder.writes, recorder.Body.Len())
	}
	if !bytes.Equal(recorder.Body.Bytes(), fixture[:16<<10]) {
		t.Fatal("canceled stream wrote bytes outside the first chunk")
	}
}
