package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	youtubereceiver "zombiebox.local/gateway/internal/receivers/youtube"
)

func configureReceiver(t *testing.T, s *Server) *receiverFixture {
	t.Helper()
	fixture := &receiverFixture{state: domain.YouTubeReceiverState{State: "READY", TVCode: "123 456 789"}}
	s.youtubeReceiver = youtubereceiver.New(fixture, s.config, func() string { return randomID(16) })
	s.deps.Resolver = receiverResolver{}
	for _, id := range []string{"youtube", "youtube_receiver"} {
		s.SeedProviders(t.Context(), map[string]domain.Config{id: {Enabled: true, URL: "https://private.invalid", Token: strings.Repeat("x", 32)}})
	}
	return fixture
}

func TestExplicitReceiverHandoffPreservesFailedDestination(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "handoff-device")
	fixture := configureReceiver(t, s)
	call(s, "PUT", "/v1/media-receiver", `{"provider":"auto"}`, "handoff-device", token, "")
	s.SeedProviders(t.Context(), map[string]domain.Config{"youtube_receiver": {Enabled: false}})
	w := call(s, "POST", "/v1/youtube/receiver", `{"replaceExisting":true}`, "handoff-device", token, "")
	if w.Code != 409 || !s.mediaReceiverInbox.Active("handoff-device") {
		t.Fatal("failed replacement lost media receiver", w.Code, w.Body)
	}
	s.SeedProviders(t.Context(), map[string]domain.Config{"youtube_receiver": {Enabled: true, URL: "https://private.invalid"}})
	w = call(s, "POST", "/v1/youtube/receiver", `{"replaceExisting":true}`, "handoff-device", token, "")
	if w.Code != 201 || s.mediaReceiverInbox.Active("handoff-device") {
		t.Fatal(w.Code, w.Body)
	}
	var state domain.YouTubeReceiverState
	json.Unmarshal(w.Body.Bytes(), &state)
	fixture.state.Command = &domain.ReceiverCommand{ID: strings.Repeat("a", 32), Action: "play", VideoID: "abcdefghijk"}
	call(s, "GET", "/v1/youtube/receiver/"+state.ReceiverID, "", "handoff-device", token, "")
	w = call(s, "POST", "/v1/playback", `{"itemId":"youtube-abcdefghijk"}`, "handoff-device", token, "")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var plan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &plan)
	w = call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay","replaceExisting":true}`, "handoff-device", token, "")
	if w.Code != 200 || !fixture.closed || s.youtubeReceiver.Active("handoff-device") {
		t.Fatal(w.Code, w.Body)
	}
	if w = call(s, "GET", "/v1/youtube/receiver/"+state.ReceiverID, "", "handoff-device", token, ""); w.Code != 404 {
		t.Fatal("revoked lease remained usable", w.Code)
	}
	s.mu.Lock()
	_, exists := s.sessions[plan.SessionID]
	s.mu.Unlock()
	if exists {
		t.Fatal("revoked receiver stream survived handoff")
	}
}

func TestCastHandoffWaitsForReadyAndRechecksConsent(t *testing.T) {
	s := testServer(t, nil, "")
	sender := pair(t, s, "cast-handoff-sender")
	receiver := pair(t, s, "cast-handoff-receiver")
	var ready atomic.Bool
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nsegment.ts\n"))
	}))
	defer relay.Close()
	s.opt.RelayURL = relay.URL
	prefs := func(enabled bool) {
		body := `{"mode":"TV","uiLanguage":"en","subtitleMode":"auto","allowCasting":true,"allowReceiverHandoff":` + map[bool]string{true: "true", false: "false"}[enabled] + `}`
		if w := call(s, "PUT", "/v1/device/preferences", body, "cast-handoff-receiver", receiver, ""); w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	prefs(false)
	call(s, "PUT", "/v1/media-receiver", `{"provider":"auto"}`, "cast-handoff-receiver", receiver, "")
	create := func() *httptest.ResponseRecorder {
		return call(s, "POST", "/v1/cast", `{"receiverId":"cast-handoff-receiver","replaceExisting":true}`, "cast-handoff-sender", sender, "")
	}
	if w := create(); w.Code != 409 {
		t.Fatal("missing target consent", w.Code)
	}
	prefs(true)
	w := create()
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var grant struct{ CastID string }
	json.Unmarshal(w.Body.Bytes(), &grant)
	path := "/v1/cast/" + grant.CastID + "/ready"
	if w = call(s, "POST", path, "", "cast-handoff-sender", sender, ""); w.Code != 503 || !s.mediaReceiverInbox.Active("cast-handoff-receiver") {
		t.Fatal("unready cast stole receiver", w.Code)
	}
	ready.Store(true)
	prefs(false)
	if w = call(s, "POST", path, "", "cast-handoff-sender", sender, ""); w.Code != 409 || !s.mediaReceiverInbox.Active("cast-handoff-receiver") {
		t.Fatal("revoked consent ignored", w.Code)
	}
	prefs(true)
	if w = call(s, "POST", path, "", "cast-handoff-sender", sender, ""); w.Code != 200 || s.mediaReceiverInbox.Active("cast-handoff-receiver") {
		t.Fatal(w.Code, w.Body)
	}
	active := call(s, "GET", "/v1/cast/active", "", "cast-handoff-receiver", receiver, "")
	if strings.Contains(active.Body.String(), `"plan":null`) {
		t.Fatal(active.Body)
	}
	// Explicit target selection can retire Cast without granting sender control of another screen.
	if w = call(s, "PUT", "/v1/media-receiver", `{"provider":"spotify","replaceExisting":true}`, "cast-handoff-receiver", receiver, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = call(s, "PUT", "/v1/cast/"+grant.CastID, "", "cast-handoff-sender", sender, ""); w.Code != 404 {
		t.Fatal("old sender lease survived", w.Code)
	}
}

type delayedReceiverResolver struct{ entered, proceed chan struct{} }

func (r delayedReceiverResolver) Resolve(ctx context.Context, source domain.Source) (domain.Source, error) {
	close(r.entered)
	<-r.proceed
	return (receiverResolver{}).Resolve(ctx, source)
}

func TestHandoffRejectsPlaybackResolvedForAnOldYouTubeLease(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "late-receiver")
	fixture := configureReceiver(t, s)
	opened := call(s, "POST", "/v1/youtube/receiver", `{}`, "late-receiver", token, "")
	var state domain.YouTubeReceiverState
	json.Unmarshal(opened.Body.Bytes(), &state)
	fixture.state.Command = &domain.ReceiverCommand{ID: strings.Repeat("a", 32), Action: "play", VideoID: "abcdefghijk"}
	call(s, "GET", "/v1/youtube/receiver/"+state.ReceiverID, "", "late-receiver", token, "")
	resolver := delayedReceiverResolver{make(chan struct{}), make(chan struct{})}
	s.deps.Resolver = resolver
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- call(s, "POST", "/v1/playback", `{"itemId":"youtube-abcdefghijk","receiverId":"`+state.ReceiverID+`"}`, "late-receiver", token, "")
	}()
	<-resolver.entered
	switched := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay","replaceExisting":true}`, "late-receiver", token, "")
	close(resolver.proceed)
	w := <-done
	if switched.Code != 200 || w.Code != 409 {
		t.Fatal("late receiver resolution survived replacement", switched.Code, w.Code)
	}
	if len(s.sessions) != 0 {
		t.Fatal("late playback leaked a session")
	}
}
