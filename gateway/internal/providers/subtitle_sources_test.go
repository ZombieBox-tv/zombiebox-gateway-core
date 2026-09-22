package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderSubtitleAttachments(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/recentlyAdded":
			_, _ = w.Write([]byte(`<MediaContainer><Video ratingKey="1"><Media><Part key="/video"><Stream streamType="3" codec="srt" key="/subs" languageCode="spa" forced="1"/><Stream streamType="3" codec="srt" key="https://attacker.invalid/leak"/></Part></Media></Video></MediaContainer>`))
		case "/Users/user/Items":
			if r.URL.Query().Get("Fields") != "Overview,MediaSources" {
				t.Error("subtitle metadata not requested")
			}
			_, _ = w.Write([]byte(`{"Items":[{"Id":"1","Name":"Movie","Type":"Movie","MediaSources":[{"Id":"primary","MediaStreams":[{"Index":2,"Type":"Subtitle","Codec":"srt","IsExternal":true,"Language":"es"},{"Index":3,"Type":"Subtitle","Codec":"pgssub","IsExternal":true}]}]}]}`))
		case "/stream/movie/1.json":
			_, _ = w.Write([]byte(`{"streams":[{"url":"https://media.example/video.mp4","subtitles":[{"url":"https://subs.example/sub.vtt","lang":"eng"},{"url":"file:///private","lang":"es"}]}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	a := New(upstream.Client(), upstream.Client())
	c := Config{URL: upstream.URL, Token: "private", UserID: "user"}
	plex, err := a.Plex(context.Background(), c)
	if err != nil || len(plex) != 1 || len(plex[0].Subtitles) != 1 {
		t.Fatalf("plex: %v %+v", err, plex)
	}
	p := plex[0].Subtitles[0]
	if p.URL != upstream.URL+"/subs" || p.Headers.Get("X-Plex-Token") != "private" || !p.Forced {
		t.Fatal("lost scoped Plex attachment")
	}
	jellyfin, err := a.Jellyfin(context.Background(), c)
	if err != nil || len(jellyfin) != 1 || len(jellyfin[0].Subtitles) != 1 {
		t.Fatalf("jellyfin: %v %+v", err, jellyfin)
	}
	j := jellyfin[0].Subtitles[0]
	if j.URL != upstream.URL+"/Videos/1/primary/Subtitles/2/Stream.srt" || j.Headers.Get("X-Emby-Token") != "private" {
		t.Fatal("wrong Jellyfin attachment")
	}
	stremio, err := a.Resolve(context.Background(), Source{MIME: "application/x-zombie-stremio", URL: upstream.URL + "/stream/movie/1.json"})
	if err != nil || len(stremio.Subtitles) != 1 || len(stremio.Subtitles[0].Headers) != 0 {
		t.Fatal("invalid Stremio attachments", err)
	}
}

func TestSubtitleOriginAndCountBounds(t *testing.T) {
	for _, reference := range []string{"//other.invalid/sub", "https://other.invalid/sub", "file:///private", "http://user:secret@media.example/sub", "/sub#fragment"} {
		if sameOriginSubtitle("http://media.example/base", reference) != "" {
			t.Fatal("accepted unsafe origin", reference)
		}
	}
	entries := make([]stremioSubtitle, 100)
	for i := range entries {
		entries[i] = stremioSubtitle{URL: "https://subs.example/a.srt"}
	}
	if len(stremioSubtitles(entries)) != 32 {
		t.Fatal("unbounded attachments")
	}
}
