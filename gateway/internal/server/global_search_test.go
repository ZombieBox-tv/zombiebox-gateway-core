package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/providers"
)

func TestGlobalSearchUsesUpstreamQueriesAndPreservesPartialResults(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hubs/search":
			if r.URL.Query().Get("query") != "movie" || r.Header.Get("X-Plex-Token") != "private" {
				t.Error("lost query/credential")
			}
			fmt.Fprint(w, `<MediaContainer><Hub>`)
			for i := 0; i < 25; i++ {
				fmt.Fprintf(w, `<Video ratingKey="%d" title="Movie %d" type="movie"/>`, i, i)
			}
			fmt.Fprint(w, `</Hub></MediaContainer>`)
		case "/library/metadata/0":
			fmt.Fprint(w, `<MediaContainer><Video ratingKey="0" type="movie"><Media><Part key="/part.mp4"/></Media></Video></MediaContainer>`)
		default:
			w.WriteHeader(503)
		}
	}))
	defer upstream.Close()
	s := testServer(t, nil, t.TempDir())
	s.SeedProviders(context.Background(), map[string]providers.Config{
		"plex":     {Enabled: true, URL: upstream.URL, Token: "private"},
		"jellyfin": {Enabled: true, URL: upstream.URL, Token: "secret", UserID: "user"},
	})
	token := pair(t, s, "search-device")
	if w := call(s, "GET", "/v1/search?q=x", "", "search-device", token, ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	w := call(s, "GET", "/v1/search?q=movie", "", "search-device", token, "")
	var result struct{ Sections []searchSection }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body)
	}
	if len(result.Sections) != 6 || result.Sections[1].State != "READY" || result.Sections[2].State != "UNAVAILABLE" || !result.Sections[1].More {
		t.Fatal(result)
	}
	if len(result.Sections[1].Items) > 20 || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), upstream.URL) {
		t.Fatal("unbounded/private search data")
	}
	item := result.Sections[1].Items[0]
	if !item.Playable {
		t.Fatal("preview lost lazy playable metadata")
	}
	plan := call(s, "POST", "/v1/playback", `{"itemId":"`+item.ID+`"}`, "search-device", token, "")
	if plan.Code != 201 {
		t.Fatal(plan.Code, plan.Body)
	}
}
