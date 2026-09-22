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
	receiver.Capabilities = domain.Capabilities{SuiteVersion: 2, CacheKey: "bound", Probes: []domain.Probe{
		{ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		{ID: "hls", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
	}}
	if err := s.db.Put(t.Context(), "devices", receiver.ID, receiver); err != nil {
		t.Fatal(err)
	}
	for _, example := range []struct {
		extra          string
		status, height int
	}{
		{"", 201, 360}, {`,"maxVideoHeight":720`, 201, 360},
		{`,"maxVideoHeight":1080`, 201, 1080}, {`,"maxVideoHeight":2160`, 400, 0}, {`,"maxVideoHeight":0`, 400, 0},
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
}
