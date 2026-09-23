package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestAirPlayVideoToAudioReplacesOwnedStreamAndRetiresOldTicket(t *testing.T) {
	var mode atomic.Int32
	mode.Store(1)
	privateToken := strings.Repeat("a", 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+privateToken {
			t.Error("AirPlay worker request lost its private token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/status":
			fmt.Fprintf(w, `{"active":%t,"audioActive":%t,"metadata":{"title":"Track","artist":"Artist","album":"Album"}}`, mode.Load() == 1, mode.Load() == 2)
		case "/stream/index.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nvideo.ts\n")
		case "/stream/audio.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\naudio.ts\n")
		case "/stream/video.ts":
			fmt.Fprint(w, "video-fixture")
		case "/stream/audio.ts":
			fmt.Fprint(w, "audio-fixture")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(t.Context(), map[string]providers.Config{"airplay": {Enabled: true, URL: upstream.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "airplay-switch")
	if w := call(s, "PUT", "/v1/media-receiver", `{"provider":"airplay"}`, "airplay-switch", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	read := func() inbox.Snapshot {
		w := call(s, "GET", "/v1/media-receiver", "", "airplay-switch", token, "")
		var snapshot inbox.Snapshot
		if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil || w.Code != 200 {
			t.Fatal(w.Code, w.Body, err)
		}
		return snapshot
	}
	video := read()
	if video.Plan == nil || video.Plan.Item.Kind != "video" || video.Plan.Item.Provider != "airplay" || strings.Contains(video.Plan.URL, privateToken) {
		t.Fatal("video session or semantic boundary missing", video)
	}
	assertStream := func(plan *domain.Plan, expected string) {
		w := call(s, "GET", plan.URL, "", "", "", "")
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "#EXTM3U") || strings.Contains(w.Body.String(), upstream.URL) {
			t.Fatal("manifest unavailable or upstream leaked", w.Code, w.Body)
		}
		lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
		segment := lines[len(lines)-1]
		if !strings.HasPrefix(segment, "/v1/streams/") {
			t.Fatal("segment was not rewritten to the local gateway", segment)
		}
		w = call(s, "GET", segment, "", "", "", "")
		if w.Code != 200 || w.Body.String() != expected {
			t.Fatal("receiver segment unavailable", w.Code, w.Body)
		}
	}
	assertStream(video.Plan, "video-fixture")
	mode.Store(2)
	audio := read()
	if audio.Plan == nil || audio.Plan.Item.Kind != "audio" || audio.Plan.Item.Title != "Track" || audio.Plan.Item.Subtitle != "Artist" || audio.Plan.SessionID == video.Plan.SessionID {
		t.Fatal("audio did not replace video", audio)
	}
	if w := call(s, "GET", video.Plan.URL, "", "", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("old video ticket survived", w.Code)
	}
	assertStream(audio.Plan, "audio-fixture")
	mode.Store(0)
	ended := read()
	if ended.Plan != nil || ended.NowPlaying == nil || ended.NowPlaying.State != "STOPPED" {
		t.Fatal("ended AirPlay stream stayed active", ended)
	}
	if w := call(s, "GET", audio.Plan.URL, "", "", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("ended audio ticket survived", w.Code)
	}
}
