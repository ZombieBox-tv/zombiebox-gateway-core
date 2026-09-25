package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/playback"
)

func TestCastConsentOwnershipRelayAndRevocation(t *testing.T) {
	s := testServer(t, nil, "")
	sender := pair(t, s, "cast-sender")
	receiver := pair(t, s, "cast-receiver")
	other := pair(t, s, "other-device")
	var masters atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		s.mu.Lock()
		valid := false
		for _, c := range s.casts {
			if user == "zombie" && password == c.readToken {
				valid = true
			}
		}
		s.mu.Unlock()
		if !ok || !valid {
			w.WriteHeader(403)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/index.m3u8") {
			masters.Add(1)
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nstream.m3u8?reader=opaque\n")
			return
		}
		if r.URL.Query().Get("reader") != "opaque" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nsegment.ts?reader=opaque\n")
	}))
	defer relay.Close()
	s.opt.RelayURL = relay.URL
	s.opt.RelayAdminToken = strings.Repeat("a", 32)
	create := func() *httptest.ResponseRecorder {
		return call(s, "POST", "/v1/cast", `{"receiverId":"cast-receiver"}`, "cast-sender", sender, "")
	}
	if w := create(); w.Code != 409 {
		t.Fatalf("must require opt-in: %d", w.Code)
	}
	prefs := `{"mode":"TV","uiLanguage":"en","subtitleMode":"auto","allowCasting":true}`
	if w := call(s, "PUT", "/v1/device/preferences", prefs, "cast-receiver", receiver, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := call(s, "GET", "/v1/cast/receivers", "", "cast-sender", sender, ""); !strings.Contains(w.Body.String(), `"online":true`) {
		t.Fatal(w.Body)
	}
	w := create()
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	var grant struct{ CastID, PublishToken, PublishPath string }
	json.Unmarshal(w.Body.Bytes(), &grant)
	if w := create(); w.Code != 429 {
		t.Fatal("unbounded casts")
	}
	auth := func(action, password, key string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"user": "zombie", "password": password, "path": grant.PublishPath, "action": action, "id": "12345678-abcd"})
		return call(s, "POST", "/internal/relay/auth?key="+key, string(body), "", "", "")
	}
	if auth("publish", grant.PublishToken, "").Code != 403 {
		t.Fatal("public auth callback accepted")
	}
	if auth("publish", grant.PublishToken, s.opt.RelayAdminToken).Code != 204 {
		t.Fatal("publisher rejected")
	}
	if auth("read", grant.PublishToken, s.opt.RelayAdminToken).Code != 403 {
		t.Fatal("publish credential grants read")
	}
	path := "/v1/cast/" + grant.CastID
	if call(s, "PUT", path, "", "other-device", other, "").Code != 404 {
		t.Fatal("lease ownership")
	}
	if call(s, "POST", path+"/ready", "", "cast-sender", sender, "").Code != 200 {
		t.Fatal("relay readiness")
	}
	var active struct{ Plan domain.Plan }
	json.Unmarshal(call(s, "GET", "/v1/cast/active", "", "cast-receiver", receiver, "").Body.Bytes(), &active)
	if active.Plan.SessionID == "" || !active.Plan.Live {
		t.Fatal("missing receiver plan")
	}
	stream := call(s, "GET", active.Plan.URL, "", "", "", "")
	if stream.Code != 200 || strings.Contains(stream.Body.String(), relay.URL) || !strings.Contains(stream.Body.String(), "ticket=") {
		t.Fatalf("unprotected HLS: %d %s", stream.Code, stream.Body)
	}
	call(s, "GET", active.Plan.URL, "", "", "", "")
	if masters.Load() != 1 {
		t.Fatal("master playlist recreated relay readers")
	}
	if w := call(s, "GET", "/v1/cast/active", "", "other-device", other, ""); !strings.Contains(w.Body.String(), `"plan":null`) {
		t.Fatal("plan leaked")
	}
	if call(s, "DELETE", path, "", "other-device", other, "").Code != 404 {
		t.Fatal("stop ownership")
	}
	prefs = strings.Replace(prefs, "true", "false", 1)
	if call(s, "PUT", "/v1/device/preferences", prefs, "cast-receiver", receiver, "").Code != 200 {
		t.Fatal("disable")
	}
	if auth("publish", grant.PublishToken, s.opt.RelayAdminToken).Code != 403 {
		t.Fatal("revoked publisher accepted")
	}
	if call(s, "GET", active.Plan.URL, "", "", "", "").Code != 401 {
		t.Fatal("revoked stream accepted")
	}
}

func TestCastLeaseExpiryAndClose(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "cast-sender")
	s.casts["expired"] = &castSession{id: "expired", sender: "cast-sender", receiver: "receiver", expires: time.Now().Add(-time.Second)}
	if call(s, "PUT", "/v1/cast/expired", "", "cast-sender", token, "").Code != 404 {
		t.Fatal("expired lease renewed")
	}
	s.Close()
	if len(s.casts) != 0 {
		t.Fatal("shutdown retained casts")
	}
}

func TestCastWireNegotiatesCeilingWithoutRaisingOldSenders(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "sender-ceiling")
	receiverToken := pair(t, s, "receiver-ceiling")
	call(s, "GET", "/v1/device", "", "receiver-ceiling", receiverToken, "")
	s.opt.RelayURL = "http://127.0.0.1:8888"
	var receiver domain.Device
	if err := s.db.Get(t.Context(), "devices", "receiver-ceiling", &receiver); err != nil {
		t.Fatal(err)
	}
	receiver.Preferences.AllowCasting = true
	receiver.Capabilities = domain.Capabilities{SuiteVersion: 2, CacheKey: devices.ProbeCacheKey(receiver), Probes: []domain.Probe{
		{ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
	}}
	if err := s.db.Put(t.Context(), "devices", receiver.ID, receiver); err != nil {
		t.Fatal(err)
	}
	for _, example := range []struct {
		extra          string
		status, height int
	}{
		{"", 201, 360}, {`,"maxVideoHeight":720`, 201, 360},
		{`,"maxVideoHeight":1080`, 201, 1080}, {`,"maxVideoHeight":2160`, 201, 1080},
		{`,"maxVideoHeight":0`, 400, 0}, {`,"maxVideoHeight":4320`, 400, 0},
	} {
		w := call(s, "POST", "/v1/cast", `{"receiverId":"receiver-ceiling"`+example.extra+`}`, "sender-ceiling", token, "")
		if w.Code != example.status {
			t.Fatal(w.Code, w.Body)
		}
		if w.Code != 201 {
			continue
		}
		var grant struct {
			CastID string
			Video  playback.CastVideo
		}
		if err := json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
			t.Fatal(err)
		}
		if grant.Video.MaxHeight != example.height {
			t.Fatal(grant)
		}
		if w = call(s, "DELETE", "/v1/cast/"+grant.CastID, "", "sender-ceiling", token, ""); w.Code != 200 {
			t.Fatal(w.Body)
		}
	}

	// Receiver with verified 4K probes and display evidence
	receiver4KToken := pair(t, s, "receiver-4k")
	call(s, "GET", "/v1/device", "", "receiver-4k", receiver4KToken, "")
	var receiver4K domain.Device
	if err := s.db.Get(t.Context(), "devices", "receiver-4k", &receiver4K); err != nil {
		t.Fatal(err)
	}
	receiver4K.Preferences.AllowCasting = true
	receiver4K.Registration.Display = domain.Display{Width: 3840, Height: 2160}
	receiver4K.Registration.Hardware = &domain.HardwareReport{Displays: []domain.DisplayHint{{Default: true, Modes: []domain.DisplayModeHint{{Height: 2160}}}}}
	receiver4K.Registration.Memory.PhysicalMB = 2048
	receiver4K.Capabilities = domain.Capabilities{SuiteVersion: 2, CacheKey: devices.ProbeCacheKey(receiver4K), Probes: []domain.Probe{
		{ID: "h264-2160-high", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		{ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
	}}
	if err := s.db.Put(t.Context(), "devices", receiver4K.ID, receiver4K); err != nil {
		t.Fatal(err)
	}
	for _, example := range []struct {
		extra          string
		status, height int
	}{
		{"", 201, 360},
		{`,"maxVideoHeight":720`, 201, 360},
		{`,"maxVideoHeight":1080`, 201, 1080},
		{`,"maxVideoHeight":2160`, 201, 2160},
		{`,"maxVideoHeight":4320`, 400, 0},
	} {
		w := call(s, "POST", "/v1/cast", `{"receiverId":"receiver-4k"`+example.extra+`}`, "sender-ceiling", token, "")
		if w.Code != example.status {
			t.Fatal(w.Code, w.Body)
		}
		if w.Code != 201 {
			continue
		}
		var grant struct {
			CastID string
			Video  playback.CastVideo
		}
		if err := json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
			t.Fatal(err)
		}
		if grant.Video.MaxHeight != example.height {
			t.Fatal(grant)
		}
		if example.height == 2160 && (grant.Video.MaxWidth != 3840 || grant.Video.Bitrate != 12000000) {
			t.Fatal("unexpected 4K video parameters", grant.Video)
		}
		if w = call(s, "DELETE", "/v1/cast/"+grant.CastID, "", "sender-ceiling", token, ""); w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
}

func TestAudioCastNegotiatesWithoutVideoAndDeliversAudioPlan(t *testing.T) {
	s := testServer(t, nil, "")
	sender := pair(t, s, "audio-sender")
	receiver := pair(t, s, "audio-receiver")
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nsegment.ts\n")
	}))
	defer relay.Close()
	s.opt.RelayURL = relay.URL
	body := `{"receiverId":"audio-receiver","mode":"AUDIO"}`
	if w := call(s, "POST", "/v1/cast", body, "audio-sender", sender, ""); w.Code != 409 {
		t.Fatal("audio bypassed receiver consent", w.Code)
	}
	prefs := `{"mode":"TV","uiLanguage":"en","subtitleMode":"auto","allowCasting":true}`
	if w := call(s, "PUT", "/v1/device/preferences", prefs, "audio-receiver", receiver, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	for _, mode := range []string{"MEDIA", "", "audio"} {
		if w := call(s, "POST", "/v1/cast", `{"receiverId":"audio-receiver","mode":"`+mode+`"}`, "audio-sender", sender, ""); w.Code != 400 {
			t.Fatal("invalid mode accepted", mode, w.Code)
		}
	}
	w := call(s, "POST", "/v1/cast", body, "audio-sender", sender, "")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var grant struct {
		CastID, Mode string
		Audio        struct {
			Codec                         string
			Channels, SampleRate, Bitrate int
		}
		Video json.RawMessage
	}
	if err := json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.Mode != "AUDIO" || len(grant.Video) != 0 || grant.Audio.Codec != "aac" || grant.Audio.Channels != 2 || grant.Audio.SampleRate != 44100 || grant.Audio.Bitrate != 128000 {
		t.Fatal("wrong audio contract", w.Body)
	}
	path := "/v1/cast/" + grant.CastID
	if w := call(s, "POST", path+"/ready", "", "audio-sender", sender, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	var active struct{ Plan domain.Plan }
	if err := json.Unmarshal(call(s, "GET", "/v1/cast/active", "", "audio-receiver", receiver, "").Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if active.Plan.Item.Kind != "audio" || active.Plan.Item.Title != "Audio sharing" || !active.Plan.Live {
		t.Fatal(active.Plan)
	}
	if w := call(s, "DELETE", path, "", "audio-sender", sender, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := call(s, "GET", active.Plan.URL, "", "", "", ""); w.Code != 401 {
		t.Fatal("audio stream survived revocation", w.Code)
	}
}

func TestCastServerGoodputGrantFreshVsStaleAndAudioUnaffected(t *testing.T) {
	s := testServer(t, nil, "")
	sender := pair(t, s, "sender-goodput")
	receiver := pair(t, s, "receiver-goodput")
	call(s, "GET", "/v1/device", "", "receiver-goodput", receiver, "")
	s.opt.RelayURL = "http://127.0.0.1:8888"

	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "receiver-goodput", &dev); err != nil {
		t.Fatal(err)
	}
	dev.Preferences.AllowCasting = true
	dev.Capabilities = domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     devices.ProbeCacheKey(dev),
		Probes: []domain.Probe{
			{ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		},
	}
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}

	requestGrant := func(body string) (*httptest.ResponseRecorder, string, playback.CastVideo) {
		w := call(s, "POST", "/v1/cast", body, "sender-goodput", sender, "")
		if w.Code != 201 {
			return w, "", playback.CastVideo{}
		}
		var grant struct {
			CastID string             `json:"castId"`
			Video  playback.CastVideo `json:"video"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
			t.Fatal(err)
		}
		return w, grant.CastID, grant.Video
	}

	deleteGrant := func(id string) {
		if id != "" {
			if w := call(s, "DELETE", "/v1/cast/"+id, "", "sender-goodput", sender, ""); w.Code != 200 {
				t.Fatalf("failed to delete cast %s: %d", id, w.Code)
			}
		}
	}

	// 1. Fresh sample with 3000 kbps: caps 1080p request to 720p
	s.mu.Lock()
	s.networkSamples["receiver-goodput"] = networkSample{measured: time.Now(), kbps: 3000}
	s.mu.Unlock()
	w, id, video := requestGrant(`{"receiverId":"receiver-goodput","maxVideoHeight":1080}`)
	if w.Code != 201 || video.MaxHeight != 720 || video.Bitrate != 2000000 {
		t.Fatalf("expected 720p cap with fresh 3000 kbps sample, got code %d, video: %v", w.Code, video)
	}
	deleteGrant(id)

	// 2. Fresh sample with 1800 kbps: caps 1080p request to 480p
	s.mu.Lock()
	s.networkSamples["receiver-goodput"] = networkSample{measured: time.Now(), kbps: 1800}
	s.mu.Unlock()
	w, id, video = requestGrant(`{"receiverId":"receiver-goodput","maxVideoHeight":1080}`)
	if w.Code != 201 || video.MaxHeight != 480 || video.Bitrate != 1200000 {
		t.Fatalf("expected 480p cap with fresh 1800 kbps sample, got code %d, video: %v", w.Code, video)
	}
	deleteGrant(id)

	// 3. Fresh sample with insufficient goodput (< 800 kbps budget): safe rejection with 409 receiver_media_unsupported
	for _, lowKbps := range []int64{1000, 500, 100} {
		s.mu.Lock()
		s.networkSamples["receiver-goodput"] = networkSample{measured: time.Now(), kbps: lowKbps}
		s.mu.Unlock()
		w, _, _ = requestGrant(`{"receiverId":"receiver-goodput","maxVideoHeight":1080}`)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "receiver_media_unsupported") {
			t.Fatalf("expected 409 receiver_media_unsupported for low goodput %d kbps, got code %d, body: %s", lowKbps, w.Code, w.Body.String())
		}
	}

	// 4. Stale sample (> 5 minutes old): ignored, preserves decoder ceiling (1080p)
	s.mu.Lock()
	s.networkSamples["receiver-goodput"] = networkSample{measured: time.Now().Add(-6 * time.Minute), kbps: 500}
	s.mu.Unlock()
	w, id, video = requestGrant(`{"receiverId":"receiver-goodput","maxVideoHeight":1080}`)
	if w.Code != 201 || video.MaxHeight != 1080 || video.Bitrate != 4000000 {
		t.Fatalf("expected stale sample to preserve 1080p, got code %d, video: %v", w.Code, video)
	}
	deleteGrant(id)

	// 5. Unknown/absent sample: ignored, preserves decoder ceiling (1080p)
	s.mu.Lock()
	delete(s.networkSamples, "receiver-goodput")
	s.mu.Unlock()
	w, id, video = requestGrant(`{"receiverId":"receiver-goodput","maxVideoHeight":1080}`)
	if w.Code != 201 || video.MaxHeight != 1080 || video.Bitrate != 4000000 {
		t.Fatalf("expected absent sample to preserve 1080p, got code %d, video: %v", w.Code, video)
	}
	deleteGrant(id)

	// 6. AUDIO mode: unaffected even when fresh goodput is low (500 kbps which rejects SCREEN)
	s.mu.Lock()
	s.networkSamples["receiver-goodput"] = networkSample{measured: time.Now(), kbps: 500}
	s.mu.Unlock()
	wAudio := call(s, "POST", "/v1/cast", `{"receiverId":"receiver-goodput","mode":"AUDIO"}`, "sender-goodput", sender, "")
	if wAudio.Code != 201 {
		t.Fatalf("expected 201 for AUDIO mode despite low goodput, got %d, body: %s", wAudio.Code, wAudio.Body.String())
	}
	var audioGrant struct {
		CastID string `json:"castId"`
		Mode   string `json:"mode"`
		Audio  struct {
			Codec   string `json:"codec"`
			Bitrate int    `json:"bitrate"`
		} `json:"audio"`
		Video json.RawMessage `json:"video"`
	}
	if err := json.Unmarshal(wAudio.Body.Bytes(), &audioGrant); err != nil {
		t.Fatal(err)
	}
	if audioGrant.Mode != "AUDIO" || audioGrant.Audio.Codec != "aac" || audioGrant.Audio.Bitrate != 128000 || len(audioGrant.Video) != 0 {
		t.Fatalf("unexpected audio grant: %+v", audioGrant)
	}
	deleteGrant(audioGrant.CastID)
}
