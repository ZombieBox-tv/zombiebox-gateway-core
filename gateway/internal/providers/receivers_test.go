package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSpotifySemanticBoundary(t *testing.T) {
	var command string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			t.Error("missing private token")
		}
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			command = r.URL.Path + " " + string(b)
			return
		}
		io.WriteString(w, `{"username":"private-account","device_id":"private-id","stopped":false,"paused":true,"volume":40,"volume_steps":80,"track":{"name":"Song","album_cover_url":"https://i.scdn.co/image/fixture","artist_names":["Artist"],"duration":9000,"position":1200}}`)
	}))
	defer upstream.Close()
	c := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("t", 32)}
	status, err := testAdapters.SpotifyStatus(context.Background(), c)
	if err != nil || status.State != "PAUSED" || status.PositionMS != 1200 || status.Volume != 50 || status.Item.Title != "Song" {
		t.Fatalf("status: %+v %v", status, err)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "private-") || strings.Contains(string(raw), "i.scdn.co") {
		t.Fatal("upstream identity leaked")
	}
	sources, err := testAdapters.Fetch(context.Background(), "spotify", c, "")
	if err != nil || len(sources) != 1 || !sources[0].Live || sources[0].MIME != "audio/mpeg" {
		t.Fatalf("sources: %+v %v", sources, err)
	}
	if sources[0].ArtworkURL != "https://i.scdn.co/image/fixture" || len(sources[0].ArtworkHeaders) != 0 {
		t.Fatal("artwork lost or token forwarded to CDN")
	}
	if err = testAdapters.SpotifyCommand(context.Background(), c, PlayerCommand{Action: "seek", PositionMS: 1200}); err != nil {
		t.Fatal(err)
	}
	if command != `/player/seek {"position":1200}` {
		t.Fatal(command)
	}
	for _, bad := range []PlayerCommand{{Action: "../../token"}, {Action: "seek", PositionMS: -1}, {Action: "volume", Volume: 101}} {
		if testAdapters.SpotifyCommand(context.Background(), c, bad) == nil {
			t.Fatal("invalid command accepted")
		}
	}
}

func TestAirPlayIdleAndActive(t *testing.T) {
	active := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]bool{"active": active})
	}))
	defer upstream.Close()
	c := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("x", 32)}
	for _, value := range []bool{false, true} {
		active = value
		sources, err := testAdapters.AirPlay(context.Background(), c)
		if err != nil || len(sources) != 2 || sources[0].Item.Playable != value || !sources[0].Live {
			t.Fatalf("sources: %+v %v", sources, err)
		}
	}
}

func TestSpotifyStatusTrackMappingAndTransition(t *testing.T) {
	trackNum := 1
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("t", 32) {
			t.Error("missing private token")
		}
		if trackNum == 1 {
			io.WriteString(w, `{"stopped":false,"paused":false,"buffering":false,"volume":75,"volume_steps":100,"track":{"name":"Track One","album_cover_url":"https://i.scdn.co/image/track1","artist_names":["Artist A"],"duration":180000,"position":5000}}`)
		} else {
			io.WriteString(w, `{"stopped":false,"paused":false,"buffering":false,"volume":75,"volume_steps":100,"track":{"name":"Track Two","album_cover_url":"https://image-cdn-ak.spotifycdn.com/image/track2","artist_names":["Artist B","Artist C"],"duration":210000,"position":0}}`)
		}
	}))
	defer upstream.Close()

	c := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("t", 32)}

	status1, err := testAdapters.SpotifyStatus(context.Background(), c)
	if err != nil {
		t.Fatalf("status1 failed: %v", err)
	}
	if status1.State != "PLAYING" || status1.Item == nil || status1.Item.Title != "Track One" || status1.Item.Subtitle != "Artist A" || status1.Item.DurationMS != 180000 || status1.PositionMS != 5000 {
		t.Fatalf("unexpected status1: %+v", status1)
	}
	if status1.ArtworkURL != "https://i.scdn.co/image/track1" || !strings.HasPrefix(status1.Item.ImageURL, "/v1/artwork/spotify-connect?rev=") {
		t.Fatalf("unexpected artwork1: %q, %q", status1.ArtworkURL, status1.Item.ImageURL)
	}

	// Track transition
	trackNum = 2
	status2, err := testAdapters.SpotifyStatus(context.Background(), c)
	if err != nil {
		t.Fatalf("status2 failed: %v", err)
	}
	if status2.State != "PLAYING" || status2.Item == nil || status2.Item.Title != "Track Two" || status2.Item.Subtitle != "Artist B, Artist C" || status2.Item.DurationMS != 210000 || status2.PositionMS != 0 {
		t.Fatalf("unexpected status2: %+v", status2)
	}
	if status2.ArtworkURL != "https://image-cdn-ak.spotifycdn.com/image/track2" || !strings.HasPrefix(status2.Item.ImageURL, "/v1/artwork/spotify-connect?rev=") {
		t.Fatalf("unexpected artwork2: %q, %q", status2.ArtworkURL, status2.Item.ImageURL)
	}
	if status1.Item.ImageURL == status2.Item.ImageURL {
		t.Fatalf("artwork rev should change on new track: %s == %s", status1.Item.ImageURL, status2.Item.ImageURL)
	}
}

func TestSpotifyStatusIdle204NoContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	c := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("t", 32)}
	status, err := testAdapters.SpotifyStatus(context.Background(), c)
	if err != nil {
		t.Fatalf("expected nil error on 204 No Content, got: %v", err)
	}
	if status.State != "STOPPED" || status.Item == nil || status.Item.Title != "Spotify Connect" || !status.Item.Playable || status.ArtworkURL != "" {
		t.Fatalf("unexpected idle status on 204: %+v", status)
	}
}

func TestSpotifyStatusNoTrack(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"stopped":true,"paused":false,"buffering":false,"volume":50,"volume_steps":100,"track":null}`)
	}))
	defer upstream.Close()

	c := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("t", 32)}
	status, err := testAdapters.SpotifyStatus(context.Background(), c)
	if err != nil {
		t.Fatalf("expected nil error on stopped track: null, got: %v", err)
	}
	if status.State != "STOPPED" || status.Item == nil || status.Item.Title != "Spotify Connect" || !status.Item.Playable || status.ArtworkURL != "" {
		t.Fatalf("unexpected status on track: null: %+v", status)
	}
}

func TestSpotifyReceptionWaitsForCurrentTrack(t *testing.T) {
	var hasTrack atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasTrack.Load() {
			io.WriteString(w, `{"stopped":false,"buffering":true,"track":null}`)
			return
		}
		io.WriteString(w, `{"stopped":false,"track":{"name":"Ready song","artist_names":["Artist"]}}`)
	}))
	defer upstream.Close()

	config := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("t", 32)}
	source, state, err := testAdapters.Reception(context.Background(), "spotify", config)
	if err != nil || source != nil || state.State != "BUFFERING" || state.Item == nil || state.Item.Title != "Spotify Connect" {
		t.Fatalf("trackless player must not open a generic media stream: source=%v state=%+v err=%v", source, state, err)
	}

	hasTrack.Store(true)
	source, state, err = testAdapters.Reception(context.Background(), "spotify", config)
	if err != nil || source == nil || state.State != "PLAYING" || source.Item.Title != "Ready song" || source.MIME != "audio/mpeg" {
		t.Fatalf("loaded track must start reception: source=%v state=%+v err=%v", source, state, err)
	}
}

func TestSpotifyStatusCoverURLSecurity(t *testing.T) {
	var currentCover string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"stopped": false,
			"track": map[string]any{
				"name":            "Security Test",
				"album_cover_url": currentCover,
				"artist_names":    []string{"Test"},
				"duration":        60000,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	c := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("t", 32)}

	// HTTP upgraded to HTTPS
	currentCover = "http://i.scdn.co/image/insecure123"
	s, err := testAdapters.SpotifyStatus(context.Background(), c)
	if err != nil || s.ArtworkURL != "https://i.scdn.co/image/insecure123" {
		t.Fatalf("expected http upgraded to https, got %q (err %v)", s.ArtworkURL, err)
	}

	// Valid Spotify CDN subdomains
	for _, valid := range []string{
		"https://image-cdn-ak.spotifycdn.com/image/abc",
		"https://image-cdn-fa.scdn.co/image/def",
		"https://scdn.co/image/ghi",
		"https://spotifycdn.com/image/jkl",
	} {
		currentCover = valid
		s, err = testAdapters.SpotifyStatus(context.Background(), c)
		if err != nil || s.ArtworkURL != valid {
			t.Fatalf("expected %q accepted, got %q (err %v)", valid, s.ArtworkURL, err)
		}
	}

	// Invalid / malicious URLs must be rejected (empty ArtworkURL)
	for _, bad := range []string{
		"https://attacker.com/evil.png",
		"https://i.scdn.co.attacker.com/evil.png",
		"https://evil-scdn.co/fake.png",
		"https://user:pass@i.scdn.co/image/creds",
		"https://i.scdn.co:8443/image/badport",
		"javascript:alert(1)",
		"ftp://i.scdn.co/image",
		"http://evil.com/image.png",
	} {
		currentCover = bad
		s, err = testAdapters.SpotifyStatus(context.Background(), c)
		if err != nil {
			t.Fatalf("unexpected error on %q: %v", bad, err)
		}
		if s.ArtworkURL != "" {
			t.Fatalf("malicious url %q should have been rejected, got %q", bad, s.ArtworkURL)
		}
		if s.Item != nil && s.Item.ImageURL != "" {
			t.Fatalf("image url should be empty when artwork rejected for %q", bad)
		}
	}
}

func TestSpotifyStatusErrorHandling(t *testing.T) {
	statusCode := http.StatusOK
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
	}))
	defer upstream.Close()

	c := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("t", 32)}

	statusCode = http.StatusServiceUnavailable
	_, err := testAdapters.SpotifyStatus(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected 503 error, got: %v", err)
	}

	statusCode = http.StatusInternalServerError
	_, err = testAdapters.SpotifyStatus(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got: %v", err)
	}
}

func TestAirPlayMetadataOnlyAndPlayableReadiness(t *testing.T) {
	var (
		audioActive bool
		metadata    map[string]string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"active":      false,
			"audioActive": audioActive,
		}
		if metadata != nil {
			resp["metadata"] = metadata
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	c := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("a", 32)}

	// 1. Idle state: inactive, no metadata
	sources, err := testAdapters.AirPlay(context.Background(), c)
	if err != nil || len(sources) != 2 {
		t.Fatalf("expected 2 sources, got %v (err %v)", sources, err)
	}
	if sources[0].Item.Playable || sources[1].Item.Playable {
		t.Fatal("sources should not be playable when idle")
	}
	if sources[1].Item.Title != "AirPlay audio" || sources[1].ArtworkURL != "" {
		t.Fatalf("unexpected idle audio item: %+v", sources[1])
	}

	// 2. Metadata-only state: audioActive is false, but metadata is present (Apple Music with unplayable/stalled stream)
	metadata = map[string]string{
		"title":  "Sample Track",
		"artist": "Sample Artist",
		"album":  "Sample Album",
	}
	sources, err = testAdapters.AirPlay(context.Background(), c)
	if err != nil || len(sources) != 2 {
		t.Fatalf("expected 2 sources, got %v (err %v)", sources, err)
	}
	audioSource := sources[1]
	if audioSource.Item.Playable {
		t.Fatal("audio source must NOT be playable in metadata-only state")
	}
	if audioSource.Item.Title != "Sample Track" || audioSource.Item.Subtitle != "Sample Artist" || audioSource.Item.Description != "Sample Album" {
		t.Fatalf("audio item should contain metadata even when unplayable: %+v", audioSource.Item)
	}
	if audioSource.ArtworkURL == "" || !strings.Contains(audioSource.ArtworkURL, "/artwork?rev=") {
		t.Fatalf("artwork URL should be set in metadata-only state: %q", audioSource.ArtworkURL)
	}

	// Reception in metadata-only state: must not start playback!
	source, state, err := testAdapters.Reception(context.Background(), "airplay", c)
	if err != nil || source != nil || state.State != "STOPPED" {
		t.Fatalf("metadata-only AirPlay must not start playback: source=%v state=%+v err=%v", source, state, err)
	}

	// 3. Playable valid state: audioActive is true
	audioActive = true
	sources, err = testAdapters.AirPlay(context.Background(), c)
	if err != nil || len(sources) != 2 {
		t.Fatalf("expected 2 sources, got %v (err %v)", sources, err)
	}
	if !sources[1].Item.Playable {
		t.Fatal("audio source must be playable when audioActive is true")
	}
	if sources[1].Item.Title != "Sample Track" || sources[1].ArtworkURL == "" {
		t.Fatalf("playable audio source missing metadata: %+v", sources[1])
	}

	// Reception in playable state: starts playback
	source, state, err = testAdapters.Reception(context.Background(), "airplay", c)
	if err != nil || source == nil || state.State != "PLAYING" || source.Item.Title != "Sample Track" {
		t.Fatalf("playable AirPlay should start playback: source=%v state=%+v err=%v", source, state, err)
	}
}
