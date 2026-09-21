package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"zombiebox.local/gateway/internal/domain"
)

// The Python schema check invokes this test with an isolated capture path.
// Samples come from real handlers, so fixtures cannot hide response drift.
func TestWireContracts(t *testing.T) {
	path := os.Getenv("ZOMBIE_CONTRACT_CAPTURE")
	if path == "" {
		t.Skip("invoked by scripts/check-protocol.py")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Contract.mp4"), []byte("contract"), 0600); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, nil, dir)
	registration := `{"clientVersion":"contract","protocolVersion":1,"installationId":"contract-device","pairingCode":"123456","platform":{"androidApi":13}}`
	reg := call(s, "POST", "/v1/devices/register", registration, "", "", "")
	samples := map[string]json.RawMessage{"DeviceRegistration": json.RawMessage(registration), "RegistrationResponse": reg.Body.Bytes()}
	var result struct{ DeviceToken string }
	if err := json.Unmarshal(reg.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	token := result.DeviceToken
	for name, path := range map[string]string{"Health": "/health", "ScreenModel": "/v1/home", "DeviceRecord": "/v1/device", "ProviderStatusResponse": "/v1/providers", "EventBatch": "/v1/events?wait=0"} {
		w := call(s, "GET", path, "", "contract-device", token, "")
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		samples[name] = w.Body.Bytes()
	}
	var screen domain.Screen
	json.Unmarshal(samples["ScreenModel"], &screen)
	w := call(s, "POST", "/v1/playback", `{"itemId":"`+screen.Hero.Item.ID+`"}`, "contract-device", token, "")
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	samples["PlaybackPlan"] = w.Body.Bytes()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("#EXTM3U\n#EXTINF:1,\nsegment.ts\n")) }))
	defer relay.Close()
	s.opt.RelayURL = relay.URL
	s.opt.RelayAdminToken = "contract-only-relay-token"
	receiver := pair(t, s, "contract-receiver")
	preferences := `{"mode":"TV","uiLanguage":"en","subtitleMode":"auto","allowCasting":true}`
	call(s, "PUT", "/v1/device/preferences", preferences, "contract-receiver", receiver, "")
	samples["Preferences"] = call(s, "GET", "/v1/device/preferences", "", "contract-receiver", receiver, "").Body.Bytes()
	samples["CastReceivers"] = call(s, "GET", "/v1/cast/receivers", "", "contract-device", token, "").Body.Bytes()
	samples["CastRequest"] = json.RawMessage(`{"receiverId":"contract-receiver"}`)
	grant := call(s, "POST", "/v1/cast", string(samples["CastRequest"]), "contract-device", token, "")
	if grant.Code != 201 {
		t.Fatal(grant.Body)
	}
	samples["CastGrant"] = grant.Body.Bytes()
	var cast struct{ CastID string }
	json.Unmarshal(grant.Body.Bytes(), &cast)
	if ready := call(s, "POST", "/v1/cast/"+cast.CastID+"/ready", "", "contract-device", token, ""); ready.Code != 200 {
		t.Fatal(ready.Body)
	}
	samples["ActiveCast"] = call(s, "GET", "/v1/cast/active", "", "contract-receiver", receiver, "").Body.Bytes()
	data, err := json.Marshal(samples)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
