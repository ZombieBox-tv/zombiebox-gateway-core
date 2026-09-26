package artwork

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestLanczosDownscaleFiltersGradient(t *testing.T) {
	const sourceSize = 1200
	source := image.NewRGBA(image.Rect(0, 0, sourceSize, sourceSize))
	for y := 0; y < sourceSize; y++ {
		for x := 0; x < sourceSize; x++ {
			// The smooth slope checks the resize mapping; alternating detail should
			// average away when reducing by two rather than alias into the output.
			red := uint8(30 + x/20 + (x%2)*40)
			green := uint8(20 + y/20)
			source.SetRGBA(x, y, color.RGBA{R: red, G: green, B: 90, A: 255})
		}
	}

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	derivative, err := compress(t.Context(), encoded.Bytes(), 600, 600)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(derivative))
	if err != nil {
		t.Fatal(err)
	}
	red, _, _, _ := decoded.At(200, 300).RGBA()
	filteredRed := int(red >> 8)
	if filteredRed < 64 || filteredRed > 76 {
		t.Fatalf("filtered gradient sample = %d, want near 70 with alternating detail averaged out", filteredRed)
	}
	if len(derivative) > maxDerivativeBytes {
		t.Fatalf("gradient derivative exceeds encoded limit: %d bytes", len(derivative))
	}
}

func TestComplexArtworkFitsEncodedBudget(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 700, 700))
	state := uint32(0x6d2b79f5)
	for y := 0; y < source.Bounds().Dy(); y++ {
		for x := 0; x < source.Bounds().Dx(); x++ {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			source.SetRGBA(x, y, color.RGBA{
				R: uint8(state >> 24),
				G: uint8(state >> 16),
				B: uint8(state >> 8),
				A: 255,
			})
		}
	}

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	derivative, err := compress(t.Context(), encoded.Bytes(), 600, 600)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(derivative))
	if err != nil || format != "jpeg" || config.Width != 600 || config.Height != 600 {
		t.Fatalf("unexpected complex derivative: %dx%d %s %v", config.Width, config.Height, format, err)
	}
	if len(derivative) > maxDerivativeBytes {
		t.Fatalf("complex derivative exceeds encoded limit: %d bytes", len(derivative))
	}
}
