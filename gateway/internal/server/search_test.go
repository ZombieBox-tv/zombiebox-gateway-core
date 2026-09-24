package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"zombiebox.local/gateway/internal/providers"
)

func TestYouTubeSearchToPrivatePlaybackPlan(t *testing.T) {
	var searches atomic.Int32
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			searches.Add(1)
			if r.URL.Query().Get("q") == "science" {
				_, _ = w.Write([]byte(`{"items":[{"id":"aqz-KE-bpKQ","title":"A relevant result"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"items":[]}`))
			}
		} else {
			_, _ = w.Write([]byte(`{"url":"https://r1.googlevideo.com/videoplayback?signature=private","mimeType":"video/mp4"}`))
		}
	}))
	defer wrapper.Close()
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)}}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "search-device")
	other := pair(t, s, "other-device")
	for i := 0; i < 2; i++ {
		result := call(s, "GET", "/v1/home?provider=youtube&q=science", "", "search-device", token, "")
		if result.Code != 200 || !strings.Contains(result.Body.String(), "A relevant result") {
			t.Fatalf("search: %d %s", result.Code, result.Body)
		}
	}
	if searches.Load() != 1 {
		t.Fatal("search cache missed")
	}
	result := call(s, "POST", "/v1/playback", `{"itemId":"youtube-aqz-KE-bpKQ"}`, "other-device", other, "")
	if result.Code != 404 {
		t.Fatal("search results leaked across device scopes")
	}
	result = call(s, "POST", "/v1/playback", `{"itemId":"youtube-aqz-KE-bpKQ"}`, "search-device", token, "")
	if result.Code != 201 || strings.Contains(result.Body.String(), "googlevideo") || strings.Contains(result.Body.String(), "private") {
		t.Fatalf("playback boundary: %s", result.Body)
	}
	var plan struct{ SessionID string }
	_ = json.Unmarshal(result.Body.Bytes(), &plan)
	if len(s.sessions[plan.SessionID].source.Headers) != 0 {
		t.Fatal("worker token forwarded to stream")
	}
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	result = call(s, "POST", "/v1/playback", `{"itemId":"youtube-aqz-KE-bpKQ"}`, "search-device", token, "")
	if result.Code != 404 {
		t.Fatal("disabled provider search remained playable")
	}
}

func TestYouTubeHomeFallsBackToRealExploreResultsAndCachesPerDevice(t *testing.T) {
	var browses atomic.Int32
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog":
			_, _ = w.Write([]byte(`{"items":[]}`))
		case "/browse":
			browses.Add(1)
			if r.URL.Query().Get("q") == "popular" {
				_, _ = w.Write([]byte(`{"items":[{"id":"aqz-KE-bpKQ","kind":"video","title":"Explore video"}],"nextOffset":-1}`))
			} else {
				_, _ = w.Write([]byte(`{"items":[],"nextOffset":-1}`))
			}
		default:
			_, _ = w.Write([]byte(`{"url":"https://r1.googlevideo.com/videoplayback?signature=private","mimeType":"video/mp4"}`))
		}
	}))
	defer wrapper.Close()
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)}}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "home-device")
	other := pair(t, s, "other-device")
	for i := 0; i < 2; i++ {
		result := call(s, "GET", "/v1/home?provider=youtube", "", "home-device", token, "")
		if result.Code != 200 || !strings.Contains(result.Body.String(), "Explore video") {
			t.Fatalf("home explore: %d %s", result.Code, result.Body)
		}
	}
	if browses.Load() != 2 {
		t.Fatalf("home feed was fetched %d times, want anonymous and fallback once each", browses.Load())
	}
	if result := call(s, "POST", "/v1/playback", `{"itemId":"youtube-aqz-KE-bpKQ"}`, "other-device", other, ""); result.Code != 404 {
		t.Fatalf("explore source leaked to another device: %d", result.Code)
	}
	if result := call(s, "POST", "/v1/playback", `{"itemId":"youtube-aqz-KE-bpKQ"}`, "home-device", token, ""); result.Code != 201 {
		t.Fatalf("explore item could not play: %d %s", result.Code, result.Body)
	}
}
