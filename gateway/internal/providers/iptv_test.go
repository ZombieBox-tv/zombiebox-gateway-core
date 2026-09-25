package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIPTVSourceStates(t *testing.T) {
	// 1. No source: Enabled=true without URL or PlaylistPath returns ErrPlaylistUnconfigured.
	noSource := Config{Enabled: true}
	sources, err := testAdapters.IPTV(context.Background(), noSource)
	if !errors.Is(err, ErrPlaylistUnconfigured) {
		t.Fatalf("expected ErrPlaylistUnconfigured, got: %v", err)
	}
	if sources != nil {
		t.Fatalf("expected nil sources on unconfigured IPTV, got: %+v", sources)
	}

	// 2. Source configured but unreachable: returns a distinct network/unavailable error.
	unreachable := Config{Enabled: true, URL: "http://127.0.0.1:1/nonexistent.m3u"}
	_, err = testAdapters.IPTV(context.Background(), unreachable)
	if err == nil {
		t.Fatal("expected error for unreachable URL")
	}
	if errors.Is(err, ErrPlaylistUnconfigured) {
		t.Fatal("unreachable URL must not return ErrPlaylistUnconfigured")
	}

	// 3. Disabled provider: Fetch returns empty list and no error.
	disabled := Config{Enabled: false}
	fetched, err := testAdapters.Fetch(context.Background(), "iptv", disabled, "")
	if err != nil || len(fetched) != 0 {
		t.Fatalf("expected empty sources for disabled IPTV, got sources=%+v err=%v", fetched, err)
	}

	// 4. Valid configured source: parses M3U channels correctly.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:-1 tvg-id=\"chan1\",Channel 1\nhttp://example.test/stream.m3u8\n")
	}))
	defer server.Close()

	valid := Config{Enabled: true, URL: server.URL}
	validSources, err := testAdapters.IPTV(context.Background(), valid)
	if err != nil {
		t.Fatalf("unexpected error for valid source: %v", err)
	}
	if len(validSources) != 1 || validSources[0].Item.Title != "Channel 1" {
		t.Fatalf("expected 1 parsed channel, got: %+v", validSources)
	}
}
