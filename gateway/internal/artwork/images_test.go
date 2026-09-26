package artwork

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

type doFunc func(*http.Request) (*http.Response, error)

func (f doFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func TestDerivativeBoundsCacheAndCredentials(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	picture.Set(0, 0, color.White)
	var encoded bytes.Buffer
	jpeg.Encode(&encoded, picture, nil)
	calls := 0
	images := New(doFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("X-Plex-Token") == "" {
			t.Error("missing credentials")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encoded.Bytes()))}, nil
	}), nil)
	source := domain.Source{ArtworkURL: "http://fixture.invalid/image", ArtworkHeaders: http.Header{"X-Plex-Token": {"private"}}}
	for i := 0; i < 2; i++ {
		data, err := images.Image(t.Context(), source, domain.ArtworkLandscapeMedium)
		if err != nil {
			t.Fatal(err)
		}
		c, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || format != "jpeg" || c.Width > 320 || c.Height > 180 {
			t.Fatal(c, format, err)
		}
	}
	if calls != 1 {
		t.Fatal("derivative not cached", calls)
	}
	source.ArtworkHeaders.Set("X-Plex-Token", "rotated")
	images.Image(t.Context(), source, domain.ArtworkLandscapeMedium)
	if calls != 2 {
		t.Fatal("credential rotation reused old derivative")
	}
}
func TestRejectsNonImage(t *testing.T) {
	images := New(doFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString("<html>private upstream error</html>"))}, nil
	}), nil)
	if _, err := images.Image(t.Context(), domain.Source{ArtworkURL: "http://fixture.invalid/image"}, domain.ArtworkLandscapeMedium); err == nil {
		t.Fatal("HTML relayed as image")
	}
}

func TestAudioDerivativeKeepsSquareArtWithinBudget(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 1200, 1200))
	for y := 0; y < picture.Bounds().Dy(); y++ {
		for x := 0; x < picture.Bounds().Dx(); x++ {
			picture.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x * y), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, picture, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	images := New(doFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(encoded.Bytes()))}, nil
	}), nil)

	data, err := images.Image(t.Context(), domain.Source{ArtworkURL: "http://fixture.invalid/album.jpg"}, domain.ArtworkAudioSmall)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" || config.Width != 600 || config.Height != 600 {
		t.Fatalf("unexpected audio derivative: %dx%d %s %v", config.Width, config.Height, format, err)
	}
	if len(data) > 256<<10 {
		t.Fatalf("audio derivative exceeds encoded limit: %d bytes", len(data))
	}
}

func TestCancelledFollowerDoesNotCancelSharedImage(t *testing.T) {
	var encoded bytes.Buffer
	_ = jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 32)), nil)
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	images := New(doFunc(func(*http.Request) (*http.Response, error) {
		calls++
		close(entered)
		<-release
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encoded.Bytes()))}, nil
	}), nil)
	source := domain.Source{ArtworkURL: "http://fixture.invalid/shared.jpg"}
	done := make(chan error, 1)
	go func() {
		_, err := images.Image(t.Context(), source, domain.ArtworkLandscapeSmall)
		done <- err
	}()
	<-entered
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := images.Image(cancelled, source, domain.ArtworkLandscapeSmall)
	if !errors.Is(err, context.Canceled) {
		t.Error("follower did not honor cancellation", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal("owner failed", err)
	}
	if _, err := images.Image(t.Context(), source, domain.ArtworkLandscapeSmall); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("duplicate upstream request", calls)
	}
}
