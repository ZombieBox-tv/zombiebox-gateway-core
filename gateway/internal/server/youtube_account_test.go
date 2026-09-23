package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/youtubeaccount"
)

func TestAccountAuthorizationAndSubscriptionsOpenScopedBrowse(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			io.WriteString(w, `{"device_code":"private-code","user_code":"TEST-CODE","verification_url":"https://www.google.com/device","expires_in":300,"interval":5}`)
		case "/token":
			io.WriteString(w, `{"access_token":"private-access","refresh_token":"private-refresh","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.readonly"}`)
		case "/v3/subscriptions":
			if r.Header.Get("Authorization") != "Bearer private-access" {
				t.Error("missing private account token")
			}
			io.WriteString(w, `{"items":[{"snippet":{"title":"Subscribed channel","resourceId":{"channelId":"UCabcdefghijklmnopqrstuv"}}}]}`)
		case "/browse":
			if r.URL.Query().Get("parent") != "channel:UCabcdefghijklmnopqrstuv" {
				t.Error("account path not retained server-side")
			}
			io.WriteString(w, `{"items":[{"id":"abcdefghijk","kind":"video","title":"Video"}],"nextOffset":-1}`)
		case "/revoke":
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	s := testServer(t, nil, "")
	s.youtubeAccount = youtubeaccount.New(s.db, upstream.Client(), "fixture-client", "", youtubeaccount.Endpoints{Device: upstream.URL + "/device", Token: upstream.URL + "/token", Revoke: upstream.URL + "/revoke", Data: upstream.URL + "/v3"}, func() time.Time { return clock })
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"youtube": {Enabled: true, URL: upstream.URL, Token: strings.Repeat("a", 32)}}); err != nil {
		t.Fatal(err)
	}
	owner := pair(t, s, "account-tv")
	other := pair(t, s, "other-tv")
	if w := call(s, "GET", "/v1/youtube/account", "", "", "", ""); w.Code != 401 {
		t.Fatal("unauthenticated status", w.Code)
	}
	started := call(s, "POST", "/v1/youtube/account/authorization", "", "account-tv", owner, "")
	if started.Code != 200 || !strings.Contains(started.Body.String(), "TEST-CODE") || strings.Contains(started.Body.String(), "private-code") {
		t.Fatal("unsafe authorization prompt", started.Code, started.Body)
	}
	if w := call(s, "POST", "/v1/youtube/account/authorization/poll", "", "account-tv", owner, ""); w.Code != 429 {
		t.Fatal("ignored provider poll interval", w.Code)
	}
	clock = clock.Add(5 * time.Second)
	if w := call(s, "POST", "/v1/youtube/account/authorization/poll", "", "account-tv", owner, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"connected":true`) {
		t.Fatal("account did not connect", w.Code, w.Body)
	}
	list := call(s, "GET", "/v1/youtube/account/subscriptions", "", "account-tv", owner, "")
	var page struct{ Items []domain.Item }
	if list.Code != 200 || json.Unmarshal(list.Body.Bytes(), &page) != nil || len(page.Items) != 1 || !strings.HasPrefix(page.Items[0].BrowseID, "browse-") || strings.Contains(list.Body.String(), "private-access") {
		t.Fatal("account list leaked or omitted data", list.Code, list.Body)
	}
	if w := call(s, "GET", "/v1/browse?provider=youtube&parent="+page.Items[0].BrowseID, "", "other-tv", other, ""); w.Code != 410 {
		t.Fatal("account browse ID escaped device scope", w.Code)
	}
	child := call(s, "GET", "/v1/browse?provider=youtube&parent="+page.Items[0].BrowseID, "", "account-tv", owner, "")
	if child.Code != 200 || !strings.Contains(child.Body.String(), "Video") {
		t.Fatal("account channel could not open", child.Code, child.Body)
	}
	if w := call(s, "DELETE", "/v1/youtube/account", "", "account-tv", owner, "wrong"); w.Code != 403 {
		t.Fatal("disconnect bypassed operator", w.Code)
	}
	if w := call(s, "DELETE", "/v1/youtube/account", "", "account-tv", owner, "123456"); w.Code != 200 {
		t.Fatal("disconnect", w.Code, w.Body)
	}
	if w := call(s, "GET", "/v1/youtube/account/subscriptions", "", "account-tv", owner, ""); w.Code != 409 {
		t.Fatal("disconnected account still readable", w.Code)
	}
}
