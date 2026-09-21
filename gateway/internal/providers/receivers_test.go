package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		io.WriteString(w, `{"username":"private-account","device_id":"private-id","stopped":false,"paused":true,"volume":40,"volume_steps":80,"track":{"name":"Song","artist_names":["Artist"],"duration":9000,"position":1200}}`)
	}))
	defer upstream.Close()
	c := Config{Enabled: true, URL: upstream.URL, Token: strings.Repeat("t", 32)}
	status, err := testAdapters.SpotifyStatus(context.Background(), c)
	if err != nil || status.State != "PAUSED" || status.PositionMS != 1200 || status.Volume != 50 || status.Item.Title != "Song" {
		t.Fatalf("status: %+v %v", status, err)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "private-") {
		t.Fatal("upstream identity leaked")
	}
	sources, err := testAdapters.Fetch(context.Background(), "spotify", c, "")
	if err != nil || len(sources) != 1 || !sources[0].Live || sources[0].MIME != "audio/mpeg" {
		t.Fatalf("sources: %+v %v", sources, err)
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
