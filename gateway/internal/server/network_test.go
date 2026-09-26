package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

func TestNetworkSampleIsBoundedPairedSingleUseAndExpires(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "network-device")
	other := pair(t, s, "other-network-device")
	if w := call(s, "GET", "/v1/network/sample", "", "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := call(s, "GET", "/v1/network/sample", "", "network-device", token, "")
	if w.Code != 200 || w.Body.Len() != 1048576 || w.Header().Get("Content-Encoding") != "identity" {
		t.Fatal(w.Code, w.Body.Len())
	}
	id := w.Header().Get("X-Zombie-Sample")
	request := `{"sampleId":"` + id + `","bytes":131072,"elapsedMs":1500}`
	if w := call(s, "POST", "/v1/device/network", request, "other-network-device", other, ""); w.Code != 409 {
		t.Fatal("cross-device report", w.Code)
	}
	w = call(s, "POST", "/v1/device/network", request, "network-device", token, "")
	var report struct {
		Kbps int64 `json:"kbps"`
	}
	json.Unmarshal(w.Body.Bytes(), &report)
	if w.Code != 200 || report.Kbps != 699 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(s, "POST", "/v1/device/network", request, "network-device", token, ""); w.Code != 409 {
		t.Fatal("replayed report", w.Code)
	}
	if w := call(s, "GET", "/v1/network/sample", "", "network-device", token, ""); w.Code != 429 {
		t.Fatal("sample cooldown", w.Code)
	}
	metadata := &domain.Metadata{Streams: []domain.Stream{{Type: "video"}}}
	decision := playbackDecision{mode: "DIRECT_PLAY", metadata: metadata}
	device := domain.Device{ID: "network-device"}
	if s.networkQuality(device, decision) != "LOW" {
		t.Fatal("measured low link ignored")
	}
	device.Capabilities.Probes = []domain.Probe{{ID: "http-fmp4", Status: "FAIL"}}
	if s.networkQuality(device, decision) != "" {
		t.Fatal("known decoder failure bypassed")
	}
	device.Capabilities.Probes = nil
	sample := s.networkSamples[device.ID]
	sample.measured = time.Now().Add(-6 * time.Minute)
	s.networkSamples[device.ID] = sample
	if s.networkQuality(device, decision) != "" {
		t.Fatal("stale estimate used")
	}
}

func TestNetworkQualitySkipsDowngradeForKnownLengthOnlyFMP4(t *testing.T) {
	s := testServer(t, nil, "")
	now := time.Now()
	s.networkSamples["network-quality-device"] = networkSample{measured: now, kbps: 700}
	decision := playbackDecision{
		mode:     "DIRECT_PLAY",
		metadata: &domain.Metadata{Streams: []domain.Stream{{Type: "video"}}},
	}
	knownLengthPass := domain.Probe{
		ID: "http-fmp4", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix(),
	}
	chunkedPrepareRejection := domain.Probe{
		ID: "http-fmp4-chunked", Status: "UNKNOWN",
		Detail: "what=0,extra=0@prepare http=200,video/mp4", TestedAt: now.Unix(),
	}

	capabilities := func(device domain.Device, probes ...domain.Probe) domain.Capabilities {
		return domain.Capabilities{
			SuiteVersion: devices.ProbeSuiteVersion,
			DeviceID:     device.ID,
			CacheKey:     devices.ProbeCacheKey(device),
			Probes:       probes,
		}
	}
	withEvidence := func(chunked domain.Probe) func(*domain.Device) {
		return func(device *domain.Device) {
			device.Capabilities = capabilities(*device, knownLengthPass, chunked)
		}
	}

	tests := []struct {
		name      string
		configure func(*domain.Device)
		want      string
	}{
		{name: "other device keeps low-link adaptation", want: "LOW"},
		{
			name:      "fresh known-length pass and recorded chunked prepare rejection",
			configure: withEvidence(chunkedPrepareRejection),
			want:      "",
		},
		{
			name: "fresh known-length pass and chunked failure",
			configure: withEvidence(domain.Probe{
				ID: "http-fmp4-chunked", Status: "FAIL", TestedAt: now.Unix(),
			}),
			want: "",
		},
		{
			name: "fresh known-length pass and stalled chunked probe",
			configure: withEvidence(domain.Probe{
				ID: "http-fmp4-chunked", Status: "UNKNOWN", Stalled: true, TestedAt: now.Unix(),
			}),
			want: "",
		},
		{
			name: "stale chunked rejection is ignored",
			configure: withEvidence(domain.Probe{
				ID: "http-fmp4-chunked", Status: "UNKNOWN",
				Detail: chunkedPrepareRejection.Detail, TestedAt: now.Add(-8 * 24 * time.Hour).Unix(),
			}),
			want: "LOW",
		},
		{
			name: "stale known-length pass is ignored",
			configure: func(device *domain.Device) {
				stalePass := knownLengthPass
				stalePass.TestedAt = now.Add(-8 * 24 * time.Hour).Unix()
				device.Capabilities = capabilities(*device, stalePass, chunkedPrepareRejection)
			},
			want: "LOW",
		},
		{
			name: "wrong cache key is ignored",
			configure: func(device *domain.Device) {
				device.Capabilities = capabilities(*device, knownLengthPass, chunkedPrepareRejection)
				device.Capabilities.CacheKey = "stale-cache-key"
			},
			want: "LOW",
		},
		{
			name: "ambiguous unknown is ignored",
			configure: withEvidence(domain.Probe{
				ID: "http-fmp4-chunked", Status: "UNKNOWN",
				Detail: "error@prepare http=200,video/mp4", TestedAt: now.Unix(),
			}),
			want: "LOW",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := domain.Device{ID: "network-quality-device"}
			if test.configure != nil {
				test.configure(&device)
			}
			if got := s.networkQuality(device, decision); got != test.want {
				t.Fatalf("networkQuality() = %q, want %q", got, test.want)
			}
		})
	}
}

type networkMedia struct{ trackMedia }

func (*networkMedia) Probe(context.Context, string) (domain.Metadata, error) {
	metadata := domain.Metadata{Streams: []domain.Stream{{Index: 0, Type: "video", Codec: "h264", Profile: "Constrained Baseline", Width: 640, Height: 360}}}
	metadata.Format.BitRate = "8000000"
	return metadata, nil
}

func TestPlaybackAppliesNetworkQualityAndRespectsOverrides(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Movie.mp4"), []byte("fixture"), 0600)
	s := testServer(t, nil, dir)
	s.deps.Media = &networkMedia{}
	token := pair(t, s, "network-playback")
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", "network-playback", &device); err != nil {
		t.Fatal(err)
	}
	device.Capabilities = domain.Capabilities{SuiteVersion: devices.ProbeSuiteVersion, CacheKey: devices.ProbeCacheKey(device), Probes: []domain.Probe{
		{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
	}}
	if err := s.db.Put(t.Context(), "devices", device.ID, device); err != nil {
		t.Fatal(err)
	}
	sources, err := providers.Local(dir)
	if err != nil || len(sources) != 1 {
		t.Fatal(err)
	}
	s.networkSamples["network-playback"] = networkSample{measured: time.Now(), kbps: 700}
	for _, test := range []struct {
		mode  string
		adapt bool
		want  string
	}{
		{"AUTO", true, "TRANSCODE"}, {"AUTO", false, "DIRECT_PLAY"}, {"DIRECT_PLAY", true, "DIRECT_PLAY"},
	} {
		body, _ := json.Marshal(map[string]any{"itemId": sources[0].Item.ID, "mode": test.mode, "networkAdaptation": test.adapt, "positionMs": 1200})
		w := call(s, "POST", "/v1/playback", string(body), "network-playback", token, "")
		var plan domain.Plan
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &plan) != nil || plan.Mode != test.want {
			t.Fatal(w.Code, w.Body)
		}
		if plan.Mode == "TRANSCODE" && (s.sessions[plan.SessionID].selection.Quality != "LOW" || plan.TimelineOffsetMS != 1200) {
			t.Fatal("lost profile/position", plan)
		}
	}
}

func TestContinuousAdaptationPreservesSessionUntilClientAdoption(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Movie.mp4"), []byte("fixture"), 0600)
	s := testServer(t, nil, dir)
	s.deps.Media = &networkMedia{}
	token := pair(t, s, "continuous-device")
	sources, _ := providers.Local(dir)
	body, _ := json.Marshal(map[string]any{"itemId": sources[0].Item.ID, "mode": "AUTO", "networkAdaptation": true})
	w := call(s, "POST", "/v1/playback", string(body), "continuous-device", token, "")
	var original domain.Plan
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &original) != nil {
		t.Fatal(w.Code, w.Body)
	}
	now := time.Now()
	s.networkSamples["continuous-device"] = networkSample{measured: now.Add(-time.Minute), kbps: 700}
	route := "/v1/playback/" + original.SessionID + "/adapt"
	w = call(s, "POST", route, `{"positionMs":20000}`, "continuous-device", token, "")
	var result struct{ Plan *domain.Plan }
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.Plan != nil {
		t.Fatal("single sample switched", w.Body)
	}
	s.networkSamples["continuous-device"] = networkSample{measured: now, kbps: 700}
	w = call(s, "POST", route, `{"positionMs":21000}`, "continuous-device", token, "")
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.Plan == nil || result.Plan.TimelineOffsetMS != 21000 {
		t.Fatal(w.Code, w.Body)
	}
	old := s.sessions[original.SessionID]
	next := s.sessions[result.Plan.SessionID]
	if old == nil || old.ctx.Err() != nil || next.selection.Quality != "LOW" || next.selection.AudioID != old.selection.AudioID {
		t.Fatal("lost source or selection before adoption")
	}
	w = call(s, "POST", route, `{"positionMs":22000}`, "continuous-device", token, "")
	result.Plan = nil
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.Plan != nil {
		t.Fatal("sample replay created replacement")
	}
}
