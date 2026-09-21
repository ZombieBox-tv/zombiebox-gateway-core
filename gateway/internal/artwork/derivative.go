package artwork

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	_ "image/png"

	"zombiebox.local/gateway/internal/domain"
)

func profileBounds(profile domain.ArtworkProfile) (int, int, bool) {
	width, height := 0, 0
	switch profile {
	case domain.ArtworkLandscapeSmall:
		width, height = 240, 135
	case domain.ArtworkLandscapeMedium:
		width, height = 320, 180
	case domain.ArtworkHeroSmall:
		width, height = 640, 360
	case domain.ArtworkHeroMedium:
		width, height = 960, 540
	case domain.ArtworkPosterSmall:
		width, height = 180, 270
	case domain.ArtworkPosterMedium:
		width, height = 320, 480
	default:
		return 0, 0, false
	}
	return width, height, true
}

func compress(ctx context.Context, data []byte, width, height int) ([]byte, error) {
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
	return output.Bytes(), nil
}
