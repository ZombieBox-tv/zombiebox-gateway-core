package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
