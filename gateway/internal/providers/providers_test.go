package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlaylistMetadataAndSchemes(t *testing.T) {
	sources, err := ParseM3U([]byte("#EXTM3U\n#EXTINF:-1 tvg-id=\"bbc\" tvg-name=\"BBC, UK\",BBC, World\n/live.m3u8\n#EXTINF:-1,Bad\nfile:///etc/passwd\n"), "https://example.test/list")
	if err != nil || len(sources) != 1 || sources[0].Item.Title != "BBC, World" || sources[0].EPGID != "bbc" || sources[0].URL != "https://example.test/live.m3u8" {
		t.Fatalf("%+v %v", sources, err)
	}
}
func TestXMLTVTimeZonesAndExpiredPrograms(t *testing.T) {
	now := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)
	guide, err := ParseXMLTV([]byte(`<tv><programme channel="bbc" start="20260920113000 -0600" stop="20260920123000 -0600"><title>News &amp; weather</title></programme><programme channel="bbc" start="20260919000000 +0000" stop="20260919010000 +0000"><title>Old</title></programme></tv>`), now)
	if err != nil || len(guide["bbc"]) != 1 || guide["bbc"][0].Title != "News & weather" || guide["bbc"][0].Start != now.Add(-30*time.Minute).Unix() {
		t.Fatal(guide, err)
	}
}
func TestRedirectDropsCrossOriginTokens(t *testing.T) {
	leaked := make(chan bool, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/final" {
			http.Redirect(w, r, "/final", 302)
			return
		}
		leaked <- r.Header.Get("Authorization") != "" || r.Header.Get("X-Plex-Token") != ""
		fmt.Fprint(w, "ok")
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer origin.Close()
	body, err := testAdapters.request(context.Background(), origin.URL, http.Header{"Authorization": []string{"Bearer secret"}, "X-Plex-Token": []string{"secret"}})
	if err != nil || string(body) != "ok" || <-leaked {
		t.Fatal("redirect leaked credentials or failed", err)
	}
}
func TestPlaylistBound(t *testing.T) {
	var body strings.Builder
	body.WriteString("#EXTM3U\n")
	for i := 0; i < 5100; i++ {
		fmt.Fprintf(&body, "#EXTINF:-1,Channel %d\nhttps://example.test/%d.ts\n", i, i)
	}
	sources, e := ParseM3U([]byte(body.String()), "")
	if e != nil || len(sources) != 5000 {
		t.Fatal(len(sources), e)
	}
}
