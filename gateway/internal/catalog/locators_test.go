package catalog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/store"
)

func TestRestartResolvesFreshMediaWithoutPersistingCredentials(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	calls := 0
	backend := browseBackend(func(_ context.Context, provider string, _ domain.Config, parent, query string, offset int) (domain.BrowseResult, error) {
		calls++
		if parent == "" {
			return domain.BrowseResult{Sources: []domain.Source{{BrowsePath: "library", Item: domain.Item{ID: "library", Provider: provider, Title: "Library"}}}}, nil
		}
		return domain.BrowseResult{Sources: []domain.Source{{URL: "https://private.test/rotating-token", Item: domain.Item{ID: "episode", Provider: provider, Title: "Episode", Playable: true}}}}, nil
	})
	config := domain.Config{Enabled: true, Token: "private-secret"}
	first := NewPersistentBrowser(backend, db)
	page, err := first.Page(ctx, "device", "plex", "Plex", 1, config, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	folder := page.Items[0].BrowseID
	restarted := NewPersistentBrowser(backend, db)
	page, err = restarted.Page(ctx, "device", "plex", "Plex", 0, config, folder, "", 0)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	restarted = NewPersistentBrowser(backend, db)
	source, ok := restarted.Resolve(ctx, "device", "episode", config, 0)
	if !ok || source.Source.URL == "" || calls != 3 {
		t.Fatal(source, ok, calls)
	}
	if _, ok := restarted.Resolve(ctx, "other-device", "episode", config, 0); ok {
		t.Fatal("cross-device locator")
	}
	config.Token = "different-account"
	if _, ok := restarted.Resolve(ctx, "device", "episode", config, 0); ok {
		t.Fatal("old account locator")
	}
	values, _ := db.List(ctx, "browse-locators")
	data, _ := json.Marshal(values)
	if strings.Contains(string(data), "private-secret") || strings.Contains(string(data), "rotating-token") || strings.Contains(string(data), "private.test") {
		t.Fatal("credential/source persisted", string(data))
	}
}
