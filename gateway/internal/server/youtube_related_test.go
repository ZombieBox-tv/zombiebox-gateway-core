package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/youtubeaccount"
)

func TestYouTubeRelatedRelevanceAndMetadata(t *testing.T) {
	currentID := "aqz-KE-bpKQ"
	rel1ID := "rel12345678"
	rel2ID := "rel23456789"

	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/browse":
			q := r.URL.Query().Get("q")
			if q != currentID && q != "Mechanical Keyboard Build" {
				t.Errorf("unexpected query: %s", q)
			}
			fmt.Fprintf(w, `{"items":[
				{"id":"%s","kind":"video","title":"Mechanical Keyboard Build","subtitle":"Keyb Channel","description":"How to build a custom keyboard","durationMs":600000},
				{"id":"%s","kind":"video","title":"Switch Lubing Guide","subtitle":"Keyb Channel","description":"Lube switches guide","durationMs":300000},
				{"id":"%s","kind":"video","title":"Keycaps Review","subtitle":"Caps Channel","description":"Best PBT keycaps","durationMs":450000}
			],"nextOffset":20}`, currentID, rel1ID, rel2ID)
		case "/catalog":
			fmt.Fprint(w, `{"items":[]}`)
		default:
			fmt.Fprint(w, `{"url":"https://r1.googlevideo.com/videoplayback?signature=private","mimeType":"video/mp4"}`)
		}
	}))
	defer wrapper.Close()

	s := testServer(t, nil, "")
	s.deps.RemoteMedia = remoteMediaStub{}
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}

	token := pair(t, s, "tv-device")

	// Call related endpoint
	resp := call(s, "GET", "/v1/youtube/related?video="+currentID, "", "tv-device", token, "")
	if resp.Code != 200 {
		t.Fatalf("related endpoint returned %d: %s", resp.Code, resp.Body)
	}

	var page domain.RelatedPage
	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if page.VideoID != currentID {
		t.Errorf("expected videoId %s, got %s", currentID, page.VideoID)
	}
	if page.CurrentVideo == nil {
		t.Fatal("expected currentVideo metadata to be populated")
	}
	if page.CurrentVideo.Title != "Mechanical Keyboard Build" {
		t.Errorf("expected currentVideo title 'Mechanical Keyboard Build', got %s", page.CurrentVideo.Title)
	}
	if page.CurrentVideo.Subtitle != "Keyb Channel" {
		t.Errorf("expected currentVideo subtitle 'Keyb Channel', got %s", page.CurrentVideo.Subtitle)
	}
	if page.CurrentVideo.Description != "How to build a custom keyboard" {
		t.Errorf("expected currentVideo description, got %s", page.CurrentVideo.Description)
	}
	if page.CurrentVideo.DurationMS != 600000 {
		t.Errorf("expected currentVideo durationMs 600000, got %d", page.CurrentVideo.DurationMS)
	}

	// Verify current video is excluded from related items
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 related items (current video excluded), got %d", len(page.Items))
	}
	for _, it := range page.Items {
		if it.ID == "youtube-"+currentID || it.ID == currentID {
			t.Errorf("current video %s was included in its own related items!", currentID)
		}
	}

	// Verify related item metadata
	item1 := page.Items[0]
	if item1.ID != "youtube-"+rel1ID {
		t.Errorf("expected item ID youtube-%s, got %s", rel1ID, item1.ID)
	}
	if item1.Title != "Switch Lubing Guide" {
		t.Errorf("expected title 'Switch Lubing Guide', got %s", item1.Title)
	}
	if item1.Subtitle != "Keyb Channel" {
		t.Errorf("expected channel/subtitle 'Keyb Channel', got %s", item1.Subtitle)
	}
	if item1.Description != "Lube switches guide" {
		t.Errorf("expected description 'Lube switches guide', got %s", item1.Description)
	}
	if item1.DurationMS != 300000 {
		t.Errorf("expected durationMs 300000, got %d", item1.DurationMS)
	}
	if !item1.Playable {
		t.Errorf("expected playable true")
	}
	if !strings.HasPrefix(item1.ImageURL, "/v1/artwork/") {
		t.Errorf("expected decorated artwork URL starting with /v1/artwork/, got %s", item1.ImageURL)
	}

	if page.NextCursor == "" || !page.HasMore {
		t.Errorf("expected valid nextCursor and hasMore=true")
	}

	// Verify that related item can be played directly
	playResp := call(s, "POST", "/v1/playback", fmt.Sprintf(`{"itemId":"%s"}`, item1.ID), "tv-device", token, "")
	if playResp.Code != 201 {
		t.Fatalf("playback of related item failed: %d %s", playResp.Code, playResp.Body)
	}

	// Verify that related item is retained in source cache with artwork URL
	src := s.searchSource(context.Background(), "tv-device", item1.ID)
	if src == nil {
		t.Fatalf("related item was not retained in source cache: %s", item1.ID)
	}
	if src.ArtworkURL == "" {
		t.Fatalf("related item has empty ArtworkURL")
	}
}

func TestYouTubeRelatedContinuationAndDeduplication(t *testing.T) {
	currentID := "aqz-KE-bpKQ"
	itemA := "vidA1234567"
	itemB := "vidB1234567"
	itemC := "vidC1234567"
	itemD := "vidD1234567"

	var browseCall atomic.Int32
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callNum := browseCall.Add(1)
		offset := r.URL.Query().Get("offset")
		switch offset {
		case "0":
			// Page 1: returns itemA, itemB, nextOffset 20
			fmt.Fprintf(w, `{"items":[
				{"id":"%s","kind":"video","title":"Current","subtitle":"Ch","durationMs":100},
				{"id":"%s","kind":"video","title":"Item A","subtitle":"Ch","durationMs":100},
				{"id":"%s","kind":"video","title":"Item B","subtitle":"Ch","durationMs":100}
			],"nextOffset":20}`, currentID, itemA, itemB)
		case "20":
			// Page 2: returns itemB (duplicate from page 1), itemC, nextOffset 40
			fmt.Fprintf(w, `{"items":[
				{"id":"%s","kind":"video","title":"Item B Dupe","subtitle":"Ch","durationMs":100},
				{"id":"%s","kind":"video","title":"Item C","subtitle":"Ch","durationMs":100}
			],"nextOffset":40}`, itemB, itemC)
		case "40":
			// Page 3: returns itemD, nextOffset -1 (exhausted)
			fmt.Fprintf(w, `{"items":[
				{"id":"%s","kind":"video","title":"Item D","subtitle":"Ch","durationMs":100}
			],"nextOffset":-1}`, itemD)
		default:
			t.Errorf("call %d: unexpected offset: %s", callNum, offset)
			fmt.Fprint(w, `{"items":[],"nextOffset":-1}`)
		}
	}))
	defer wrapper.Close()

	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}

	token := pair(t, s, "tv-device-pages")

	// Page 1
	resp1 := call(s, "GET", "/v1/youtube/related?video="+currentID+"&limit=10", "", "tv-device-pages", token, "")
	if resp1.Code != 200 {
		t.Fatalf("page 1 failed: %d %s", resp1.Code, resp1.Body)
	}
	var p1 domain.RelatedPage
	_ = json.Unmarshal(resp1.Body.Bytes(), &p1)
	if len(p1.Items) != 2 {
		t.Fatalf("expected 2 items on page 1, got %d", len(p1.Items))
	}
	if p1.Items[0].ID != "youtube-"+itemA || p1.Items[1].ID != "youtube-"+itemB {
		t.Errorf("unexpected items on page 1: %+v", p1.Items)
	}
	if p1.NextCursor == "" || !p1.HasMore {
		t.Fatal("expected non-empty nextCursor on page 1")
	}

	// Page 2 with cursor from Page 1
	resp2 := call(s, "GET", "/v1/youtube/related?video="+currentID+"&cursor="+p1.NextCursor+"&limit=10", "", "tv-device-pages", token, "")
	if resp2.Code != 200 {
		t.Fatalf("page 2 failed: %d %s", resp2.Code, resp2.Body)
	}
	var p2 domain.RelatedPage
	_ = json.Unmarshal(resp2.Body.Bytes(), &p2)
	// Item B must be deduplicated across pages, returning only new item C
	if len(p2.Items) != 1 {
		t.Fatalf("expected exactly 1 item on page 2 (item B deduplicated), got %d: %+v", len(p2.Items), p2.Items)
	}
	if p2.Items[0].ID != "youtube-"+itemC {
		t.Errorf("expected itemC on page 2, got: %+v", p2.Items[0].ID)
	}
	for _, it := range p2.Items {
		if it.ID == "youtube-"+currentID || it.ID == currentID {
			t.Errorf("current video %s was included on page 2!", currentID)
		}
	}
	if p2.NextCursor == "" || !p2.HasMore {
		t.Fatal("expected non-empty nextCursor on page 2")
	}

	// Page 3 with cursor from Page 2
	resp3 := call(s, "GET", "/v1/youtube/related?video="+currentID+"&cursor="+p2.NextCursor+"&limit=10", "", "tv-device-pages", token, "")
	if resp3.Code != 200 {
		t.Fatalf("page 3 failed: %d %s", resp3.Code, resp3.Body)
	}
	var p3 domain.RelatedPage
	_ = json.Unmarshal(resp3.Body.Bytes(), &p3)
	if len(p3.Items) != 1 || p3.Items[0].ID != "youtube-"+itemD {
		t.Errorf("expected itemD on page 3, got %+v", p3.Items)
	}
	for _, it := range p3.Items {
		if it.ID == "youtube-"+currentID || it.ID == currentID {
			t.Errorf("current video %s was included on page 3!", currentID)
		}
	}
	// Termination check: nextCursor should be empty, hasMore false
	if p3.NextCursor != "" || p3.HasMore {
		t.Errorf("expected termination on exhausted stream, got nextCursor='%s', hasMore=%v", p3.NextCursor, p3.HasMore)
	}
}

func TestYouTubeRelatedEmptyState(t *testing.T) {
	currentID := "aqz-KE-bpKQ"
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[],"nextOffset":-1}`)
	}))
	defer wrapper.Close()

	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}

	token := pair(t, s, "empty-device")

	resp := call(s, "GET", "/v1/youtube/related?video="+currentID, "", "empty-device", token, "")
	if resp.Code != 200 {
		t.Fatalf("expected 200 for empty state, got %d: %s", resp.Code, resp.Body)
	}
	var page domain.RelatedPage
	_ = json.Unmarshal(resp.Body.Bytes(), &page)
	if len(page.Items) != 0 {
		t.Errorf("expected empty items, got %d", len(page.Items))
	}
	if page.NextCursor != "" {
		t.Errorf("expected empty nextCursor, got %s", page.NextCursor)
	}
	if page.HasMore {
		t.Errorf("expected hasMore false")
	}
	if page.Total != 0 {
		t.Errorf("expected total 0, got %d", page.Total)
	}
}

func TestYouTubeRelatedValidationAndBadCursors(t *testing.T) {
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: "http://127.0.0.1:1", Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}

	token := pair(t, s, "validation-device")

	// Missing video ID
	if w := call(s, "GET", "/v1/youtube/related", "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("missing video: want 400, got %d", w.Code)
	}

	// Too short video ID
	if w := call(s, "GET", "/v1/youtube/related?video=short", "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("short video: want 400, got %d", w.Code)
	}

	// Invalid characters in video ID
	if w := call(s, "GET", "/v1/youtube/related?video=bad!video@1", "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("invalid video chars: want 400, got %d", w.Code)
	}

	// Corrupted base64 cursor
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&cursor=not_valid_base64_chars!@#$", "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("corrupted cursor: want 400, got %d", w.Code)
	}

	// Valid base64 but invalid JSON
	garbageJSON := base64.RawURLEncoding.EncodeToString([]byte("this is not json"))
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&cursor="+garbageJSON, "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("invalid json cursor: want 400, got %d", w.Code)
	}

	// Cursor for different video ID
	wrongVideoCursor := base64.RawURLEncoding.EncodeToString([]byte(`{"v":"other123456","o":20}`))
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&cursor="+wrongVideoCursor, "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("mismatched video cursor: want 400, got %d", w.Code)
	}

	// Cursor with negative offset
	negOffsetCursor := base64.RawURLEncoding.EncodeToString([]byte(`{"v":"aqz-KE-bpKQ","o":-1}`))
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&cursor="+negOffsetCursor, "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("negative offset cursor: want 400, got %d", w.Code)
	}

	// Cursor with offset > 360
	bigOffsetCursor := base64.RawURLEncoding.EncodeToString([]byte(`{"v":"aqz-KE-bpKQ","o":400}`))
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&cursor="+bigOffsetCursor, "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("big offset cursor: want 400, got %d", w.Code)
	}

	// Cursor exceeding max length (512 bytes)
	hugeCursor := strings.Repeat("A", 600)
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&cursor="+hugeCursor, "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("oversized cursor: want 400, got %d", w.Code)
	}

	// Invalid offset query param
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&offset=-5", "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("negative offset param: want 400, got %d", w.Code)
	}
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&offset=500", "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("oversized offset param: want 400, got %d", w.Code)
	}

	// Invalid limit query param
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&limit=0", "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("zero limit: want 400, got %d", w.Code)
	}
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ&limit=100", "", "validation-device", token, ""); w.Code != 400 {
		t.Errorf("excess limit: want 400, got %d", w.Code)
	}

	// Disabled provider
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if w := call(s, "GET", "/v1/youtube/related?video=aqz-KE-bpKQ", "", "validation-device", token, ""); w.Code != 409 {
		t.Errorf("disabled provider: want 409, got %d", w.Code)
	}
}

func TestYouTubeHomeFeedThreeTierFallback(t *testing.T) {
	// Setup upstream wrapper and fake Google OAuth API
	var browseQuery atomic.Value
	var browseParent atomic.Value
	browseQuery.Store("")
	browseParent.Store("")

	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/browse":
			q := r.URL.Query().Get("q")
			parent := r.URL.Query().Get("parent")
			browseQuery.Store(q)
			browseParent.Store(parent)
			if strings.HasPrefix(parent, "channel:UC") {
				// Subscribed channel's videos
				fmt.Fprint(w, `{"items":[{"id":"subVideo123","kind":"video","title":"Subscribed Channel Video"}],"nextOffset":-1}`)
			} else if q == "mechanical keyboards" {
				// Recent context query
				fmt.Fprint(w, `{"items":[{"id":"keyVideo123","kind":"video","title":"Keyboards Video"}],"nextOffset":-1}`)
			} else if q == "popular" {
				// Generic popular
				fmt.Fprint(w, `{"items":[{"id":"popVideo123","kind":"video","title":"Popular Video"}],"nextOffset":-1}`)
			} else {
				fmt.Fprint(w, `{"items":[],"nextOffset":-1}`)
			}
		case "/catalog":
			fmt.Fprint(w, `{"items":[]}`)
		default:
			fmt.Fprint(w, `{"url":"https://r1.googlevideo.com/videoplayback?signature=private","mimeType":"video/mp4"}`)
		}
	}))
	defer wrapper.Close()

	googleAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			fmt.Fprint(w, `{"device_code":"dc","user_code":"uc","verification_url":"https://google.com/device","expires_in":1800,"interval":5}`)
		case "/token":
			fmt.Fprint(w, `{"access_token":"token","refresh_token":"refresh","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.readonly"}`)
		case "/v3/subscriptions":
			fmt.Fprint(w, `{"items":[{"id":"sub1","snippet":{"title":"Tech Channel","description":"","resourceId":{"channelId":"UC1234567890123456789012"}}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer googleAPI.Close()

	s := testServer(t, nil, "")
	clock := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)
	s.youtubeAccount = youtubeaccount.New(s.db, googleAPI.Client(), "fixture-client", "", youtubeaccount.Endpoints{
		Device: googleAPI.URL + "/device",
		Token:  googleAPI.URL + "/token",
		Revoke: googleAPI.URL + "/revoke",
		Data:   googleAPI.URL + "/v3",
	}, func() time.Time { return clock })

	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}

	token := pair(t, s, "home-device-fallback")

	// --- Case 1: Cold start, disconnected account, no search/view history ---
	// Must fall back to Tier 3: generic anonymous popular
	res1 := call(s, "GET", "/v1/home?provider=youtube", "", "home-device-fallback", token, "")
	if res1.Code != 200 || !strings.Contains(res1.Body.String(), "Popular Video") {
		t.Fatalf("cold home: want Popular Video, got %d %s", res1.Code, res1.Body)
	}
	if browseQuery.Load().(string) != "popular" {
		t.Fatalf("expected popular query on cold home, got: %s", browseQuery.Load().(string))
	}

	// --- Case 2: Recent search/view context (Tier 2) ---
	// Device searches for "mechanical keyboards"
	searchRes := call(s, "GET", "/v1/home?provider=youtube&q=mechanical+keyboards", "", "home-device-fallback", token, "")
	if searchRes.Code != 200 {
		t.Fatalf("search failed: %d", searchRes.Code)
	}

	// Now fetch Home without query - must switch to recent search/view context (Tier 2) instead of popular!
	res2 := call(s, "GET", "/v1/home?provider=youtube", "", "home-device-fallback", token, "")
	if res2.Code != 200 || !strings.Contains(res2.Body.String(), "Keyboards Video") {
		t.Fatalf("recent context home: want Keyboards Video, got %d %s", res2.Code, res2.Body)
	}
	if browseQuery.Load().(string) != "mechanical keyboards" {
		t.Fatalf("expected 'mechanical keyboards' query on context home, got: %s", browseQuery.Load().(string))
	}

	// --- Case 3: Signed-in profile feed (Tier 1) ---
	// Connect YouTube account
	_ = call(s, "POST", "/v1/youtube/account/authorization", "", "home-device-fallback", token, "")
	clock = clock.Add(6 * time.Second)
	pollRes := call(s, "POST", "/v1/youtube/account/authorization/poll", "", "home-device-fallback", token, "")
	if pollRes.Code != 200 {
		t.Fatalf("poll failed: %d %s", pollRes.Code, pollRes.Body)
	}

	// Fetch Home: must prioritize signed-in profile feed over recent context!
	res3 := call(s, "GET", "/v1/home?provider=youtube", "", "home-device-fallback", token, "")
	if res3.Code != 200 || !strings.Contains(res3.Body.String(), "Subscribed Channel Video") {
		t.Fatalf("signed-in home: want Subscribed Channel Video, got %d %s", res3.Code, res3.Body)
	}
	if !strings.HasPrefix(browseParent.Load().(string), "channel:UC") {
		t.Fatalf("expected subscribed channel parent, got: %s", browseParent.Load().(string))
	}

	// --- Case 4: Disconnect account ---
	// Must not manufacture recommendations from disconnected account
	_ = call(s, "DELETE", "/v1/youtube/account", "", "home-device-fallback", token, "123456")

	// After disconnect, falls back to Tier 2 (recent context)
	res4 := call(s, "GET", "/v1/home?provider=youtube", "", "home-device-fallback", token, "")
	if res4.Code != 200 || !strings.Contains(res4.Body.String(), "Keyboards Video") {
		t.Fatalf("post-disconnect home: want Keyboards Video, got %d %s", res4.Code, res4.Body)
	}
}

func TestYouTubeRelatedRepeatedOnlyPageTerminates(t *testing.T) {
	currentID := "aqz-KE-bpKQ"
	itemA := "vidA1234567"
	itemB := "vidB1234567"

	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		switch offset {
		case "0":
			// Page 1: returns itemA, itemB, nextOffset 20
			fmt.Fprintf(w, `{"items":[
				{"id":"%s","kind":"video","title":"Current","subtitle":"Ch","durationMs":100},
				{"id":"%s","kind":"video","title":"Item A","subtitle":"Ch","durationMs":100},
				{"id":"%s","kind":"video","title":"Item B","subtitle":"Ch","durationMs":100}
			],"nextOffset":20}`, currentID, itemA, itemB)
		case "20":
			// Page 2: returns only repeated IDs from page 1 and current video, with nextOffset 40
			fmt.Fprintf(w, `{"items":[
				{"id":"%s","kind":"video","title":"Current Dupe","subtitle":"Ch","durationMs":100},
				{"id":"%s","kind":"video","title":"Item A Dupe","subtitle":"Ch","durationMs":100},
				{"id":"%s","kind":"video","title":"Item B Dupe","subtitle":"Ch","durationMs":100}
			],"nextOffset":40}`, currentID, itemA, itemB)
		default:
			fmt.Fprint(w, `{"items":[],"nextOffset":-1}`)
		}
	}))
	defer wrapper.Close()

	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}

	token := pair(t, s, "tv-device-repeated")

	// Page 1
	resp1 := call(s, "GET", "/v1/youtube/related?video="+currentID+"&limit=10", "", "tv-device-repeated", token, "")
	if resp1.Code != 200 {
		t.Fatalf("page 1 failed: %d %s", resp1.Code, resp1.Body)
	}
	var p1 domain.RelatedPage
	_ = json.Unmarshal(resp1.Body.Bytes(), &p1)
	if len(p1.Items) != 2 {
		t.Fatalf("expected 2 items on page 1, got %d", len(p1.Items))
	}
	if p1.NextCursor == "" || !p1.HasMore {
		t.Fatal("expected non-empty nextCursor on page 1")
	}

	// Page 2: all items are repeated / current video.
	// Must filter out earlier IDs, return only new IDs (none), and terminate!
	resp2 := call(s, "GET", "/v1/youtube/related?video="+currentID+"&cursor="+p1.NextCursor+"&limit=10", "", "tv-device-repeated", token, "")
	if resp2.Code != 200 {
		t.Fatalf("page 2 failed: %d %s", resp2.Code, resp2.Body)
	}
	var p2 domain.RelatedPage
	_ = json.Unmarshal(resp2.Body.Bytes(), &p2)
	if len(p2.Items) != 0 {
		t.Fatalf("expected 0 new items on repeated-only page 2, got %d: %+v", len(p2.Items), p2.Items)
	}
	if p2.NextCursor != "" {
		t.Errorf("expected empty nextCursor on repeated-only page, got %s", p2.NextCursor)
	}
	if p2.HasMore {
		t.Errorf("expected hasMore=false on repeated-only page termination")
	}
}

func TestYouTubeHomeFeedPrecedenceWithCatalogID(t *testing.T) {
	var browseQuery atomic.Value
	var browseParent atomic.Value
	browseQuery.Store("")
	browseParent.Store("")

	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/browse":
			q := r.URL.Query().Get("q")
			parent := r.URL.Query().Get("parent")
			browseQuery.Store(q)
			browseParent.Store(parent)
			if strings.HasPrefix(parent, "channel:UC") {
				fmt.Fprint(w, `{"items":[{"id":"subVideo123","kind":"video","title":"Subscribed Channel Video"}],"nextOffset":-1}`)
			} else if q == "mechanical keyboards" {
				fmt.Fprint(w, `{"items":[{"id":"keyVideo123","kind":"video","title":"Keyboards Video"}],"nextOffset":-1}`)
			} else if q == "curated-news" {
				fmt.Fprint(w, `{"items":[{"id":"pinVideo123","kind":"video","title":"Operator Pinned Video"}],"nextOffset":-1}`)
			} else if q == "popular" {
				fmt.Fprint(w, `{"items":[{"id":"popVideo123","kind":"video","title":"Popular Video"}],"nextOffset":-1}`)
			} else {
				fmt.Fprint(w, `{"items":[],"nextOffset":-1}`)
			}
		case "/catalog":
			q := r.URL.Query().Get("q")
			if q == "curated-news" {
				fmt.Fprint(w, `{"items":[{"id":"pinVideo123","title":"Operator Pinned Video","kind":"video","durationMs":100}]}`)
			} else {
				fmt.Fprint(w, `{"items":[]}`)
			}
		default:
			fmt.Fprint(w, `{"url":"https://r1.googlevideo.com/videoplayback?signature=private","mimeType":"video/mp4"}`)
		}
	}))
	defer wrapper.Close()

	googleAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			fmt.Fprint(w, `{"device_code":"dc","user_code":"uc","verification_url":"https://google.com/device","expires_in":1800,"interval":5}`)
		case "/token":
			fmt.Fprint(w, `{"access_token":"token","refresh_token":"refresh","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.readonly"}`)
		case "/v3/subscriptions":
			fmt.Fprint(w, `{"items":[{"id":"sub1","snippet":{"title":"Tech Channel","description":"","resourceId":{"channelId":"UC1234567890123456789012"}}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer googleAPI.Close()

	s := testServer(t, nil, "")
	clock := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)
	s.youtubeAccount = youtubeaccount.New(s.db, googleAPI.Client(), "fixture-client", "", youtubeaccount.Endpoints{
		Device: googleAPI.URL + "/device",
		Token:  googleAPI.URL + "/token",
		Revoke: googleAPI.URL + "/revoke",
		Data:   googleAPI.URL + "/v3",
	}, func() time.Time { return clock })

	// Operator has explicitly pinned CatalogID = "curated-news"
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32), CatalogID: "curated-news"},
	}); err != nil {
		t.Fatal(err)
	}

	token := pair(t, s, "home-device-pinned")

	// --- Case 1: Cold start, disconnected account, no search/view history ---
	// Must fall back to operator-pinned catalog, NOT generic popular!
	res1 := call(s, "GET", "/v1/home?provider=youtube", "", "home-device-pinned", token, "")
	if res1.Code != 200 || !strings.Contains(res1.Body.String(), "Operator Pinned Video") {
		t.Fatalf("cold home with pinned catalog: want Operator Pinned Video, got %d %s", res1.Code, res1.Body)
	}
	if strings.Contains(res1.Body.String(), "Popular Video") {
		t.Fatalf("cold home returned generic popular instead of pinned catalog")
	}

	// --- Case 2: User performs search for "mechanical keyboards" ---
	searchRes := call(s, "GET", "/v1/home?provider=youtube&q=mechanical+keyboards", "", "home-device-pinned", token, "")
	if searchRes.Code != 200 {
		t.Fatalf("search failed: %d", searchRes.Code)
	}

	// Home fetch without query: user-context feed genuinely REPLACES operator-pinned catalog!
	res2 := call(s, "GET", "/v1/home?provider=youtube", "", "home-device-pinned", token, "")
	if res2.Code != 200 || !strings.Contains(res2.Body.String(), "Keyboards Video") {
		t.Fatalf("context home: want Keyboards Video replacing pinned catalog, got %d %s", res2.Code, res2.Body)
	}
	if strings.Contains(res2.Body.String(), "Operator Pinned Video") {
		t.Fatalf("context home failed to replace operator-pinned catalog")
	}

	// --- Case 3: Connect YouTube OAuth account ---
	_ = call(s, "POST", "/v1/youtube/account/authorization", "", "home-device-pinned", token, "")
	clock = clock.Add(6 * time.Second)
	pollRes := call(s, "POST", "/v1/youtube/account/authorization/poll", "", "home-device-pinned", token, "")
	if pollRes.Code != 200 {
		t.Fatalf("poll failed: %d %s", pollRes.Code, pollRes.Body)
	}

	// Home fetch: signed-in profile feed genuinely REPLACES recent context and operator-pinned catalog!
	res3 := call(s, "GET", "/v1/home?provider=youtube", "", "home-device-pinned", token, "")
	if res3.Code != 200 || !strings.Contains(res3.Body.String(), "Subscribed Channel Video") {
		t.Fatalf("signed-in home: want Subscribed Channel Video, got %d %s", res3.Code, res3.Body)
	}
	if strings.Contains(res3.Body.String(), "Operator Pinned Video") {
		t.Fatalf("signed-in home failed to replace operator-pinned catalog")
	}

	// --- Case 4: Disconnect account ---
	_ = call(s, "DELETE", "/v1/youtube/account", "", "home-device-pinned", token, "123456")

	// Post-disconnect: falls back to recent context (replaces pinned catalog)
	res4 := call(s, "GET", "/v1/home?provider=youtube", "", "home-device-pinned", token, "")
	if res4.Code != 200 || !strings.Contains(res4.Body.String(), "Keyboards Video") {
		t.Fatalf("post-disconnect home: want Keyboards Video, got %d %s", res4.Code, res4.Body)
	}

	// --- Case 5: Fresh device with no context ---
	token2 := pair(t, s, "fresh-device-pinned")
	res5 := call(s, "GET", "/v1/home?provider=youtube", "", "fresh-device-pinned", token2, "")
	if res5.Code != 200 || !strings.Contains(res5.Body.String(), "Operator Pinned Video") {
		t.Fatalf("fresh device: want Operator Pinned Video, got %d %s", res5.Code, res5.Body)
	}
}

func TestYouTubeRelatedSeedVideoMetadataForDIALPlayback(t *testing.T) {
	currentID := "aqz-KE-bpKQ"
	rel1ID := "rel12345678"
	rel2ID := "rel23456789"

	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/browse":
			q := r.URL.Query().Get("q")
			if q == currentID {
				// Querying specifically for seed ID returns the seed video metadata
				fmt.Fprintf(w, `{"items":[
					{"id":"%s","kind":"video","title":"Mechanical Keyboard Build","subtitle":"Keyb Channel","description":"How to build a custom keyboard","durationMs":600000}
				],"nextOffset":-1}`, currentID)
			} else if q == "Mechanical Keyboard Build" || q == "related-search-title" {
				// Title query returns related items, but NOT the exact seed video!
				fmt.Fprintf(w, `{"items":[
					{"id":"%s","kind":"video","title":"Switch Lubing Guide","subtitle":"Keyb Channel","description":"Lube switches guide","durationMs":300000},
					{"id":"%s","kind":"video","title":"Keycaps Review","subtitle":"Caps Channel","description":"Best PBT keycaps","durationMs":450000}
				],"nextOffset":20}`, rel1ID, rel2ID)
			} else {
				t.Errorf("unexpected browse query: %s", q)
				fmt.Fprint(w, `{"items":[],"nextOffset":-1}`)
			}
		case "/catalog":
			fmt.Fprint(w, `{"items":[]}`)
		default:
			fmt.Fprint(w, `{"url":"https://r1.googlevideo.com/videoplayback?signature=private","mimeType":"video/mp4"}`)
		}
	}))
	defer wrapper.Close()

	s := testServer(t, nil, "")
	s.deps.RemoteMedia = remoteMediaStub{}
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}

	token := pair(t, s, "dial-tv-device")

	// Seed DIAL receiver context with placeholder title "YouTube"
	s.recordRecentYouTubeContext("dial-tv-device", "Mechanical Keyboard Build", "related-search-title", currentID)

	// Call related endpoint: title query will return rel1 & rel2, omitting the exact seed.
	// The implementation must fetch the seed metadata from the provider, populating CurrentVideo.
	resp := call(s, "GET", "/v1/youtube/related?video="+currentID, "", "dial-tv-device", token, "")
	if resp.Code != 200 {
		t.Fatalf("related endpoint returned %d: %s", resp.Code, resp.Body)
	}

	var page domain.RelatedPage
	if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if page.CurrentVideo == nil {
		t.Fatal("expected currentVideo metadata to not be left blank")
	}
	if page.CurrentVideo.Title != "Mechanical Keyboard Build" {
		t.Errorf("expected currentVideo title 'Mechanical Keyboard Build', got %s", page.CurrentVideo.Title)
	}
	if page.CurrentVideo.Subtitle != "Keyb Channel" {
		t.Errorf("expected currentVideo subtitle 'Keyb Channel', got %s", page.CurrentVideo.Subtitle)
	}
	if page.CurrentVideo.Description != "How to build a custom keyboard" {
		t.Errorf("expected currentVideo description, got %s", page.CurrentVideo.Description)
	}
	if page.CurrentVideo.DurationMS != 600000 {
		t.Errorf("expected currentVideo durationMs 600000, got %d", page.CurrentVideo.DurationMS)
	}

	// Verify current video is excluded from related items
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 related items (seed excluded), got %d", len(page.Items))
	}
	for _, it := range page.Items {
		if it.ID == "youtube-"+currentID || it.ID == currentID {
			t.Errorf("seed video %s was included in its own related items!", currentID)
		}
	}

	// Verify that the seed video is available for DIAL playback in searchSource with full metadata
	src := s.searchSource(context.Background(), "dial-tv-device", "youtube-"+currentID)
	if src == nil {
		t.Fatalf("seed video was not available in searchSource for DIAL playback")
	}
	if src.Item.Title != "Mechanical Keyboard Build" {
		t.Errorf("expected seed video title 'Mechanical Keyboard Build', got %s", src.Item.Title)
	}
	if src.Item.Subtitle != "Keyb Channel" {
		t.Errorf("expected seed video subtitle 'Keyb Channel', got %s", src.Item.Subtitle)
	}
	if src.Item.Description != "How to build a custom keyboard" {
		t.Errorf("expected seed video description, got %s", src.Item.Description)
	}
	if src.Item.DurationMS != 600000 {
		t.Errorf("expected seed video durationMs 600000, got %d", src.Item.DurationMS)
	}
}
