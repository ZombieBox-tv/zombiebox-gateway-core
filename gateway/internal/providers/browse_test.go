package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestYouTubeBrowseUsesEmptyQueryForProviderHome(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/browse" || r.URL.Query().Get("q") != "" || r.URL.Query().Get("parent") != "" {
			t.Errorf("unexpected browse request: %s", r.URL.String())
		}
		fmt.Fprint(w, `{"items":[{"id":"aqz-KE-bpKQ","kind":"video","title":"Explore video"}],"nextOffset":-1}`)
	}))
	defer upstream.Close()
	page, err := testAdapters.Browse(context.Background(), "youtube", Config{URL: upstream.URL, Token: strings.Repeat("s", 32)}, "", "", 0)
	if err != nil || len(page.Sources) != 1 || !page.Sources[0].Item.Playable {
		t.Fatalf("YouTube provider Home: %+v, %v", page, err)
	}
}

func TestPlexBrowseLibrariesChildrenAndPagination(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "fixture" {
			t.Error("token missing")
		}
		switch r.URL.Path {
		case "/library/sections":
			fmt.Fprint(w, `<MediaContainer><Directory key="1" title="TV" type="show"/></MediaContainer>`)
		case "/library/sections/1/all":
			if r.URL.Query().Get("X-Plex-Container-Size") != "40" {
				t.Error("unbounded request")
			}
			fmt.Fprint(w, `<MediaContainer totalSize="42"><Directory ratingKey="show" title="Show" type="show"/></MediaContainer>`)
		case "/library/metadata/show/children":
			fmt.Fprint(w, `<MediaContainer><Directory ratingKey="season" title="Season 1" type="season"/></MediaContainer>`)
		case "/library/metadata/season/children":
			fmt.Fprint(w, `<MediaContainer><Video ratingKey="episode" title="Episode" type="episode"><Media><Part key="/library/parts/1/file.mp4"/></Media></Video></MediaContainer>`)
		default:
			t.Error("unexpected path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	c := Config{URL: upstream.URL, Token: "fixture"}
	parent := ""
	for i, want := range []string{"section:1", "metadata:show", "metadata:season", ""} {
		page, err := testAdapters.Browse(context.Background(), "plex", c, parent, "", 0)
		if err != nil || len(page.Sources) != 1 {
			t.Fatal(page, err)
		}
		source := page.Sources[0]
		if source.BrowsePath != want {
			t.Fatal(source)
		}
		if i == 1 && page.NextOffset != 1 {
			t.Fatal("lost upstream pagination", page)
		}
		if i == 3 && (!source.Item.Playable || source.URL != upstream.URL+"/library/parts/1/file.mp4") {
			t.Fatal(source)
		}
		parent = source.BrowsePath
	}
}

func TestJellyfinBrowseUsesViewsAndParentId(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "fixture" {
			t.Error("token missing")
		}
		if r.URL.Path == "/Users/user/Views" {
			fmt.Fprint(w, `{"Items":[{"Id":"tv","Name":"TV","Type":"CollectionFolder"}],"TotalRecordCount":1}`)
			return
		}
		q := r.URL.Query()
		if q.Get("ParentId") != "tv" || q.Get("StartIndex") != "40" || q.Get("Limit") != "40" {
			t.Error("lost paging", q)
		}
		fmt.Fprint(w, `{"Items":[{"Id":"episode","Name":"Episode","Type":"Episode","RunTimeTicks":10000000}],"TotalRecordCount":42}`)
	}))
	defer upstream.Close()
	c := Config{URL: upstream.URL, Token: "fixture", UserID: "user"}
	root, err := testAdapters.Browse(context.Background(), "jellyfin", c, "", "", 0)
	if err != nil || len(root.Sources) != 1 {
		t.Fatal(root, err)
	}
	page, err := testAdapters.Browse(context.Background(), "jellyfin", c, root.Sources[0].BrowsePath, "", 40)
	if err != nil || len(page.Sources) != 1 || page.NextOffset != 41 || !page.Sources[0].Item.Playable || page.Sources[0].Item.DurationMS != 1000 {
		t.Fatal(page, err)
	}
}

func TestStremioBrowsePreservesUpstreamSkipAndEpisodeVideoIDs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/manifest.json":
			fmt.Fprint(w, `{"catalogs":[{"id":"shows","type":"series","name":"Shows","extra":[{"name":"skip"},{"name":"search"}]}]}`)
		case strings.HasPrefix(path, "/catalog/series/shows"):
			start := 0
			if strings.Contains(path, "skip=100") {
				start = 100
			} else if path != "/catalog/series/shows.json" {
				t.Error("unexpected upstream offset", path)
			}
			metas := []map[string]string{}
			for i := start; i < start+100; i++ {
				metas = append(metas, map[string]string{"id": fmt.Sprint(i), "name": fmt.Sprint(i)})
			}
			json.NewEncoder(w).Encode(map[string]any{"metas": metas})
		case path == "/meta/series/0.json":
			fmt.Fprint(w, `{"meta":{"id":"0","videos":[{"id":"0:2:1","season":2,"episode":1,"title":"Second season"},{"id":"0:1:2","season":1,"episode":2,"title":"Two"},{"id":"0:1:1","season":1,"episode":1,"title":"One"}]}}`)
		default:
			t.Error("unexpected path", path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	c := Config{URL: upstream.URL}
	ctx := context.Background()
	root, err := testAdapters.Browse(ctx, "stremio", c, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := root.Sources[0].BrowsePath
	var first string
	for _, offset := range []int{0, 40, 80, 100} {
		page, err := testAdapters.Browse(ctx, "stremio", c, parent, "", offset)
		if err != nil {
			t.Fatal(err)
		}
		count, next := 40, offset+40
		if offset == 80 {
			count, next = 20, 100
		}
		if len(page.Sources) != count || page.NextOffset != next || page.Sources[0].Item.Title != fmt.Sprint(offset) {
			t.Fatal(offset, page)
		}
		if offset == 0 {
			first = page.Sources[0].BrowsePath
		}
	}
	seasons, err := testAdapters.Browse(ctx, "stremio", c, first, "", 0)
	if err != nil || len(seasons.Sources) != 2 {
		t.Fatal(seasons, err)
	}
	episodes, err := testAdapters.Browse(ctx, "stremio", c, seasons.Sources[0].BrowsePath, "", 0)
	if err != nil || len(episodes.Sources) != 2 || episodes.Sources[0].URL != upstream.URL+"/stream/series/0:1:1.json" || !episodes.Sources[0].Item.Playable {
		t.Fatal(episodes, err)
	}
}
