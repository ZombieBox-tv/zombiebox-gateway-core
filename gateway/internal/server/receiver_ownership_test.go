package server

import (
	"testing"
	"time"
)

func TestReceiverClaimsExcludeOtherTransports(t *testing.T) {
	s := testServer(t, nil, "")
	receiver := pair(t, s, "receiver-owner")
	sender := pair(t, s, "sender-owner")
	s.opt.RelayURL = "http://relay.invalid"
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"spotify"}`, "receiver-owner", receiver, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := call(s, "POST", "/v1/youtube/receiver", `{}`, "receiver-owner", receiver, ""); w.Code != 409 {
		t.Fatal("YouTube stole an armed media receiver", w.Code)
	}
	if w := call(s, "POST", "/v1/cast", `{"receiverId":"receiver-owner"}`, "sender-owner", sender, ""); w.Code != 409 {
		t.Fatal("Cast stole an armed media receiver", w.Code)
	}
	call(s, "DELETE", "/v1/media-receiver", "", "receiver-owner", receiver, "")
	s.mu.Lock()
	s.casts["active"] = &castSession{id: "active", receiver: "receiver-owner", expires: time.Now().Add(time.Minute)}
	s.mu.Unlock()
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "receiver-owner", receiver, ""); w.Code != 409 {
		t.Fatal("AirPlay stole a Cast receiver", w.Code)
	}
	s.mu.Lock()
	delete(s.casts, "active")
	s.mu.Unlock()
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "receiver-owner", receiver, ""); w.Code != 200 {
		t.Fatal("released receiver did not become available", w.Body)
	}
}
