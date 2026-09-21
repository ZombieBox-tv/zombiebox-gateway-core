// Package artwork creates small JPEG derivatives; provider URLs never reach Android.
package artwork

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"sync"
	"time"
	"zombiebox.local/gateway/internal/domain"
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}
type entry struct {
	data    []byte
	expires time.Time
}
type Images struct {
	http  HTTPClient
	jobs  chan struct{}
	mu    sync.Mutex
	cache map[[32]byte]entry
}

func New(client HTTPClient) *Images {
	return &Images{http: client, jobs: make(chan struct{}, 2), cache: map[[32]byte]entry{}}
}
func (i *Images) Image(ctx context.Context, source domain.Source, hero bool) ([]byte, error) {
	if source.ArtworkURL == "" {
		return nil, errors.New("no artwork")
	}
	width, height := 320, 180
	if hero {
		width, height = 960, 540
	}
	raw, _ := http.NewRequest("GET", source.ArtworkURL, nil)
	if raw == nil || (raw.URL.Scheme != "http" && raw.URL.Scheme != "https") || raw.URL.User != nil {
		return nil, errors.New("invalid artwork")
	}
	keyInput := source.ArtworkURL + "\n" + source.ArtworkHeaders.Get("Authorization") + "\n" + source.ArtworkHeaders.Get("X-Plex-Token") + "\n" + source.ArtworkHeaders.Get("X-Emby-Token")
	if hero {
		keyInput += "\nhero"
	}
	key := sha256.Sum256([]byte(keyInput))
	i.mu.Lock()
	cached, ok := i.cache[key]
	i.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.data, nil
	}
	select {
	case i.jobs <- struct{}{}:
		defer func() { <-i.jobs }()
	default:
		return nil, errors.New("artwork busy")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", source.ArtworkURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = source.ArtworkHeaders.Clone()
	response, err := i.http.Do(req)
	if err != nil {
		return nil, errors.New("artwork unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, errors.New("artwork unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("artwork too large")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") || config.Width <= 0 || config.Height <= 0 || config.Width > 4096 || config.Height > 4096 || config.Width*config.Height > 4_000_000 {
		return nil, errors.New("unsupported artwork")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid artwork")
	}
	// Fit without distortion, never upscale. The view chooses a crop for its layout.
	if width > config.Width {
		width = config.Width
	}
	height = min(height, max(1, config.Height*width/config.Width))
	width = min(width, max(1, config.Width*height/config.Height))
	resized := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := decoded.Bounds()
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < width; x++ {
			resized.Set(x, y, decoded.At(bounds.Min.X+x*config.Width/width, bounds.Min.Y+y*config.Height/height))
		}
	}
	var output bytes.Buffer
	if jpeg.Encode(&output, resized, &jpeg.Options{Quality: 75}) != nil || output.Len() > 256<<10 {
		return nil, errors.New("derivative too large")
	}
	result := output.Bytes()
	i.mu.Lock()
	if len(i.cache) >= 32 {
		for k := range i.cache {
			delete(i.cache, k)
			break
		}
	}
	i.cache[key] = entry{result, time.Now().Add(15 * time.Minute)}
	i.mu.Unlock()
	return result, nil
}
