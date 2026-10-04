package server

import (
	"encoding/json"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
)

func playbackValidationFixture(now time.Time, probes ...domain.Probe) domain.Device {
	device := domain.Device{
		ID: "playback-validation-device",
		Registration: domain.Registration{
			ClientVersion: "test",
			Platform:      domain.Platform{AndroidAPI: 13},
		},
	}
	device.Capabilities = domain.Capabilities{
		Version:      1,
		DeviceID:     device.ID,
		SuiteVersion: devices.ProbeSuiteVersion,
		CacheKey:     devices.ProbeCacheKey(device),
		Probes:       probes,
	}
	return device
}

func playbackProbe(id, status string, testedAt int64) domain.Probe {
	return domain.Probe{ID: id, Status: status, TestedAt: testedAt}
}

func TestPlaybackValidationFreshOutcomesRemainCurrent(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	available := map[string]bool{"aac": true}
	for _, status := range []string{"PASS", "UNKNOWN", "FAIL"} {
		t.Run(status, func(t *testing.T) {
			device := playbackValidationFixture(now, playbackProbe("aac", status, now.Unix()))
			got := playbackValidationFor(device, now, available)
			if got.Status != "CURRENT" {
				t.Fatalf("fresh %s evidence: got %+v", status, got)
			}
		})
	}
}

func TestPlaybackValidationSevenDayBoundary(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	available := map[string]bool{"aac": true}
	cases := []struct {
		name     string
		testedAt int64
		status   string
		reason   string
	}{
		{name: "one second inside", testedAt: now.Add(-playbackValidationFreshness).Unix() + 1, status: "CURRENT"},
		{name: "exactly seven days", testedAt: now.Add(-playbackValidationFreshness).Unix(), status: "REFRESH_REQUIRED", reason: "evidence_expired"},
		{name: "beyond seven days", testedAt: now.Add(-playbackValidationFreshness).Unix() - 1, status: "REFRESH_REQUIRED", reason: "evidence_expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			device := playbackValidationFixture(now, playbackProbe("aac", "PASS", tc.testedAt))
			got := playbackValidationFor(device, now, available)
			if got.Status != tc.status || got.Reason != tc.reason {
				t.Fatalf("got %+v, want status=%s reason=%s", got, tc.status, tc.reason)
			}
		})
	}
}

func TestPlaybackValidationMissingAndInvalidEvidence(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	available := map[string]bool{"aac": true}
	cases := []struct {
		name   string
		probes []domain.Probe
		reason string
	}{
		{name: "empty", reason: "evidence_missing"},
		{name: "missing timestamp", probes: []domain.Probe{playbackProbe("aac", "UNKNOWN", 0)}, reason: "evidence_invalid"},
		{name: "future timestamp", probes: []domain.Probe{playbackProbe("aac", "PASS", now.Unix()+301)}, reason: "evidence_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			device := playbackValidationFixture(now, tc.probes...)
			got := playbackValidationFor(device, now, available)
			if got.Status != "REFRESH_REQUIRED" || got.Reason != tc.reason {
				t.Fatalf("got %+v, want refresh reason %s", got, tc.reason)
			}
		})
	}

	device := playbackValidationFixture(now, playbackProbe("aac", "PASS", now.Unix()+300))
	if got := playbackValidationFor(device, now, available); got.Status != "CURRENT" {
		t.Fatalf("timestamp at the future tolerance boundary should be current: %+v", got)
	}
}

func TestPlaybackValidationContextBinding(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	cases := []struct {
		name   string
		change func(*domain.Device)
		reason string
	}{
		{name: "missing suite", change: func(d *domain.Device) { d.Capabilities.SuiteVersion = 0 }, reason: "suite_missing"},
		{name: "mismatched suite", change: func(d *domain.Device) { d.Capabilities.SuiteVersion++ }, reason: "suite_mismatch"},
		{name: "missing cache", change: func(d *domain.Device) { d.Capabilities.CacheKey = "" }, reason: "cache_missing"},
		{name: "mismatched cache", change: func(d *domain.Device) { d.Capabilities.CacheKey = "other" }, reason: "cache_mismatch"},
		{name: "missing device binding", change: func(d *domain.Device) { d.Capabilities.DeviceID = "" }, reason: "device_binding_missing"},
		{name: "mismatched device binding", change: func(d *domain.Device) { d.Capabilities.DeviceID = "another-device" }, reason: "device_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			device := playbackValidationFixture(now, playbackProbe("aac", "PASS", now.Unix()))
			tc.change(&device)
			got := playbackValidationFor(device, now, map[string]bool{"aac": true})
			if got.Status != "REFRESH_REQUIRED" || got.Reason != tc.reason {
				t.Fatalf("got %+v, want refresh reason %s", got, tc.reason)
			}
		})
	}

	device := playbackValidationFixture(now, playbackProbe("aac", "PASS", now.Unix()))
	device.ID = ""
	if got := playbackValidationFor(device, now, map[string]bool{"aac": true}); got.Status != "UNAVAILABLE" {
		t.Fatalf("missing device context should be unavailable, got %+v", got)
	}
}

func TestPlaybackValidationAllowsFreshPartialSuiteAndChecksEachRecordedProbe(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	partial := playbackValidationFixture(now, playbackProbe("audio-track-pcm-stream", "UNKNOWN", now.Unix()))
	if got := playbackValidationFor(partial, now, map[string]bool{"audio-track-pcm-stream": true}); got.Status != "CURRENT" {
		t.Fatalf("fresh partial suite should remain current: %+v", got)
	}

	mixed := playbackValidationFixture(
		now,
		playbackProbe("aac", "PASS", now.Unix()),
		playbackProbe("http-fmp4", "UNKNOWN", now.Add(-playbackValidationFreshness).Unix()),
	)
	got := playbackValidationFor(mixed, now, map[string]bool{"aac": true, "http-fmp4": true})
	if got.Status != "REFRESH_REQUIRED" || got.Reason != "evidence_expired" {
		t.Fatalf("fresh evidence for another probe must not hide expiry: %+v", got)
	}

	unrelated := playbackValidationFixture(
		now,
		playbackProbe("aac", "PASS", now.Unix()),
		playbackProbe("multicast-loopback", "FAIL", now.Add(-10*24*time.Hour).Unix()),
	)
	got = playbackValidationFor(unrelated, now, map[string]bool{"aac": true})
	if got.Status != "CURRENT" {
		t.Fatalf("unrelated local diagnostic must not affect playback freshness: %+v", got)
	}
}

func TestPlaybackValidationDuplicateEvidenceUsesLatestCredibleTimestamp(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	cases := []struct {
		name   string
		probes []domain.Probe
	}{
		{
			name: "new result supersedes stale duplicate",
			probes: []domain.Probe{
				playbackProbe("aac", "FAIL", now.Add(-playbackValidationFreshness).Unix()),
				playbackProbe("aac", "UNKNOWN", now.Unix()),
			},
		},
		{
			name: "valid result supersedes future duplicate",
			probes: []domain.Probe{
				playbackProbe("aac", "FAIL", now.Unix()+301),
				playbackProbe("aac", "UNKNOWN", now.Unix()),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			device := playbackValidationFixture(now, tc.probes...)
			got := playbackValidationFor(device, now, map[string]bool{"aac": true})
			if got.Status != "CURRENT" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestModulesAddsPlaybackValidationWithoutAdminPrivileges(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "modules-validation-device")

	read := func() (playbackValidation, int, int) {
		t.Helper()
		response := call(s, "GET", "/v1/modules", "", "modules-validation-device", token, "")
		if response.Code != 200 {
			t.Fatalf("GET /v1/modules returned %d: %s", response.Code, response.Body)
		}
		var body struct {
			APIVersion         int                `json:"apiVersion"`
			Modules            []domain.Module    `json:"modules"`
			PlaybackValidation playbackValidation `json:"playbackValidation"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.PlaybackValidation, body.APIVersion, len(body.Modules)
	}

	initial, apiVersion, moduleCount := read()
	if apiVersion != 1 || moduleCount == 0 {
		t.Fatalf("existing module response changed: apiVersion=%d modules=%d", apiVersion, moduleCount)
	}
	if initial.Status != "REFRESH_REQUIRED" || initial.Reason != "suite_missing" {
		t.Fatalf("first-run validation should request checks: %+v", initial)
	}

	manifestResponse := call(s, "GET", "/v1/probes?suite=2&extended=1", "", "modules-validation-device", token, "")
	if manifestResponse.Code != 200 {
		t.Fatalf("probe manifest returned %d: %s", manifestResponse.Code, manifestResponse.Body)
	}
	var manifest struct {
		CacheKey     string `json:"cacheKey"`
		SuiteVersion int    `json:"suiteVersion"`
	}
	if err := json.Unmarshal(manifestResponse.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	testedAt := time.Now().Unix()
	capabilities, err := json.Marshal(map[string]any{
		"capabilitiesVersion": 1,
		"deviceId":            "modules-validation-device",
		"suiteVersion":        manifest.SuiteVersion,
		"cacheKey":            manifest.CacheKey,
		"probes": []domain.Probe{
			playbackProbe("audio-track-pcm-stream", "UNKNOWN", testedAt),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	updated := call(
		s,
		"PUT",
		"/v1/device/capabilities",
		string(capabilities),
		"modules-validation-device",
		token,
		"",
	)
	if updated.Code != 200 {
		t.Fatalf("capability update returned %d: %s", updated.Code, updated.Body)
	}
	current, _, _ := read()
	if current.Status != "CURRENT" || current.Reason != "" {
		t.Fatalf("fresh UNKNOWN outcome should be current: %+v", current)
	}

	if unauthenticated := call(s, "GET", "/v1/modules", "", "", "", ""); unauthenticated.Code != 401 {
		t.Fatalf("/v1/modules authentication changed: %d", unauthenticated.Code)
	}
}
