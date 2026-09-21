package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/store"
)

func testServer(t *testing.T, db *store.Store, media string) *Server {
	t.Helper()
	if db == nil {
		var err error
		db, err = store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
	}
	s := newTestServer(db, Options{PairingCode: "123456", MediaDir: media, PollWait: 10 * time.Millisecond})
	t.Cleanup(s.Close)
	return s
}
func call(s *Server, method, path, body, id, token, admin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Zombie-Device", id)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Zombie-Admin-Code", admin)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func pair(t *testing.T, s *Server, id string) string {
	t.Helper()
	w := call(s, "POST", "/v1/devices/register", `{"clientVersion":"test","protocolVersion":1,"installationId":"`+id+`","pairingCode":"123456","platform":{"androidApi":13},"display":{"dpad":true}}`, "", "", "")
	if w.Code != 201 {
		t.Fatalf("pair: %d %s", w.Code, w.Body)
	}
	var body struct{ DeviceToken string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.DeviceToken
}
func TestPairingRotationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "gateway.db")
	db, e := store.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s := testServer(t, db, "")
	token := pair(t, s, "test-device")
	prefs := `{"mode":"DOCKED","uiLanguage":"es","audioLanguages":["es"],"subtitleLanguages":["en"],"subtitleMode":"auto"}`
	if w := call(s, "PUT", "/v1/device/preferences", prefs, "test-device", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	next := pair(t, s, "test-device")
	if w := call(s, "GET", "/v1/device", "", "test-device", token, ""); w.Code != 401 {
		t.Fatal("old token accepted")
	}
	s.Close()
	db.Close()
	db, e = store.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s = testServer(t, db, "")
	w := call(s, "GET", "/v1/device", "", "test-device", next, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"uiLanguage":"es"`) {
		t.Fatal(w.Body)
	}
	if strings.Contains(w.Body.String(), "123456") || strings.Contains(w.Body.String(), next) {
		t.Fatal("device response leaked secrets")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("database must be private")
	}
}
func TestProviderWritesRequireAdminAndRedactSecrets(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "test-device")
	body := `{"enabled":true,"url":"http://example.test/list?key=secret-url","token":"secret-token"}`
	if w := call(s, "PUT", "/v1/providers/iptv", body, "test-device", token, ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := call(s, "PUT", "/v1/providers/iptv", body, "test-device", token, "123456"); w.Code != 200 {
		t.Fatal(w.Body)
	}
	for _, path := range []string{"/v1/providers", "/v1/modules", "/v1/device", "/v1/events?wait=0"} {
		w := call(s, "GET", path, "", "test-device", token, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "secret-") {
			t.Fatalf("%s: %s", path, w.Body)
		}
	}
	if w := call(s, "PUT", "/v1/providers/iptv", `{"playlistPath":"/etc/passwd"}`, "test-device", token, "123456"); w.Code != 400 {
		t.Fatal("client can open server paths")
	}
	if w := call(s, "PUT", "/v1/providers/iptv", `{"enabled":false}`, "test-device", token, "123456"); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if c := s.config(context.Background(), "iptv"); c.Token != "secret-token" || c.Enabled {
		t.Fatal("partial update lost token")
	}
	if w := call(s, "PUT", "/v1/providers/iptv", `{"token":""}`, "test-device", token, "123456"); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if s.config(context.Background(), "iptv").Token != "" {
		t.Fatal("token not removed")
	}
	if e := s.SeedProviders(context.Background(), map[string]providers.Config{"iptv": {Enabled: false}}); e != nil {
		t.Fatal(e)
	}
	if w := call(s, "PUT", "/v1/providers/iptv", body, "test-device", token, "123456"); w.Code != 409 {
		t.Fatal("server-managed provider changed")
	}
}
func TestAdminRateLimit(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "test-device")
	for i := 0; i < 5; i++ {
		if w := call(s, "PUT", "/v1/providers/iptv", `{}`, "test-device", token, "bad"); w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if w := call(s, "PUT", "/v1/providers/iptv", `{}`, "test-device", token, "123456"); w.Code != 429 {
		t.Fatal("unbounded administration guesses")
	}
}
func TestLocalPlaybackRangeResumeAndIsolation(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "Sample.mp4"), []byte("0123456789"), 0600); e != nil {
		t.Fatal(e)
	}
	s := testServer(t, nil, dir)
	token := pair(t, s, "first-device")
	other := pair(t, s, "other-device")
	home := call(s, "GET", "/v1/home", "", "first-device", token, "")
	var screen domain.Screen
	if e := json.Unmarshal(home.Body.Bytes(), &screen); e != nil || screen.Hero == nil {
		t.Fatal(home.Body)
	}
	w := call(s, "POST", "/v1/playback", `{"itemId":"`+screen.Hero.Item.ID+`"}`, "first-device", token, "")
	var plan domain.Plan
	if e := json.Unmarshal(w.Body.Bytes(), &plan); w.Code != 201 || e != nil {
		t.Fatal(w.Body)
	}
	r := httptest.NewRequest("GET", plan.URL, nil)
	r.Header.Set("Range", "bytes=2-5")
	part := httptest.NewRecorder()
	s.ServeHTTP(part, r)
	if part.Code != 206 || part.Body.String() != "2345" {
		t.Fatalf("range: %d %s", part.Code, part.Body)
	}
	path := "/v1/playback/" + plan.SessionID
	if w := call(s, "PUT", path+"/progress", `{"positionMs":2300,"durationMs":10000,"state":"PAUSED"}`, "other-device", other, ""); w.Code != 404 {
		t.Fatal("cross-device session update")
	}
	if w := call(s, "PUT", path+"/progress", `{"positionMs":2300,"durationMs":10000,"state":"PAUSED"}`, "first-device", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := call(s, "DELETE", path, "", "first-device", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := call(s, "GET", plan.URL, "", "", "", ""); w.Code != 401 {
		t.Fatal("stopped stream ticket accepted")
	}
	w = call(s, "POST", "/v1/playback", `{"itemId":"`+screen.Hero.Item.ID+`"}`, "first-device", token, "")
	if e := json.Unmarshal(w.Body.Bytes(), &plan); e != nil || plan.ResumeMS != 2300 {
		t.Fatal(w.Body)
	}
}
func TestHLSRelaysURLsAndKeyWithoutLeakingCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/list":
			io.WriteString(w, "#EXTM3U\n#EXTINF:-1,Test TV\nhttp://"+r.Host+"/live.m3u8?secret=subscription\n")
		case "/live.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"/key?secret=key\"\n#EXTINF:5,\n/segment.ts?secret=segment\n")
		case "/key":
			io.WriteString(w, "0123456789abcdef")
		case "/segment.ts":
			io.WriteString(w, "video-bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	s := testServer(t, nil, "")
	if e := s.SeedProviders(context.Background(), map[string]providers.Config{"iptv": {Enabled: true, URL: upstream.URL + "/list"}}); e != nil {
		t.Fatal(e)
	}
	token := pair(t, s, "test-device")
	w := call(s, "GET", "/v1/home", "", "test-device", token, "")
	var screen domain.Screen
	if e := json.Unmarshal(w.Body.Bytes(), &screen); e != nil || screen.Hero == nil {
		t.Fatal(w.Body)
	}
	w = call(s, "POST", "/v1/playback", `{"itemId":"`+screen.Hero.Item.ID+`"}`, "test-device", token, "")
	var plan domain.Plan
	if e := json.Unmarshal(w.Body.Bytes(), &plan); e != nil || w.Code != 201 {
		t.Fatal(w.Body)
	}
	w = call(s, "GET", plan.URL, "", "", "", "")
	body := w.Body.String()
	if w.Code != 200 || strings.Contains(body, "secret=") || strings.Contains(body, upstream.URL) {
		t.Fatal(body)
	}
	key := hlsURI.FindStringSubmatch(body)
	if len(key) != 2 {
		t.Fatal(body)
	}
	if w := call(s, "GET", key[1], "", "", "", ""); w.Body.String() != "0123456789abcdef" {
		t.Fatal(w.Body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "/v1/streams/") {
			if w := call(s, "GET", line, "", "", "", ""); w.Body.String() != "video-bytes" {
				t.Fatal(w.Body)
			}
		}
	}
}
func TestEventsIsolationAndExpiredCursor(t *testing.T) {
	events := newEvents()
	_, cursor, _, _ := events.read("first-device", "")
	events.publish("other-device", "private", nil)
	events.publish("", "modules.changed", nil)
	result, next, expired, _ := events.read("first-device", cursor)
	if expired || len(result) != 1 || result[0].Type != "modules.changed" || next == cursor {
		t.Fatal(result)
	}
	for i := 0; i < 260; i++ {
		events.publish("", "tick", nil)
	}
	if _, _, expired, _ := events.read("first-device", cursor); !expired {
		t.Fatal("expired cursor retained")
	}
}

func TestDisableDuringCatalogFetchCannotRestoreOldSources(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-finish
		io.WriteString(w, "#EXTM3U\n#EXTINF:-1,Removed\nhttp://example.test/live.ts\n")
	}))
	defer upstream.Close()
	s := testServer(t, nil, "")
	token := pair(t, s, "test-device")
	body := `{"enabled":true,"url":"` + upstream.URL + `"}`
	if w := call(s, "PUT", "/v1/providers/iptv", body, "test-device", token, "123456"); w.Code != 200 {
		t.Fatal(w.Body)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.catalog(context.Background()) }()
	<-entered
	if w := call(s, "PUT", "/v1/providers/iptv", `{"enabled":false}`, "test-device", token, "123456"); w.Code != 200 {
		t.Fatal(w.Body)
	}
	close(finish)
	<-done
	if sources := s.catalog(context.Background()); len(sources) != 0 {
		t.Fatal("disabled provider reappeared")
	}
}

func TestStoppingSessionCancelsActiveUpstream(t *testing.T) {
	streaming := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/list" {
			io.WriteString(w, "#EXTM3U\n#EXTINF:-1,Test\nhttp://"+r.Host+"/live.ts\n")
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(streaming)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	s := testServer(t, nil, "")
	s.SeedProviders(context.Background(), map[string]providers.Config{"iptv": {Enabled: true, URL: upstream.URL + "/list"}})
	token := pair(t, s, "test-device")
	sources := s.catalog(context.Background())
	w := call(s, "POST", "/v1/playback", `{"itemId":"`+sources[0].Item.ID+`"}`, "test-device", token, "")
	var plan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &plan)
	finished := make(chan struct{})
	go func() { defer close(finished); call(s, "GET", plan.URL, "", "", "", "") }()
	<-streaming
	if w := call(s, "DELETE", "/v1/playback/"+plan.SessionID, "", "test-device", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream did not cancel")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("relay did not stop")
	}
}

func TestSlowOptionalProviderCannotHideHealthyCatalog(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<MediaContainer><Video ratingKey="1" title="Available movie"><Media><Part key="/movie.mp4"/></Media></Video></MediaContainer>`)
	}))
	defer healthy.Close()
	s := testServer(t, nil, "")
	s.opt.CatalogWait = 100 * time.Millisecond
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"iptv": {Enabled: true, URL: slow.URL}, "plex": {Enabled: true, URL: healthy.URL, Token: "fixture"}}); err != nil {
		t.Fatal(err)
	}
	sources := s.catalog(context.Background())
	if len(sources) != 1 || sources[0].Item.Title != "Available movie" {
		t.Fatalf("slow optional service hid a working catalog: %+v", sources)
	}
}
