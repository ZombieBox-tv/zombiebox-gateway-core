package server

import (
	"encoding/json"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestAuthenticatedClientVersionRefreshInvalidatesOnlyChangedApp(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "probe-device")
	otherToken := pair(t, s, "other-device")
	if response := call(s, "PUT", "/v1/device/client", `{"clientVersion":"0.1.0-dev.52"}`, "probe-device", "", ""); response.Code != 401 {
		t.Fatalf("version refresh without a token returned %d", response.Code)
	}
	if response := call(s, "PUT", "/v1/device/client", `{"clientVersion":"0.1.0-dev.52"}`, "probe-device", otherToken, ""); response.Code != 401 {
		t.Fatalf("another device's token returned %d", response.Code)
	}
	if response := call(s, "PUT", "/v1/device/client", `{"clientVersion":"bad version"}`, "probe-device", token, ""); response.Code != 400 {
		t.Fatalf("invalid version returned %d", response.Code)
	}
	if response := call(s, "PUT", "/v1/device/capabilities", `{"capabilitiesVersion":1,"deviceId":"probe-device","probes":[{"id":"aac","status":"PASS"}]}`, "probe-device", token, ""); response.Code != 200 {
		t.Fatalf("probe setup returned %d", response.Code)
	}
	for _, version := range []string{"test", "0.1.0-dev.52", "0.1.0-dev.52"} {
		response := call(s, "PUT", "/v1/device/client", `{"clientVersion":"`+version+`"}`, "probe-device", token, "")
		if response.Code != 200 {
			t.Fatalf("refresh %s returned %d: %s", version, response.Code, response.Body)
		}
		deviceResponse := call(s, "GET", "/v1/device", "", "probe-device", token, "")
		var device domain.Device
		if err := json.Unmarshal(deviceResponse.Body.Bytes(), &device); err != nil {
			t.Fatal(err)
		}
		if device.Registration.ClientVersion != version {
			t.Fatalf("stored version %s, want %s", device.Registration.ClientVersion, version)
		}
		if version == "test" && len(device.Capabilities.Probes) != 1 {
			t.Fatal("same version discarded valid evidence")
		}
		if version != "test" && len(device.Capabilities.Probes) != 0 {
			t.Fatal("APK update retained stale evidence")
		}
	}
	if response := call(s, "GET", "/v1/device", "", "other-device", otherToken, ""); response.Code != 200 {
		t.Fatal("version refresh changed another device's token")
	}
}
