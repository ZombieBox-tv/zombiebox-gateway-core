package server

import (
	"encoding/json"
	"strings"
	"testing"
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
	if validHardware(report) {
		t.Fatal("inventory certified functional DIAL")
	}
	report = hardwareFixture()
	report.Decoders = make([]domain.CodecHint, 129)
	if validHardware(report) {
		t.Fatal("unbounded decoder inventory")
	}
}
