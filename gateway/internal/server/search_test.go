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
	s.deps.RemoteMedia = remoteMediaStub{}
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

func TestYouTubeHomeUsesProviderFeedAndExposesContinuation(t *testing.T) {
	var browses atomic.Int32
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog":
			_, _ = w.Write([]byte(`{"items":[]}`))
		case "/browse":
			browses.Add(1)
			if r.URL.Query().Get("q") != "" || r.URL.Query().Get("parent") != "" {
				t.Errorf("home must use the provider feed, got query %q parent %q", r.URL.Query().Get("q"), r.URL.Query().Get("parent"))
			}
			offset := 0
			_, _ = fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
			count := 40
			if offset == 0 {
				count++ // one lookahead proves that a continuation exists
			}
			items := make([]map[string]any, 0, count)
			for i := offset; i < offset+count; i++ {
				items = append(items, map[string]any{
					"id":    fmt.Sprintf("v%010d", i),
					"kind":  "video",
					"title": fmt.Sprintf("Home video %d", i),
				})
			}
			next := offset + 40
			if offset >= 40 {
				next = -1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "nextOffset": next})
		default:
			_, _ = w.Write([]byte(`{"url":"https://r1.googlevideo.com/videoplayback?signature=private","mimeType":"video/mp4"}`))
		}
	}))
	defer wrapper.Close()
	s := testServer(t, nil, "")
	s.deps.RemoteMedia = remoteMediaStub{}
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32), CatalogID: "curated-news"}}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "home-device")
	other := pair(t, s, "other-device")
	result := call(s, "GET", "/v1/home?provider=youtube", "", "home-device", token, "")
	var screen domain.Screen
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &screen) != nil {
		t.Fatalf("home feed: %d %s", result.Code, result.Body)
	}
	if screen.NextOffset != 40 || len(screen.Sections) != 1 || len(screen.Sections[0].Items) != 40 {
		t.Fatalf("home feed page/cursor = next %d, sections %+v", screen.NextOffset, screen.Sections)
	}
	if !strings.Contains(result.Body.String(), "Home video 0") || strings.Contains(result.Body.String(), "curated-news") {
		t.Fatalf("home did not use the provider feed: %s", result.Body)
	}
	for i := 0; i < 2; i++ {
		result = call(s, "GET", "/v1/home?provider=youtube", "", "home-device", token, "")
		if result.Code != 200 {
			t.Fatalf("cached home feed: %d %s", result.Code, result.Body)
		}
	}
	if browses.Load() != 1 {
		t.Fatalf("home feed fetched %d times for one device; expected cache reuse", browses.Load())
	}
	page := call(s, "GET", "/v1/browse?provider=youtube&offset=40", "", "home-device", token, "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Home video 40") || !strings.Contains(page.Body.String(), `"nextOffset":-1`) {
		t.Fatalf("continued home page: %d %s", page.Code, page.Body)
	}
	otherHome := call(s, "GET", "/v1/home?provider=youtube", "", "other-device", other, "")
	if otherHome.Code != 200 || browses.Load() != 3 {
		t.Fatalf("per-device Home cache: status %d, browse count %d", otherHome.Code, browses.Load())
	}
	firstID := screen.Sections[0].Items[0].ID
	if result := call(s, "POST", "/v1/playback", `{"itemId":"`+firstID+`"}`, "home-device", token, ""); result.Code != 201 {
		t.Fatalf("provider Home item could not play: %d %s", result.Code, result.Body)
	}
}

func TestYouTubeHomeCatalogFallbackUsesOnlyUnconfiguredProviderHome(t *testing.T) {
	var queries []string
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog" {
			t.Errorf("unexpected wrapper path: %s", r.URL.Path)
		}
		query := r.URL.Query().Get("q")
		queries = append(queries, query)
		if query == "curated-news" {
			_, _ = w.Write([]byte(`{"items":[{"id":"aqz-KE-bpKQ","title":"Configured search"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"aqz-KE-bpKQ","title":"Provider Home"}]}`))
	}))
	defer wrapper.Close()

	s := testServer(t, nil, "")
	s.deps.Browse = nil
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32), CatalogID: "curated-news"},
	}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "catalog-fallback-home")
	result := call(s, "GET", "/v1/home?provider=youtube", "", "catalog-fallback-home", token, "")
	if result.Code != 200 || strings.Contains(result.Body.String(), "Configured search") || len(queries) != 0 {
		t.Fatalf("configured catalog query masqueraded as Home: status=%d queries=%v body=%s", result.Code, queries, result.Body)
	}

	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}
	result = call(s, "GET", "/v1/home?provider=youtube", "", "catalog-fallback-home", token, "")
	if result.Code != 200 || !strings.Contains(result.Body.String(), "Provider Home") || len(queries) != 1 || queries[0] != "" {
		t.Fatalf("empty-query provider Home fallback: status=%d queries=%v body=%s", result.Code, queries, result.Body)
	}
}
