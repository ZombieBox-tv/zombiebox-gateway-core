package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/companion"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/playback"
)

func TestCompanionQRConsentAndCredentialSeparation(t *testing.T) {
	s := testServer(t, nil, "")
	tv := pair(t, s, "paired-television")
	w := call(s, "POST", "/v1/device/companions/invitations", `{"gateway":"http://192.168.1.2:8090"}`, "paired-television", tv, "")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var inv struct {
		Code  string
		QrPng []byte
	}
	if json.Unmarshal(w.Body.Bytes(), &inv) != nil || len(inv.QrPng) < 100 || string(inv.QrPng[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("invalid PNG")
	}
	if strings.Contains(w.Body.String(), tv) {
		t.Fatal("token leak")
	}
	w = call(s, "POST", "/v1/companion/join", `{"code":"`+inv.Code+`","name":"Daily phone"}`, "", "", "")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var joined struct {
		Request companion.Request
		Token   string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &joined)
	if w = call(s, "GET", "/v1/companion/status", "", joined.Request.ID, joined.Token, ""); w.Code != 401 {
		t.Fatal("pending authorized")
	}
	if w = call(s, "POST", "/v1/device/companions/"+joined.Request.ID+"/decision", `{"accept":true}`, "paired-television", tv, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = call(s, "GET", "/v1/companion/status", "", joined.Request.ID, joined.Token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	for _, path := range []string{"/v1/device", "/v1/providers", "/v1/cast/receivers"} {
		if w = call(s, "GET", path, "", joined.Request.ID, joined.Token, ""); w.Code != 401 {
			t.Fatal("privilege escalation", path)
		}
	}
	if w = call(s, "POST", "/v1/device/remote/poll", `{"active":true}`, "paired-television", tv, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = call(s, "POST", "/v1/companion/commands", `{"action":"PROVIDER","provider":"youtube"}`, joined.Request.ID, joined.Token, ""); w.Code != 202 {
		t.Fatal(w.Body)
	}
	if w = call(s, "DELETE", "/v1/device/companions/"+joined.Request.ID, "", "paired-television", tv, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = call(s, "GET", "/v1/companion/status", "", joined.Request.ID, joined.Token, ""); w.Code != 401 {
		t.Fatal("revocation failed")
	}
}

func TestCompanionCastCannotChooseAnotherTargetAndRevokeRetiresLease(t *testing.T) {
	s := testServer(t, nil, "")
	tv := pair(t, s, "paired-television")
	other := pair(t, s, "other-television")
	s.opt.RelayURL = "http://127.0.0.1:8888"
	s.opt.RelayAdminToken = strings.Repeat("a", 32)
	for id, token := range map[string]string{"paired-television": tv, "other-television": other} {
		w := call(s, "PUT", "/v1/device/preferences", `{"mode":"TV","uiLanguage":"en","subtitleMode":"auto","allowCasting":true}`, id, token, "")
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	inv, err := s.companions.Invite(context.Background(), "paired-television", "TV")
	if err != nil {
		t.Fatal(err)
	}
	request, token, err := s.companions.Join(context.Background(), "127.0.0.1", companion.Join{Code: inv.Code, Name: "Phone"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.companions.Decide(context.Background(), "paired-television", "TV", request.ID, true); err != nil {
		t.Fatal(err)
	}
	var target domain.Device
	if err = s.db.Get(t.Context(), "devices", "paired-television", &target); err != nil {
		t.Fatal(err)
	}
	target.Capabilities = domain.Capabilities{SuiteVersion: 2, CacheKey: "bound", Probes: []domain.Probe{
		{ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		{ID: "hls", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
	}}
	if err = s.db.Put(t.Context(), "devices", target.ID, target); err != nil {
		t.Fatal(err)
	}
	w := call(s, "POST", "/v1/companion/cast", `{"receiverId":"other-television","maxVideoHeight":1080}`, request.ID, token, "")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var grant struct {
		CastID string
		Video  playback.CastVideo
	}
	if err = json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.Video.MaxHeight != 1080 {
		t.Fatal("companion video limit lost", grant)
	}
	s.mu.Lock()
	receiver := s.casts[grant.CastID].receiver
	s.mu.Unlock()
	if receiver != "paired-television" {
		t.Fatal("cross-target Cast", receiver)
	}
	if stopped := call(s, "DELETE", "/v1/companion/cast/"+grant.CastID, "", request.ID, token, ""); stopped.Code != 200 {
		t.Fatal(stopped.Body)
	}
	w = call(s, "POST", "/v1/companion/cast", `{"receiverId":"other-television","mode":"AUDIO"}`, request.ID, token, "")
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"mode":"AUDIO"`) {
		t.Fatal("audio mode lost", w.Code, w.Body)
	}
	if err = json.Unmarshal(w.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	receiver = s.casts[grant.CastID].receiver
	s.mu.Unlock()
	if receiver != "paired-television" {
		t.Fatal("audio escaped approved target")
	}
	w = call(s, "DELETE", "/v1/device/companions/"+request.ID, "", "other-television", other, "")
	if w.Code != 403 {
		t.Fatal("foreign revocation", w.Code)
	}
	w = call(s, "DELETE", "/v1/companion/session", "", request.ID, token, "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	s.mu.Lock()
	_, present := s.casts[grant.CastID]
	s.mu.Unlock()
	if present {
		t.Fatal("revoked companion retained relay")
	}
	w = call(s, "PUT", "/v1/companion/cast/"+grant.CastID, "", request.ID, token, "")
	if w.Code != 401 {
		t.Fatal("revoked lease renewed")
	}
}

func TestNetworkPairingAndTextRoutes(t *testing.T) {
	s := testServer(t, nil, "")
	id := "c3b32045-a431-457c-9ba0-32a5464f43ec"
	tv := pair(t, s, id)
	lease := strings.Repeat("e", 32)
	w := call(s, "POST", "/v1/device/remote/poll", `{"active":true,"inputId":"`+lease+`"}`, id, tv, "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = call(s, "GET", "/v1/companion/targets", "", "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), id) || strings.Contains(w.Body.String(), tv) {
		t.Fatal(w.Code, w.Body)
	}
	w = call(s, "POST", "/v1/companion/join", `{"name":"Phone","targetId":"`+id+`","clientKey":"`+strings.Repeat("b", 64)+`"}`, "", "", "")
	var result struct {
		Request companion.Request
		Token   string
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Request.State != "PENDING" {
		t.Fatal(w.Code, w.Body)
	}
	w = call(s, "POST", "/v1/device/companions/"+result.Request.ID+"/decision", `{"accept":true}`, id, tv, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = call(s, "POST", "/v1/companion/commands", `{"action":"TEXT","text":"movie night","inputId":"`+lease+`"}`, result.Request.ID, result.Token, "")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	w = call(s, "POST", "/v1/device/remote/poll", `{"active":true,"inputId":"`+lease+`"}`, id, tv, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "movie night") {
		t.Fatal(w.Code, w.Body)
	}
}
