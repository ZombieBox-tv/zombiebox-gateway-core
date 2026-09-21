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
	etag := w.Header().Get("ETag")
	if etag == "" || w.Header().Get("Cache-Control") != "private, max-age=300" {
		t.Fatal("missing cache validators")
	}
	request := httptest.NewRequest("GET", screen.Hero.Item.ImageURL, nil)
	request.Header.Set("X-Zombie-Device", "artwork-device")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("If-None-Match", "W/"+etag)
	conditional := httptest.NewRecorder()
	s.ServeHTTP(conditional, request)
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Fatal("conditional request", conditional.Code)
	}
	request.Header.Del("Authorization")
	unauthorized := httptest.NewRecorder()
	s.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatal("cached derivative bypassed authentication")
	}
}

func TestArtworkBudgetsUseRegistrationAndFiniteProfiles(t *testing.T) {
	device := domain.Device{}
	if artworkProfile(device, "hero") != domain.ArtworkHeroSmall {
		t.Fatal("unknown device must use conservative budget")
	}
	device.Registration = domain.Registration{Memory: domain.Memory{PhysicalMB: 2048, ClassMB: 256}, Display: domain.Display{Width: 1920, Height: 1080}}
	if artworkProfile(device, "hero") != domain.ArtworkHeroMedium || artworkProfile(device, "poster") != domain.ArtworkPosterMedium {
		t.Fatal("standard device")
	}
	device.Registration.Memory.PhysicalMB = 512
	if artworkProfile(device, "poster") != domain.ArtworkPosterSmall || artworkProfile(device, "100000x100000") != domain.ArtworkLandscapeSmall {
		t.Fatal("low-memory allocation must remain bounded")
	}
}

func TestCredentialRotationChangesPublicArtworkRevision(t *testing.T) {
	sources := []domain.Source{{ArtworkURL: "https://fixture.invalid/cover.jpg", ArtworkHeaders: http.Header{"Cookie": {"session=first"}}, Item: domain.Item{ID: "cover", Title: "Cover"}}}
	decorateArtwork(sources)
	before := sources[0].Item.ImageURL
	sources[0].ArtworkHeaders.Set("Cookie", "session=second")
	decorateArtwork(sources)
	if before == sources[0].Item.ImageURL || strings.Contains(sources[0].Item.ImageURL, "session=") {
		t.Fatal("revision did not isolate credentials")
	}
}
