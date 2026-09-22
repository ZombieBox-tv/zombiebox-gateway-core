package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestStremioSearchCatalogContinuationKeepsQueryAndPaginates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.json" {
			fmt.Fprint(w, `{"catalogs":[{"id":"movies","type":"movie","name":"Movies","extra":[{"name":"skip"},{"name":"search"}]}]}`)
			return
		}
		extra := strings.TrimSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/catalog/movie/movies/"), ".json")
		values, err := url.ParseQuery(extra)
		if err != nil || (values.Get("search") != "nature & sea" && values.Get("search") != "replacement") {
			t.Error("search scope lost", r.URL.Path)
		}
		start, count := 0, 100
		if values.Get("skip") == "100" {
			start, count = 100, 1
		}
		items := []map[string]string{}
		for i := start; i < start+count; i++ {
			items = append(items, map[string]string{"id": fmt.Sprint(i), "name": fmt.Sprint(i)})
		}
		json.NewEncoder(w).Encode(map[string]any{"metas": items})
	}))
	defer upstream.Close()
	config := Config{URL: upstream.URL}
	root, err := testAdapters.Browse(context.Background(), "stremio", config, "", "nature & sea", 0)
	if err != nil || len(root.Sources) != 40 || root.NextOffset != 40 {
		t.Fatal(root, err)
	}
	folder := root.Sources[0]
	if folder.BrowsePath == "" || folder.Item.Playable || folder.Item.Title != "Movies" {
		t.Fatal("missing scoped continuation", folder)
	}
	for _, offset := range []int{0, 40, 80, 100} {
		page, err := testAdapters.Browse(context.Background(), "stremio", config, folder.BrowsePath, "", offset)
		if err != nil || len(page.Sources) == 0 || page.Sources[0].Item.Title != fmt.Sprint(offset) {
			t.Fatal(offset, page, err)
		}
		if offset == 100 && page.NextOffset != -1 {
			t.Fatal("did not terminate", page)
		}
	}
	if _, err := testAdapters.Browse(context.Background(), "stremio", config, folder.BrowsePath, "replacement", 0); err != nil {
		t.Fatal(err)
	}
}

func TestStremioSearchDoesNotInventContinuationForExhaustedCatalog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.json" {
			fmt.Fprint(w, `{"catalogs":[{"id":"small","type":"movie","extra":[{"name":"search"}]}]}`)
		} else {
			fmt.Fprint(w, `{"metas":[{"id":"one","name":"Only result"}]}`)
		}
	}))
	defer upstream.Close()
	page, err := testAdapters.Browse(context.Background(), "stremio", Config{URL: upstream.URL}, "", "query", 0)
	if err != nil || len(page.Sources) != 1 || page.NextOffset != -1 || page.Sources[0].BrowsePath != "" {
		t.Fatal(page, err)
	}
}
