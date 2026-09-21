package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type browseBackend func(context.Context, string, domain.Config, string, string, int) (domain.BrowseResult, error)

func (b browseBackend) Browse(c context.Context, p string, config domain.Config, parent, q string, offset int) (domain.BrowseResult, error) {
	return b(c, p, config, parent, q, offset)
}

func TestOpaqueBrowseNodesAreDeviceAndRevisionScoped(t *testing.T) {
	now := time.Now()
	calls := 0
	browser := NewBrowser(browseBackend(func(_ context.Context, provider string, _ domain.Config, parent, query string, offset int) (domain.BrowseResult, error) {
		calls++
		if parent == "" {
			return domain.BrowseResult{Sources: []domain.Source{{BrowsePath: "secret/provider/path", Item: domain.Item{ID: "library", Provider: provider, Title: "Library"}}}, NextOffset: -1}, nil
		}
		if parent != "secret/provider/path" || query != "title" || offset != 40 {
			t.Fatal("lost node or paging context", parent, query, offset)
		}
		return domain.BrowseResult{Sources: []domain.Source{{URL: "https://private.test/stream?token=secret", ArtworkURL: "https://private.test/art", Item: domain.Item{ID: "episode-1", Provider: provider, Title: "Episode", Playable: true}}}, NextOffset: -1}, nil
	}))
	browser.clock = func() time.Time { return now }
	root, err := browser.Page(context.Background(), "device-a", "plex", "Plex", 1, domain.Config{}, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := root.Items[0].BrowseID
	if id == "" || root.Items[0].Playable {
		t.Fatal("invalid folder", root)
	}
	for _, tuple := range []struct {
		device, provider string
		revision         uint64
	}{{"device-b", "plex", 1}, {"device-a", "jellyfin", 1}, {"device-a", "plex", 2}} {
		if _, err := browser.Page(context.Background(), tuple.device, tuple.provider, "", tuple.revision, domain.Config{}, id, "", 0); !errors.Is(err, ErrExpired) {
			t.Fatal("node escaped scope", err)
		}
	}
	page, err := browser.Page(context.Background(), "device-a", "plex", "Plex", 1, domain.Config{}, id, "title", 40)
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "Library" || page.Items[0].ID != "episode-1" || page.Items[0].ImageURL != "/v1/artwork/episode-1" {
		t.Fatal(page)
	}
	body, _ := json.Marshal(page)
	if strings.Contains(string(body), "private.test") || strings.Contains(string(body), "secret") {
		t.Fatal("provider internals leaked")
	}
	if _, ok := browser.Source("device-b", "episode-1"); ok {
		t.Fatal("source crossed devices")
	}
	if calls != 2 {
		t.Fatal("invalid nodes reached backend")
	}
	now = now.Add(31 * time.Minute)
	if _, ok := browser.Source("device-a", "episode-1"); ok {
		t.Fatal("expired source accepted")
	}
	if _, err := browser.Page(context.Background(), "device-a", "plex", "", 1, domain.Config{}, id, "", 0); !errors.Is(err, ErrExpired) {
		t.Fatal("expired node accepted")
	}
}
