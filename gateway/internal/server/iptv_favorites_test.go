package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
	"zombiebox.local/gateway/internal/store"
)

func TestIPTVFavoritesPersistAndResolveCurrentChannels(t *testing.T) {
	var removed atomic.Bool
	playlist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n")
		if !removed.Load() {
			io.WriteString(w, "#EXTINF:-1 group-title=\"News\",News\nhttps://media.example.test/live.m3u8?secret=one\n")
		}
	}))
	defer playlist.Close()
	path := filepath.Join(t.TempDir(), "gateway.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t, db, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"iptv": {Enabled: true, URL: playlist.URL},
	}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "favorite-tv")
	page := call(s, "GET", "/v1/catalog?provider=iptv", "", "favorite-tv", token, "")
	var catalog struct{ Items []domain.Item }
	if page.Code != 200 || json.Unmarshal(page.Body.Bytes(), &catalog) != nil || len(catalog.Items) != 1 {
		t.Fatalf("catalog: %d %s", page.Code, page.Body)
	}
	id := catalog.Items[0].ID
	if catalog.Items[0].Category != "News" {
		t.Fatal("playlist category was not normalized")
	}
	var grouped struct {
		Items      []domain.Item
		Categories []string
	}
	byCategory := call(s, "GET", "/v1/catalog?provider=iptv&category=News", "", "favorite-tv", token, "")
	if byCategory.Code != 200 || json.Unmarshal(byCategory.Body.Bytes(), &grouped) != nil || len(grouped.Items) != 1 || len(grouped.Categories) != 1 || grouped.Categories[0] != "News" {
		t.Fatalf("category list: %d %s", byCategory.Code, byCategory.Body)
	}
	if w := call(s, "GET", "/v1/catalog?provider=iptv&category=Sports", "", "favorite-tv", token, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatal("unrelated category returned channels", w.Code, w.Body)
	}
	if catalog.Items[0].Favorite {
		t.Fatal("new channel is already favorite")
	}
	if w := call(s, "PUT", "/v1/iptv/favorites/iptv-deadbeefdeadbeef", "", "favorite-tv", token, ""); w.Code != 404 {
		t.Fatal("accepted channel absent from current playlist")
	}
	if w := call(s, "PUT", "/v1/iptv/favorites/"+id, "", "favorite-tv", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	home := call(s, "GET", "/v1/home?provider=iptv", "", "favorite-tv", token, "")
	var screen domain.Screen
	if home.Code != 200 || json.Unmarshal(home.Body.Bytes(), &screen) != nil || len(screen.Sections) != 1 || !screen.Sections[0].Items[0].Favorite {
		t.Fatalf("home favorite flag: %d %s", home.Code, home.Body)
	}
	favorites := call(s, "GET", "/v1/catalog?provider=iptv&favorites=1", "", "favorite-tv", token, "")
	if favorites.Code != 200 || json.Unmarshal(favorites.Body.Bytes(), &catalog) != nil || len(catalog.Items) != 1 || !catalog.Items[0].Favorite {
		t.Fatalf("favorites: %d %s", favorites.Code, favorites.Body)
	}
	if w := call(s, "GET", "/v1/catalog?provider=plex&favorites=1", "", "favorite-tv", token, ""); w.Code != 400 {
		t.Fatal("favorites filter accepted for another provider")
	}
	s.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s = testServer(t, db, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"iptv": {Enabled: true, URL: playlist.URL},
	}); err != nil {
		t.Fatal(err)
	}
	favorites = call(s, "GET", "/v1/catalog?provider=iptv&favorites=1", "", "favorite-tv", token, "")
	if favorites.Code != 200 || json.Unmarshal(favorites.Body.Bytes(), &catalog) != nil || len(catalog.Items) != 1 || !catalog.Items[0].Favorite {
		t.Fatalf("favorites after restart: %d %s", favorites.Code, favorites.Body)
	}
	removed.Store(true)
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"iptv": {Enabled: true, URL: playlist.URL},
	}); err != nil {
		t.Fatal(err)
	}
	favorites = call(s, "GET", "/v1/catalog?provider=iptv&favorites=1", "", "favorite-tv", token, "")
	if favorites.Code != 200 || json.Unmarshal(favorites.Body.Bytes(), &catalog) != nil || len(catalog.Items) != 0 {
		t.Fatalf("removed channel leaked from favorites: %d %s", favorites.Code, favorites.Body)
	}
	if w := call(s, "DELETE", "/v1/iptv/favorites/"+id, "", "favorite-tv", token, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	favorites = call(s, "GET", "/v1/catalog?provider=iptv&favorites=1", "", "favorite-tv", token, "")
	if favorites.Code != 200 || json.Unmarshal(favorites.Body.Bytes(), &catalog) != nil || len(catalog.Items) != 0 {
		t.Fatalf("favorites after removal: %d %s", favorites.Code, favorites.Body)
	}
}
