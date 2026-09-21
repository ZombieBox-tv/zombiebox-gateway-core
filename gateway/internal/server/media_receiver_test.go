package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/receivers/inbox"
)

func TestMediaReceiverRoutesOwnedSourceAndAuthorizesSpotifyControls(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) {
			t.Error("worker token absent")
		}
		switch r.URL.Path {
		case "/status":
			fmt.Fprint(w, `{"stopped":false,"track":{"name":"Song","artist_names":["Artist"]}}`)
		case "/audio":
			fmt.Fprint(w, "audio-fixture")
		case "/player/pause":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	s := testServer(t, nil, t.TempDir())
	s.SeedProviders(context.Background(), map[string]providers.Config{"spotify": {Enabled: true, URL: upstream.URL, Token: strings.Repeat("s", 32)}})
	token := pair(t, s, "media-owner")
	other := pair(t, s, "media-other")
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"spotify"}`, "media-owner", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "media-other", other, ""); w.Code != 409 {
		t.Fatal("foreign claim", w.Body)
	}
	active := call(s, "GET", "/v1/media-receiver", "", "media-owner", token, "")
	var snapshot inbox.Snapshot
	json.Unmarshal(active.Body.Bytes(), &snapshot)
	if active.Code != 200 || snapshot.Plan == nil || snapshot.Plan.Item.Title != "Song" || strings.Contains(active.Body.String(), upstream.URL) {
		t.Fatal(active.Body)
	}
	stream := call(s, "GET", snapshot.Plan.URL, "", "", "", "")
	if stream.Body.String() != "audio-fixture" {
		t.Fatal(stream.Body)
	}
	if w := call(s, "POST", "/v1/player/spotify", `{"action":"pause"}`, "media-owner", token, ""); w.Code != 200 {
		t.Fatal("owner control denied", w.Body)
	}
	if w := call(s, "POST", "/v1/player/spotify", `{"action":"pause"}`, "media-other", other, ""); w.Code != 403 {
		t.Fatal("foreign control accepted", w.Code)
	}
	if w := call(s, "DELETE", "/v1/playback/"+snapshot.Plan.SessionID, "", "media-owner", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := call(s, "GET", snapshot.Plan.URL, "", "", "", ""); w.Code != 401 {
		t.Fatal("released stream survived", w.Code)
	}
	blocked := call(s, "GET", "/v1/media-receiver", "", "media-owner", token, "")
	var result struct{ Plan *domain.Plan }
	json.Unmarshal(blocked.Body.Bytes(), &result)
	if result.Plan != nil {
		t.Fatal("dismissed source reopened")
	}
}
