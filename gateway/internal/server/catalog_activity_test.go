package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

func TestYouTubeHomeUsesDeviceWatchActivityAndKeysetPages(t *testing.T) {
	s := testServer(t, nil, "")
	const device = "youtube-activity-device"
	const otherDevice = "youtube-activity-other-device"
	token := pair(t, s, device)
	otherToken := pair(t, s, otherDevice)
	now := time.Now().Unix()
	for i := 0; i < 45; i++ {
		id := fmt.Sprintf("youtube-activity-%03d", i)
		putYouTubeActivityProgress(t, s, device, domain.Progress{
			Item: domain.Item{
				ID:       id,
				Provider: "youtube",
				Kind:     "video",
				Title:    fmt.Sprintf("Watched video %d", i),
				Playable: true,
			},
			PositionMS: int64(i+1) * 1000,
			DurationMS: 120_000,
			State:      "PAUSED",
			UpdatedAt:  now + int64(i),
		})
	}
	putYouTubeActivityProgress(t, s, device, domain.Progress{
		Item:       domain.Item{ID: "plex-video", Provider: "plex", Kind: "video", Playable: true},
		PositionMS: 10_000,
		UpdatedAt:  now + 100,
	})
	putYouTubeActivityProgress(t, s, device, domain.Progress{
		Item:       domain.Item{ID: "youtube-audio", Provider: "youtube", Kind: "audio", Playable: true},
		PositionMS: 10_000,
		UpdatedAt:  now + 101,
	})
	putYouTubeActivityProgress(t, s, device, domain.Progress{
		Item:      domain.Item{ID: "youtube-not-played", Provider: "youtube", Kind: "video", Playable: true},
		UpdatedAt: now + 102,
	})
	putYouTubeActivityProgress(t, s, device, domain.Progress{
		Item:       domain.Item{ID: "youtube-unplayable", Provider: "youtube", Kind: "video", Playable: false},
		PositionMS: 10_000,
		UpdatedAt:  now + 103,
	})
	putYouTubeActivityProgress(t, s, otherDevice, domain.Progress{
		Item:       domain.Item{ID: "youtube-other-device", Provider: "youtube", Kind: "video", Playable: true},
		PositionMS: 10_000,
		UpdatedAt:  now + 104,
	})

	first := call(s, "GET", "/v1/home?provider=youtube", "", device, token, "")
	if first.Code != 200 {
		t.Fatalf("activity page 1: %d %s", first.Code, first.Body)
	}
	var firstPage struct {
		FeedType   string           `json:"feedType"`
		Sections   []domain.Section `json:"sections"`
		NextCursor string           `json:"nextCursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if firstPage.FeedType != "zombiebox_activity" || len(firstPage.Sections) != 1 {
		t.Fatalf("activity feed was not identified semantically: %s", first.Body)
	}
	if firstPage.Sections[0].ID != "youtube-activity" || firstPage.Sections[0].Title != "Your ZombieBox YouTube activity" {
		t.Fatalf("unexpected activity label: %+v", firstPage.Sections[0])
	}
	if len(firstPage.Sections[0].Items) != youtubeActivityPageSize || firstPage.NextCursor == "" {
		t.Fatalf("first page count/cursor = %d / %q", len(firstPage.Sections[0].Items), firstPage.NextCursor)
	}
	if firstPage.Sections[0].Items[0].ID != "youtube-activity-044" ||
		firstPage.Sections[0].Items[0].PositionMS != 45_000 ||
		firstPage.Sections[0].Items[0].DurationMS != 120_000 {
		t.Fatalf("latest watch progress was not preserved: %+v", firstPage.Sections[0].Items[0])
	}

	ids := make(map[string]struct{}, 45)
	for _, item := range firstPage.Sections[0].Items {
		if _, duplicate := ids[item.ID]; duplicate {
			t.Fatalf("duplicate item in page 1: %s", item.ID)
		}
		ids[item.ID] = struct{}{}
	}
	second := call(
		s,
		"GET",
		"/v1/home?provider=youtube&activityCursor="+url.QueryEscape(firstPage.NextCursor),
		"",
		device,
		token,
		"",
	)
	if second.Code != 200 {
		t.Fatalf("activity page 2: %d %s", second.Code, second.Body)
	}
	var secondPage struct {
		FeedType   string           `json:"feedType"`
		Sections   []domain.Section `json:"sections"`
		NextCursor string           `json:"nextCursor"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if secondPage.FeedType != "zombiebox_activity" || len(secondPage.Sections) != 1 || len(secondPage.Sections[0].Items) != 5 || secondPage.NextCursor != "" {
		t.Fatalf("last page count/cursor = %+v body=%s", secondPage, second.Body)
	}
	for _, item := range secondPage.Sections[0].Items {
		if _, duplicate := ids[item.ID]; duplicate {
			t.Fatalf("repeated item across pages: %s", item.ID)
		}
		ids[item.ID] = struct{}{}
	}
	if len(ids) != 45 {
		t.Fatalf("pages returned %d unique items, want 45", len(ids))
	}

	other := call(s, "GET", "/v1/home?provider=youtube", "", otherDevice, otherToken, "")
	if other.Code != 200 || !containsJSONItemID(other.Body.String(), "youtube-other-device") || containsJSONItemID(other.Body.String(), "youtube-activity-000") {
		t.Fatalf("watch activity was not device-scoped: %d %s", other.Code, other.Body)
	}
}

func TestYouTubeHomeRejectsInvalidActivityCursor(t *testing.T) {
	s := testServer(t, nil, "")
	const device = "youtube-invalid-activity-cursor"
	token := pair(t, s, device)
	result := call(s, "GET", "/v1/home?provider=youtube&activityCursor=not-a-cursor", "", device, token, "")
	if result.Code != 400 || !strings.Contains(result.Body.String(), "invalid_activity_cursor") {
		t.Fatalf("invalid cursor response: %d %s", result.Code, result.Body)
	}
}

func TestYouTubeHomeAddsDeviceScopedTitleSuggestionsOutsideHistoryCursor(t *testing.T) {
	var queries []string
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/browse" || r.URL.Query().Get("offset") != "0" {
			t.Errorf("unexpected title-search request: %s", r.URL.String())
			return
		}
		query := r.URL.Query().Get("q")
		queries = append(queries, query)
		var items []map[string]any
		switch query {
		case "Mechanical Keyboard Build":
			items = []map[string]any{
				{"id": "aqz-KE-bpKQ", "kind": "video", "title": "Mechanical Keyboard Build"},
				{"id": "yS6v3U2X1a_", "kind": "video", "title": "Keyboard Switch Comparison"},
				{"id": "AbCdEfGhIjK", "kind": "video", "title": "Keyboard Assembly"},
			}
		case "Acoustic Guitar Lesson":
			items = []map[string]any{
				{"id": "M7lc1UVf-VE", "kind": "video", "title": "Acoustic Guitar Lesson"},
				{"id": "AbCdEfGhIjK", "kind": "video", "title": "Keyboard Assembly"},
				{"id": "LmNoPqRsTuV", "kind": "video", "title": "Beginner Guitar Chords"},
			}
		case "Solo Watched Video":
			items = []map[string]any{
				{"id": "ScMzIvxBSi4", "kind": "video", "title": "Solo Watched Video"},
			}
		default:
			t.Errorf("unexpected or non-activity search query %q", query)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "nextOffset": -1})
	}))
	defer wrapper.Close()

	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: wrapper.URL, Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}
	const device = "youtube-title-suggestions"
	const otherDevice = "youtube-title-suggestions-other"
	token := pair(t, s, device)
	otherToken := pair(t, s, otherDevice)
	now := time.Now().Unix()
	putYouTubeActivityProgress(t, s, device, domain.Progress{
		Item:       domain.Item{ID: "youtube-aqz-KE-bpKQ", Provider: "youtube", Kind: "video", Title: "Mechanical Keyboard Build", Playable: true},
		PositionMS: 30_000,
		UpdatedAt:  now + 2,
	})
	putYouTubeActivityProgress(t, s, device, domain.Progress{
		Item:       domain.Item{ID: "youtube-M7lc1UVf-VE", Provider: "youtube", Kind: "video", Title: "Acoustic Guitar Lesson", Playable: true},
		PositionMS: 20_000,
		UpdatedAt:  now + 1,
	})
	for i := 0; i < 42; i++ {
		putYouTubeActivityProgress(t, s, device, domain.Progress{
			Item:       domain.Item{ID: fmt.Sprintf("youtube-history-%03d", i), Provider: "youtube", Kind: "video", Title: fmt.Sprintf("Watched video %d", i), Playable: true},
			PositionMS: 5_000,
			UpdatedAt:  now - int64(i+1),
		})
	}
	putYouTubeActivityProgress(t, s, device, domain.Progress{
		Item:       domain.Item{ID: "youtube-ScMzIvxBSi4", Provider: "youtube", Kind: "video", Title: "Old watched video", Playable: true},
		PositionMS: 5_000,
		UpdatedAt:  now - int64(31*24*time.Hour/time.Second),
	})
	putYouTubeActivityProgress(t, s, otherDevice, domain.Progress{
		Item:       domain.Item{ID: "youtube-ScMzIvxBSi4", Provider: "youtube", Kind: "video", Title: "Solo Watched Video", Playable: true},
		PositionMS: 10_000,
		UpdatedAt:  now,
	})

	first := call(s, "GET", "/v1/home?provider=youtube", "", device, token, "")
	if first.Code != 200 {
		t.Fatalf("activity Home: %d %s", first.Code, first.Body)
	}
	var firstPage struct {
		FeedType   string           `json:"feedType"`
		Sections   []domain.Section `json:"sections"`
		NextCursor string           `json:"nextCursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if firstPage.FeedType != "zombiebox_activity" || firstPage.NextCursor == "" || len(firstPage.Sections) != 2 {
		t.Fatalf("first Home page contract: %s", first.Body)
	}
	suggestions := firstPage.Sections[0]
	history := firstPage.Sections[1]
	if suggestions.ID != "youtube-watch-title-suggestions" ||
		suggestions.Title != "Title search suggestions from ZombieBox watch activity" ||
		len(suggestions.Items) != 3 {
		t.Fatalf("suggestions were mislabeled, duplicated, or included watched items: %+v", suggestions)
	}
	if history.ID != "youtube-activity" || len(history.Items) != youtubeActivityPageSize {
		t.Fatalf("suggestions changed the bounded history page: %+v", history)
	}
	if len(queries) != 2 || queries[0] != "Mechanical Keyboard Build" || queries[1] != "Acoustic Guitar Lesson" {
		t.Fatalf("suggestions did not use only the two most recent watch titles: %v", queries)
	}
	suggested := map[string]bool{}
	for _, item := range suggestions.Items {
		if item.ID == "youtube-aqz-KE-bpKQ" || item.ID == "youtube-M7lc1UVf-VE" || item.ID == "youtube-ScMzIvxBSi4" || suggested[item.ID] {
			t.Fatalf("suggestion was watched or repeated: %+v", item)
		}
		suggested[item.ID] = true
	}
	if !suggested["youtube-yS6v3U2X1a_"] || !suggested["youtube-AbCdEfGhIjK"] || !suggested["youtube-LmNoPqRsTuV"] {
		t.Fatalf("title search results were not retained: %+v", suggested)
	}
	source := s.searchSource(context.Background(), device, "youtube-yS6v3U2X1a_")
	if source == nil || !source.Item.Playable {
		t.Fatalf("suggestion source was not retained for playback: %+v", source)
	}
	if source := s.searchSource(context.Background(), otherDevice, "youtube-yS6v3U2X1a_"); source != nil {
		t.Fatalf("suggestion source leaked to another device: %+v", source)
	}

	second := call(s, "GET", "/v1/home?provider=youtube&activityCursor="+url.QueryEscape(firstPage.NextCursor), "", device, token, "")
	if second.Code != 200 {
		t.Fatalf("activity continuation: %d %s", second.Code, second.Body)
	}
	var secondPage struct {
		FeedType   string           `json:"feedType"`
		Sections   []domain.Section `json:"sections"`
		NextCursor string           `json:"nextCursor"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if secondPage.FeedType != "zombiebox_activity" || len(secondPage.Sections) != 1 ||
		secondPage.Sections[0].ID != "youtube-activity" || len(secondPage.Sections[0].Items) != 5 ||
		secondPage.NextCursor != "" {
		t.Fatalf("continuation did not remain scoped to history: %s", second.Body)
	}
	for _, item := range secondPage.Sections[0].Items {
		if suggested[item.ID] {
			t.Fatalf("suggestion was merged into the history cursor: %s", item.ID)
		}
	}

	other := call(s, "GET", "/v1/home?provider=youtube", "", otherDevice, otherToken, "")
	if other.Code != 200 {
		t.Fatalf("other device Home: %d %s", other.Code, other.Body)
	}
	var otherPage struct {
		Sections []domain.Section `json:"sections"`
	}
	if err := json.Unmarshal(other.Body.Bytes(), &otherPage); err != nil {
		t.Fatal(err)
	}
	if len(otherPage.Sections) != 1 || otherPage.Sections[0].ID != "youtube-activity" {
		t.Fatalf("self-only title search should leave this device history-only: %s", other.Body)
	}
	if len(queries) != 3 || queries[2] != "Solo Watched Video" {
		t.Fatalf("suggestions used another device's watch history: %v", queries)
	}
}

func putYouTubeActivityProgress(t *testing.T, s *Server, device string, progress domain.Progress) {
	t.Helper()
	if err := s.db.Put(context.Background(), "progress:"+device, progress.Item.ID, progress); err != nil {
		t.Fatal(err)
	}
}

func containsJSONItemID(body, id string) bool {
	var response struct {
		Sections []domain.Section `json:"sections"`
	}
	if json.Unmarshal([]byte(body), &response) != nil {
		return false
	}
	for _, section := range response.Sections {
		for _, item := range section.Items {
			if item.ID == id {
				return true
			}
		}
	}
	return false
}
