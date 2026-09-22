package server

import (
	"encoding/json"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/devices"

	"zombiebox.local/gateway/internal/domain"
)

func hardwareFixture() domain.HardwareReport {
	return domain.HardwareReport{Version: 1, Fingerprint: strings.Repeat("a", 64), ABIs: []string{"armeabi-v7a"}, CPUCores: 2, PhysicalMB: 1024, Network: "ETHERNET", Decoders: []domain.CodecHint{}, ExternalPlayers: []string{}, NativeDIAL: "UNKNOWN", Multicast: "UNKNOWN"}
}
func TestHardwareInvalidatesProbesOnlyWhenFingerprintChanges(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "hardware-device")
	report := hardwareFixture()
	save := func() {
		data, _ := json.Marshal(report)
		w := call(s, "PUT", "/v1/device/hardware", string(data), "hardware-device", token, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	save()
	caps := `{"capabilitiesVersion":1,"deviceId":"hardware-device","probes":[{"id":"aac","status":"PASS"}]}`
	if w := call(s, "PUT", "/v1/device/capabilities", caps, "hardware-device", token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	save()
	var device domain.Device
	s.db.Get(t.Context(), "devices", "hardware-device", &device)
	if len(device.Capabilities.Probes) != 1 {
		t.Fatal("stable inventory lost probe evidence")
	}
	report.Fingerprint = strings.Repeat("b", 64)
	save()
	s.db.Get(t.Context(), "devices", "hardware-device", &device)
	if len(device.Capabilities.Probes) != 0 {
		t.Fatal("changed firmware retained stale probes")
	}
	if w := call(s, "PUT", "/v1/device/hardware", `{}`, "hardware-device", token, ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := call(s, "PUT", "/v1/device/hardware", `{}`, "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestHardwareHintsCannotCertifyNativeBackends(t *testing.T) {
	report := hardwareFixture()
	report.NativeDIAL = "PASS"
	if devices.ValidHardware(report) {
		t.Fatal("inventory certified functional DIAL")
	}
	report = hardwareFixture()
	report.Decoders = make([]domain.CodecHint, 129)
	if devices.ValidHardware(report) {
		t.Fatal("unbounded decoder inventory")
	}
}

func TestNativeInventoryRemainsDeclarativeAndBounded(t *testing.T) {
	report := hardwareFixture()
	report.Decoders = []domain.CodecHint{{Name: "OEM.decoder", Types: []string{"video/hevc"}, Acceleration: "HARDWARE", Profiles: []domain.CodecProfileHint{{MIME: "video/hevc", Profile: 1, Level: 1024}}}}
	report.Encoders = []domain.CodecHint{{Name: "OEM.encoder", Types: []string{"video/avc"}, Acceleration: "UNKNOWN"}}
	report.Displays = []domain.DisplayHint{{ID: 0, Default: true, Width: 3840, Height: 2160, RefreshMilliHz: 59940, ActiveModeID: 1, Modes: []domain.DisplayModeHint{{ID: 1, Width: 3840, Height: 2160, RefreshMilliHz: 59940}}}}
	if !devices.ValidHardware(report) {
		t.Fatal("valid native inventory rejected")
	}
	s := testServer(t, nil, "")
	token := pair(t, s, "inventory-device")
	raw, _ := json.Marshal(report)
	if w := call(s, "PUT", "/v1/device/hardware", string(raw), "inventory-device", token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var device domain.Device
	_ = s.db.Get(t.Context(), "devices", "inventory-device", &device)
	if len(device.Capabilities.Probes) != 0 {
		t.Fatal("inventory manufactured playback evidence")
	}
	w := call(s, "GET", "/v1/diagnostics", "", "inventory-device", token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"encoderCount":1`) || !strings.Contains(w.Body.String(), `"refreshMilliHz":59940`) {
		t.Fatal(w.Body)
	}
	for _, mutate := range []func(*domain.HardwareReport){
		func(r *domain.HardwareReport) { r.Encoders[0].ProbeCandidates = []string{"h264-2160-high"} },
		func(r *domain.HardwareReport) { r.Decoders[0].Acceleration = "PASS" },
		func(r *domain.HardwareReport) { r.Decoders[0].Profiles[0].MIME = "video/avc" },
		func(r *domain.HardwareReport) { r.Displays[0].ActiveModeID = 99 },
		func(r *domain.HardwareReport) { r.Displays[0].Modes[0].Width = 1920 },
		func(r *domain.HardwareReport) { r.Displays[0].RefreshMilliHz = 480001 },
		func(r *domain.HardwareReport) { r.Displays = append(r.Displays, r.Displays[0]) },
	} {
		var candidate domain.HardwareReport
		_ = json.Unmarshal(raw, &candidate)
		mutate(&candidate)
		if devices.ValidHardware(candidate) {
			t.Fatal("invalid inventory accepted", candidate)
		}
	}
}
