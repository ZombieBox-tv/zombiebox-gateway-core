package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	youtubereceiver "zombiebox.local/gateway/internal/receivers/youtube"

	"zombiebox.local/gateway/internal/companionmedia"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
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
	browseFixture(t, s)
	browse := call(s, "GET", "/v1/browse?provider=plex", "", "contract-device", token, "")
	if browse.Code != 200 {
		t.Fatal(browse.Body)
	}
	samples["BrowsePage"] = browse.Body.Bytes()
	samples["MediaReceiver"] = call(s, "GET", "/v1/media-receiver", "", "contract-device", token, "").Body.Bytes()
	samples["MediaReceiverSelection"] = json.RawMessage(`{"provider":"auto"}`)

	for name, path := range map[string]string{"SearchResults": "/v1/search?q=movie", "DiagnosticReport": "/v1/diagnostics", "IntegrationResponse": "/v1/integrations", "Health": "/health", "ScreenModel": "/v1/home", "DeviceRecord": "/v1/device", "ProviderStatusResponse": "/v1/providers", "EventBatch": "/v1/events?wait=0", "YouTubeAccountStatus": "/v1/youtube/account"} {
		w := call(s, "GET", path, "", "contract-device", token, "")
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		samples[name] = w.Body.Bytes()
	}
	download := call(s, "GET", "/v1/network/sample", "", "contract-device", token, "")
	samples["NetworkSampleReport"] = json.RawMessage(`{"sampleId":"` + download.Header().Get("X-Zombie-Sample") + `","bytes":1048576,"elapsedMs":1250}`)
	report := call(s, "POST", "/v1/device/network", string(samples["NetworkSampleReport"]), "contract-device", token, "")
	if report.Code != 200 {
		t.Fatal(report.Body)
	}
	samples["NetworkEstimate"] = report.Body.Bytes()

	var screen domain.Screen
	json.Unmarshal(samples["ScreenModel"], &screen)
	w := call(s, "POST", "/v1/playback", `{"itemId":"`+screen.Hero.Item.ID+`"}`, "contract-device", token, "")
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	samples["PlaybackPlan"] = w.Body.Bytes()
	var localPlan domain.Plan
	if err := json.Unmarshal(w.Body.Bytes(), &localPlan); err != nil {
		t.Fatal(err)
	}
	s.deps.Media = &trackMedia{}
	samples["TrackInventory"] = call(s, "GET", "/v1/playback/"+localPlan.SessionID+"/tracks", "", "contract-device", token, "").Body.Bytes()
	samples["SubtitleCues"] = call(s, "GET", "/v1/playback/"+localPlan.SessionID+"/subtitles/2", "", "contract-device", token, "").Body.Bytes()
	samples["AudioSelection"] = json.RawMessage(`{"audioId":1,"positionMs":1250}`)
	selected := call(s, "POST", "/v1/playback/"+localPlan.SessionID+"/audio", string(samples["AudioSelection"]), "contract-device", token, "")
	if selected.Code != 201 {
		t.Fatal(selected.Body)
	}
	samples["PlaybackPlan"] = selected.Body.Bytes()

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
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/code" {
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/status" {
			w.Write([]byte(`{"stopped":false,"volume_steps":100,"track":{"name":"Contract song","artist_names":["Contract artist"]}}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer worker.Close()
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"spotify": {Enabled: true, URL: worker.URL, Token: strings.Repeat("x", 32)}, "rebrowser": {Enabled: true, URL: worker.URL, Token: strings.Repeat("x", 32)}}); err != nil {
		t.Fatal(err)
	}
	call(s, "PUT", "/v1/media-receiver", `{"provider":"auto"}`, "contract-device", token, "")
	receiverState := call(s, "GET", "/v1/media-receiver", "", "contract-device", token, "")
	if receiverState.Code != 200 {
		t.Fatal(receiverState.Body)
	}
	samples["MediaReceiver"] = receiverState.Body.Bytes()
	samples["NowPlaying"] = call(s, "GET", "/v1/player/spotify", "", "contract-device", token, "").Body.Bytes()
	samples["AuthorizationPrompt"] = call(s, "GET", "/v1/player/spotify/authorization", "", "contract-device", token, "123456").Body.Bytes()
	samples["BrowserSession"] = call(s, "POST", "/v1/browser", `{"url":"https://example.org"}`, "contract-device", token, "").Body.Bytes()
	samples["ProbeManifest"] = call(s, "GET", "/v1/probes", "", "contract-device", token, "").Body.Bytes()
	s.deps.YouTubeReceiver = &receiverFixture{state: domain.YouTubeReceiverState{State: "READY", TVCode: "123456789"}}
	s.youtubeReceiver = youtubereceiver.New(s.deps.YouTubeReceiver, s.config, func() string { return randomID(16) })
	s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "https://fixture.invalid", Token: strings.Repeat("t", 32)}, "youtube_receiver": {Enabled: true, URL: "https://fixture.invalid", Token: strings.Repeat("t", 32)}})
	call(s, "DELETE", "/v1/media-receiver", "", "contract-device", token, "")
	samples["YouTubeReceiverState"] = call(s, "POST", "/v1/youtube/receiver", `{}`, "contract-device", token, "").Body.Bytes()
	inventory := hardwareFixture()
	inventory.Encoders = []domain.CodecHint{{Name: "Declared encoder", Types: []string{"video/avc"}, Acceleration: "UNKNOWN", Profiles: []domain.CodecProfileHint{{MIME: "video/avc", Profile: 1, Level: 256}}}}
	inventory.Displays = []domain.DisplayHint{{ID: 0, Default: true, Width: 1920, Height: 1080, RefreshMilliHz: 60000, ActiveModeID: 1, Modes: []domain.DisplayModeHint{{ID: 1, Width: 1920, Height: 1080, RefreshMilliHz: 60000}}}}
	hardware, _ := json.Marshal(inventory)
	samples["HardwareReport"] = call(s, "PUT", "/v1/device/hardware", string(hardware), "contract-device", token, "").Body.Bytes()
	companionContractSamples(t, s, samples, "contract-device", token)
	data, err := json.Marshal(samples)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func companionContractSamples(t *testing.T, s *Server, samples map[string]json.RawMessage, id, token string) {
	t.Helper()
	invitation := call(s, "POST", "/v1/device/companions/invitations", `{"gateway":"http://192.0.2.10:8090"}`, id, token, "")
	if invitation.Code != 201 {
		t.Fatal(invitation.Body)
	}
	samples["CompanionInvitation"] = invitation.Body.Bytes()
	var invitationBody struct{ Code string }
	_ = json.Unmarshal(invitation.Body.Bytes(), &invitationBody)
	joined := call(s, "POST", "/v1/companion/join", `{"name":"Phone","code":"`+invitationBody.Code+`"}`, "", "", "")
	if joined.Code != 201 {
		t.Fatal(joined.Body)
	}
	samples["CompanionJoinResponse"] = joined.Body.Bytes()
	var body struct {
		Request struct{ ID string }
		Token   string
	}
	_ = json.Unmarshal(joined.Body.Bytes(), &body)
	approved := call(s, "POST", "/v1/device/companions/"+body.Request.ID+"/decision", `{"accept":true}`, id, token, "")
	if approved.Code != 200 {
		t.Fatal(approved.Body)
	}
	samples["CompanionInventory"] = call(s, "GET", "/v1/device/companions", "", id, token, "").Body.Bytes()
	samples["CompanionRequest"] = call(s, "POST", "/v1/companion/requests/"+body.Request.ID, `{"token":"`+body.Token+`"}`, "", "", "").Body.Bytes()
	call(s, "POST", "/v1/device/remote/poll", `{"active":true}`, id, token, "")
	command := call(s, "POST", "/v1/companion/commands", `{"action":"OK"}`, body.Request.ID, body.Token, "")
	if command.Code != 202 {
		t.Fatal(command.Body)
	}
	samples["CompanionCommand"] = command.Body.Bytes()
	samples["CompanionPoll"] = call(s, "POST", "/v1/device/remote/poll", `{"active":true}`, id, token, "").Body.Bytes()
	samples["CompanionStatus"] = call(s, "GET", "/v1/companion/status", "", body.Request.ID, body.Token, "").Body.Bytes()
	uploads, err := companionmedia.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer uploads.Close()
	s.deps.Uploads = uploads
	s.deps.Media = &trackMedia{}
	file := "\x00\x00\x00\x18ftypisomcontents"
	uploaded := call(s, "PUT", "/v1/companion/media/"+strings.Repeat("d", 32), file, body.Request.ID, body.Token, "")
	if uploaded.Code != 201 {
		t.Fatal(uploaded.Code, uploaded.Body)
	}
	samples["CompanionMediaReceipt"] = uploaded.Body.Bytes()
	samples["CompanionMediaPlay"] = json.RawMessage(`{"title":"My video"}`)
	samples["CompanionMediaStatus"] = call(s, "GET", "/v1/companion/media", "", body.Request.ID, body.Token, "").Body.Bytes()
}
