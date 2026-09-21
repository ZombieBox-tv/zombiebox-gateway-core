package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

type receiverFixture struct {
	state  domain.YouTubeReceiverState
	closed bool
	acks   int
}

func (f *receiverFixture) OpenReceiver(_ context.Context, _ domain.Config, id string) (domain.YouTubeReceiverState, error) {
	f.state.ReceiverID = id
	return f.state, nil
}
func (f *receiverFixture) PollReceiver(context.Context, domain.Config, string) (domain.YouTubeReceiverState, error) {
	return f.state, nil
}
func (f *receiverFixture) AcknowledgeReceiver(context.Context, domain.Config, string, domain.ReceiverAcknowledgement) error {
	f.acks++
	return nil
}
func (f *receiverFixture) CloseReceiver(context.Context, domain.Config, string) error {
	f.closed = true
	return nil
}

type receiverResolver struct{}

func (receiverResolver) Resolve(_ context.Context, s domain.Source) (domain.Source, error) {
	s.URL = "https://fixture.invalid/video.mp4"
	s.MIME = "video/mp4"
	return s, nil
}
func TestYouTubeReceiverOwnsCommandsAndUsesNormalPlayback(t *testing.T) {
	s := testServer(t, nil, "")
	fixture := &receiverFixture{state: domain.YouTubeReceiverState{State: "READY", TVCode: "123 456 789"}}
	s.deps.YouTubeReceiver = fixture
	s.deps.Resolver = receiverResolver{}
	for _, id := range []string{"youtube", "youtube_receiver"} {
		s.SeedProviders(t.Context(), map[string]domain.Config{id: {Enabled: true, URL: "https://private.invalid", Token: strings.Repeat("private", 6)}})
	}
	owner := pair(t, s, "owner-device")
	other := pair(t, s, "other-device")
	opened := call(s, "POST", "/v1/youtube/receiver", `{}`, "owner-device", owner, "")
	if opened.Code != 201 {
		t.Fatal(opened.Code, opened.Body)
	}
	var state domain.YouTubeReceiverState
	json.Unmarshal(opened.Body.Bytes(), &state)
	path := "/v1/youtube/receiver/" + state.ReceiverID
	if w := call(s, "POST", "/v1/youtube/receiver", `{}`, "other-device", other, ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := call(s, "GET", path, "", "other-device", other, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	fixture.state.Command = &domain.ReceiverCommand{ID: strings.Repeat("a", 32), Action: "play", VideoID: "abcdefghijk"}
	w := call(s, "GET", path, "", "owner-device", owner, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "youtube-abcdefghijk") {
		t.Fatal(w.Code, w.Body)
	}
	w = call(s, "POST", "/v1/playback", `{"itemId":"youtube-abcdefghijk"}`, "owner-device", owner, "")
	if w.Code != 201 || strings.Contains(w.Body.String(), "private.invalid") || strings.Contains(w.Body.String(), "privateprivate") {
		t.Fatal(w.Code, w.Body)
	}
	var plan domain.Plan
	json.Unmarshal(w.Body.Bytes(), &plan)
	if !strings.HasPrefix(plan.URL, "/v1/streams/") {
		t.Fatal(plan)
	}
	feedback := `{"commandId":"` + strings.Repeat("a", 32) + `","success":true,"state":"PLAYING","positionMs":1000,"durationMs":10000,"volume":100,"muted":false}`
	if w = call(s, "POST", path+"/state", feedback, "other-device", other, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w = call(s, "POST", path+"/state", feedback, "owner-device", owner, ""); w.Code != 200 || fixture.acks != 1 {
		t.Fatal(w.Code, w.Body)
	}
	if w = call(s, "DELETE", path, "", "owner-device", owner, ""); w.Code != 200 || !fixture.closed {
		t.Fatal(w.Code)
	}
	if s.youTubeReceiverSource("owner-device", "youtube-abcdefghijk") != nil {
		t.Fatal("receiver source survived disable")
	}
}
func TestReceiverRejectsInvalidCommands(t *testing.T) {
	for _, command := range []*domain.ReceiverCommand{{ID: strings.Repeat("a", 32), Action: "play", VideoID: "https://localhost"}, {ID: "bad", Action: "stop"}, {ID: strings.Repeat("a", 32), Action: "exec"}} {
		if validReceiverState(domain.YouTubeReceiverState{ReceiverID: "owner", State: "READY", Command: command}, "owner") {
			t.Fatal("invalid command accepted")
		}
	}
}
