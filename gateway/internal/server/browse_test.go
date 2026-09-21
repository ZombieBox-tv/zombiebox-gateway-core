package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

func browseFixture(t *testing.T, s *Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (strings.HasPrefix(r.URL.Path, "/library/") || r.URL.Path == "/file.mp4") && r.Header.Get("X-Plex-Token") != "fixture-token" {
			t.Error("missing provider token")
		}
		switch r.URL.Path {
		case "/library/sections":
			fmt.Fprint(w, `<MediaContainer><Directory key="1" title="Movies" type="movie"/></MediaContainer>`)
		case "/library/sections/1/all":
			fmt.Fprint(w, `<MediaContainer><Video ratingKey="1" title="Movie" type="movie"><Media><Part key="/file.mp4"/></Media></Video></MediaContainer>`)
		case "/file.mp4":
			fmt.Fprint(w, "fixture-media")
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(upstream.Close)
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"plex": {Enabled: true, URL: upstream.URL, Token: "fixture-token"}}); err != nil {
		t.Fatal(err)
	}
}

func TestBrowseSourcesResolveToOwnedPlaybackAndInvalidateOnConfigChange(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	browseFixture(t, s)
	token := pair(t, s, "browse-device")
	other := pair(t, s, "other-device")
	root := call(s, "GET", "/v1/browse?provider=plex", "", "browse-device", token, "")
	if root.Code != 200 {
		t.Fatal(root.Body)
	}
	var page domain.BrowsePage
	json.Unmarshal(root.Body.Bytes(), &page)
	if len(page.Items) != 1 {
		t.Fatal(page)
	}
	parent := page.Items[0].BrowseID
	if w := call(s, "GET", "/v1/browse?provider=plex&parent="+parent, "", "other-device", other, ""); w.Code != 410 {
		t.Fatal("cross-device node", w.Code, w.Body)
	}
	children := call(s, "GET", "/v1/browse?provider=plex&parent="+parent, "", "browse-device", token, "")
	if children.Code != 200 {
		t.Fatal(children.Body)
	}
	json.Unmarshal(children.Body.Bytes(), &page)
	if len(page.Items) != 1 || !page.Items[0].Playable {
		t.Fatal(page)
	}
	plan := call(s, "POST", "/v1/playback", `{"itemId":"`+page.Items[0].ID+`"}`, "browse-device", token, "")
	if plan.Code != 201 || strings.Contains(plan.Body.String(), "fixture-token") {
		t.Fatal(plan.Body)
	}
	var playback domain.Plan
	json.Unmarshal(plan.Body.Bytes(), &playback)
	stream := call(s, "GET", playback.URL, "", "", "", "")
	if stream.Code != 200 || stream.Body.String() != "fixture-media" {
		t.Fatal(stream.Code, stream.Body)
	}
	s.mu.Lock()
	s.configRevision["plex"]++
	s.mu.Unlock()
	if w := call(s, "GET", "/v1/browse?provider=plex&parent="+parent, "", "browse-device", token, ""); w.Code != 410 {
		t.Fatal("stale config accepted", w.Body)
	}
	if source := s.browseSource(context.Background(), "browse-device", page.Items[0].ID); source != nil {
		t.Fatal("stale source accepted")
	}
}
