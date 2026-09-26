package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/artwork"
	"zombiebox.local/gateway/internal/domain"
)

type testWebPEncoder struct{ data []byte }

func (e testWebPEncoder) Encode(context.Context, image.Image, int) ([]byte, error) {
	return append([]byte(nil), e.data...), nil
}

type failingTestWebPEncoder struct{}

func (failingTestWebPEncoder) Encode(context.Context, image.Image, int) ([]byte, error) {
	return nil, errors.New("WebP unavailable")
}

func testWebPBytes() []byte {
	data := make([]byte, 30)
	copy(data[:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8 ")
	binary.LittleEndian.PutUint32(data[16:20], uint32(len(data)-20))
	copy(data[20:], []byte{0, 0, 0x9d, 0x01, 0x2a, 1, 0, 1, 0, 0})
	return data
}

func TestPrivateArtworkIsReplacedByAuthenticatedDerivative(t *testing.T) {
	var encoded bytes.Buffer
	jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1200, 1200)), nil)
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
	webp := testWebPBytes()
	s.deps.Artwork = artwork.NewWithWebPEncoder(http.DefaultClient, nil, testWebPEncoder{data: webp})
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
	if w.Code != 200 || err != nil || format != "jpeg" || c.Width > 320 || c.Height > 180 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal(w.Code, c, err)
	}
	api13WebPRequest := call(s, "GET", screen.Hero.Item.ImageURL+"&format=webp", "", "artwork-device", token, "")
	if api13WebPRequest.Code != 200 || api13WebPRequest.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("API 13 must remain JPEG: status=%d content-type=%q", api13WebPRequest.Code, api13WebPRequest.Header().Get("Content-Type"))
	}
	audioURL := screen.Hero.Item.ImageURL + "&size=audio"
	audio := call(s, "GET", audioURL, "", "artwork-device", token, "")
	audioConfig, audioFormat, audioErr := image.DecodeConfig(bytes.NewReader(audio.Body.Bytes()))
	if audio.Code != 200 || audioErr != nil || audioFormat != "jpeg" || audioConfig.Width != 600 || audioConfig.Height != 600 || len(audio.Body.Bytes()) > 256<<10 {
		t.Fatalf("audio artwork response: %d %dx%d %s %d bytes %v", audio.Code, audioConfig.Width, audioConfig.Height, audioFormat, audio.Body.Len(), audioErr)
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

	api14Registration := call(s, "POST", "/v1/devices/register", `{"clientVersion":"test","protocolVersion":1,"installationId":"artwork-device","pairingCode":"123456","platform":{"androidApi":14},"display":{"dpad":true}}`, "", "", "")
	if api14Registration.Code != http.StatusCreated {
		t.Fatalf("API 14 re-pair: %d %s", api14Registration.Code, api14Registration.Body)
	}
	var api14Body struct{ DeviceToken string }
	if err := json.Unmarshal(api14Registration.Body.Bytes(), &api14Body); err != nil {
		t.Fatal(err)
	}
	api14 := call(s, "GET", screen.Hero.Item.ImageURL+"&format=webp", "", "artwork-device", api14Body.DeviceToken, "")
	if api14.Code != 200 || api14.Header().Get("Content-Type") != "image/webp" || !bytes.Equal(api14.Body.Bytes(), webp) {
		t.Fatalf("API 14 WebP response: status=%d content-type=%q", api14.Code, api14.Header().Get("Content-Type"))
	}
	s.deps.Artwork = artwork.NewWithWebPEncoder(http.DefaultClient, nil, failingTestWebPEncoder{})
	fallback := call(s, "GET", screen.Hero.Item.ImageURL+"&format=webp", "", "artwork-device", api14Body.DeviceToken, "")
	_, fallbackFormat, fallbackErr := image.DecodeConfig(bytes.NewReader(fallback.Body.Bytes()))
	if fallback.Code != 200 || fallback.Header().Get("Content-Type") != "image/jpeg" || fallbackErr != nil || fallbackFormat != "jpeg" {
		t.Fatalf("WebP failure fallback: status=%d content-type=%q format=%q err=%v", fallback.Code, fallback.Header().Get("Content-Type"), fallbackFormat, fallbackErr)
	}
}

func TestArtworkBudgetsUseRegistrationAndFiniteProfiles(t *testing.T) {
	device := domain.Device{}
	if artworkProfile(device, "hero") != domain.ArtworkHeroSmall || artworkProfile(device, "audio") != domain.ArtworkAudioSmall {
		t.Fatal("unknown device must use conservative budget")
	}
	device.Registration = domain.Registration{Memory: domain.Memory{PhysicalMB: 2048, ClassMB: 256}, Display: domain.Display{Width: 1920, Height: 1080}}
	if artworkProfile(device, "hero") != domain.ArtworkHeroMedium || artworkProfile(device, "poster") != domain.ArtworkPosterMedium || artworkProfile(device, "audio") != domain.ArtworkAudioMedium || artworkProfile(device, "") != domain.ArtworkLandscapeMedium {
		t.Fatal("standard device")
	}
	device.Registration.Memory.PhysicalMB = 512
	if artworkProfile(device, "poster") != domain.ArtworkPosterSmall || artworkProfile(device, "audio") != domain.ArtworkAudioSmall || artworkProfile(device, "100000x100000") != domain.ArtworkLandscapeSmall {
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
