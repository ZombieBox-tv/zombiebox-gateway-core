package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestYouTubeBrowseRelatedExcludesCurrentVideoAndPreservesMetadata(t *testing.T) {
	currentID := "aqz-KE-bpKQ"
	relatedID := "rel12345678"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/browse" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("parent") != "" {
			t.Errorf("upstream parent should be empty, got: %s", r.URL.Query().Get("parent"))
		}
		if r.URL.Query().Get("q") != currentID {
			t.Errorf("upstream q should be %s, got: %s", currentID, r.URL.Query().Get("q"))
		}
		// Return both the current video and a related video with description and duration
		fmt.Fprintf(w, `{"items":[
			{"id":"%s","kind":"video","title":"Current Video","subtitle":"Channel A","description":"Current description","durationMs":120000},
			{"id":"%s","kind":"video","title":"Related Video","subtitle":"Channel B","description":"Related description","durationMs":180000}
		],"nextOffset":20}`, currentID, relatedID)
	}))
	defer upstream.Close()

	page, err := testAdapters.Browse(context.Background(), "youtube", Config{URL: upstream.URL, Token: strings.Repeat("s", 32)}, "related:"+currentID, "", 0)
	if err != nil {
		t.Fatalf("browse related: %v", err)
	}

	// Current video must be excluded!
	if len(page.Sources) != 1 {
		t.Fatalf("expected 1 related item (current video excluded), got %d", len(page.Sources))
	}
	item := page.Sources[0].Item
	if item.ID != "youtube-"+relatedID {
		t.Errorf("expected item ID youtube-%s, got %s", relatedID, item.ID)
	}
	if item.Title != "Related Video" {
		t.Errorf("expected title 'Related Video', got %s", item.Title)
	}
	if item.Subtitle != "Channel B" {
		t.Errorf("expected subtitle 'Channel B', got %s", item.Subtitle)
	}
	if item.Description != "Related description" {
		t.Errorf("expected description 'Related description', got %s", item.Description)
	}
	if item.DurationMS != 180000 {
		t.Errorf("expected durationMs 180000, got %d", item.DurationMS)
	}
	if !item.Playable {
		t.Errorf("expected playable true")
	}
	if page.NextOffset != 20 {
		t.Errorf("expected nextOffset 20, got %d", page.NextOffset)
	}
}

func TestYouTubeBrowseRelatedRejectsInvalidParent(t *testing.T) {
	_, err := testAdapters.Browse(context.Background(), "youtube", Config{URL: "http://127.0.0.1:1", Token: strings.Repeat("s", 32)}, "related:invalid_too_short", "", 0)
	if err == nil {
		t.Fatal("expected error for invalid related parent")
	}
}
