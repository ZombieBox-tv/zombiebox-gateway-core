package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestPrivateArtworkIsReplacedByAuthenticatedDerivative(t *testing.T) {
	var encoded bytes.Buffer
	jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 640, 360)), nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "private-token" {
			t.Error("credential missing")
		}
		if r.URL.Path == "/cover.jpg" {
			w.Write(encoded.Bytes())
			return
		}
		w.Write([]byte(`<MediaContainer><Video ratingKey="1" title="Fixture" thumb="/cover.jpg"><Media><Part key="/media.mp4"/></Media></Video></MediaContainer>`))
	}))
	defer upstream.Close()
	s := testServer(t, nil, "")
	s.SeedProviders(context.Background(), map[string]domain.Config{"plex": {Enabled: true, URL: upstream.URL, Token: "private-token"}})
	token := pair(t, s, "artwork-device")
	home := call(s, "GET", "/v1/home", "", "artwork-device", token, "")
	if strings.Contains(home.Body.String(), upstream.URL) || strings.Contains(home.Body.String(), "private-token") {
		t.Fatal("provider details leaked")
	}
	var screen domain.Screen
	json.Unmarshal(home.Body.Bytes(), &screen)
	if screen.Hero == nil || !strings.HasPrefix(screen.Hero.Item.ImageURL, "/v1/artwork/plex-1?rev=") {
		t.Fatal(home.Body)
	}
	if w := call(s, "GET", screen.Hero.Item.ImageURL, "", "", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := call(s, "GET", screen.Hero.Item.ImageURL, "", "artwork-device", token, "")
	c, format, err := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if w.Code != 200 || err != nil || format != "jpeg" || c.Width > 320 || c.Height > 180 {
		t.Fatal(w.Code, c, err)
	}
}
