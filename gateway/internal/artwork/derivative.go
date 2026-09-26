package artwork

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"strconv"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

const (
	maxDerivativeBytes = 256 << 10
	maxEncoderInput    = 4 << 20
	maxJPEGQuality     = 88
	minJPEGQuality     = 32
	lanczosRadius      = 3
)

type ImageFormat string

const (
	FormatJPEG ImageFormat = "jpeg"
	FormatWebP ImageFormat = "webp"
)

// WebPEncoder receives only a decoded, resized image. It cannot access provider
// URLs or credentials, and the caller validates its bounded encoded result.
type WebPEncoder interface {
	Encode(context.Context, image.Image, int) ([]byte, error)
}

// ArtworkProcessRunner is the narrow subprocess boundary used by the optional
// FFmpeg encoder. Media conversion remains owned by the existing media runner.
type ArtworkProcessRunner interface {
	Run(context.Context, string, []string, io.Writer) error
}

type FFmpegWebPEncoder struct {
	executable string
	runner     ArtworkProcessRunner
}

// NewFFmpegWebPEncoder returns nil when no usable process boundary is wired.
// This keeps WebP optional for Edge and for deployments without FFmpeg.
func NewFFmpegWebPEncoder(executable string, runner ArtworkProcessRunner) WebPEncoder {
	if executable == "" || runner == nil {
		return nil
	}
	return FFmpegWebPEncoder{executable: executable, runner: runner}
}

func (e FFmpegWebPEncoder) Encode(ctx context.Context, source image.Image, quality int) ([]byte, error) {
	if e.executable == "" || e.runner == nil {
		return nil, errors.New("WebP encoder unavailable")
	}
	if quality < 0 || quality > 100 {
		return nil, errors.New("invalid WebP quality")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var pngData boundedBuffer
	pngData.limit = maxEncoderInput
	if err := png.Encode(&pngData, source); err != nil {
		return nil, err
	}
	if pngData.overflow || pngData.Len() == 0 {
		return nil, errors.New("artwork encoder input too large")
	}
	file, err := os.CreateTemp("", "zombie-artwork-webp-*.png")
	if err != nil {
		return nil, errors.New("artwork encoder input unavailable")
	}
	fileName := file.Name()
	defer os.Remove(fileName)
	written, writeErr := file.Write(pngData.Bytes())
	closeErr := file.Close()
	if writeErr != nil || written != pngData.Len() || closeErr != nil {
		return nil, errors.New("artwork encoder input unavailable")
	}

	var output boundedBuffer
	output.limit = maxDerivativeBytes
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-max_alloc", "67108864",
		"-threads", "1", "-filter_threads", "1", "-protocol_whitelist", "file,pipe",
		"-f", "image2", "-i", fileName, "-map", "0:v:0", "-frames:v", "1",
		"-an", "-sn", "-dn", "-map_metadata", "-1", "-threads", "1", "-c:v", "libwebp",
		"-lossless", "0", "-quality", strconv.Itoa(quality), "-compression_level", "4",
		"-pix_fmt", "yuv420p", "-f", "webp", "pipe:1",
	}
	if err := e.runner.Run(ctx, e.executable, args, &output); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("artwork WebP encoding failed")
	}
	if output.overflow || encodedFormat(output.Bytes()) != FormatWebP {
		return nil, errors.New("invalid artwork WebP derivative")
	}
	return output.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 && len(data) > 0 {
		b.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	if len(data) > remaining {
		b.overflow = true
		written, _ := b.Buffer.Write(data[:remaining])
		return written, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(data)
}

func encodedFormat(data []byte) ImageFormat {
	if len(data) >= 4 && data[0] == 0xff && data[1] == 0xd8 && data[len(data)-2] == 0xff && data[len(data)-1] == 0xd9 {
		return FormatJPEG
	}
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" || string(data[12:16]) != "VP8 " {
		return ""
	}
	if int(binary.LittleEndian.Uint32(data[4:8])) != len(data)-8 || int(binary.LittleEndian.Uint32(data[16:20])) != len(data)-20 {
		return ""
	}
	return FormatWebP
}

type sampleWeight struct {
	index  int
	weight float64
}

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
	case domain.ArtworkAudioSmall:
		width, height = 600, 600
	case domain.ArtworkAudioMedium:
		width, height = 800, 800
	default:
		return 0, 0, false
	}
	return width, height, true
}

func compress(ctx context.Context, data []byte, width, height int) ([]byte, error) {
	result, err := compressAs(ctx, data, width, height, FormatJPEG, nil)
	return result, err
}

func compressAs(ctx context.Context, data []byte, width, height int, preferred ImageFormat, encoder WebPEncoder) ([]byte, error) {
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
	resized, err := resizeLanczos(ctx, decoded, width, height)
	if err != nil {
		return nil, err
	}
	if preferred == FormatWebP && encoder != nil {
		webp, encodeErr := encoder.Encode(ctx, resized, 80)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if encodeErr == nil && len(webp) <= maxDerivativeBytes && encodedFormat(webp) == FormatWebP {
			return webp, nil
		}
	}
	return encodeJPEGWithinBudget(ctx, resized)
}

// resizeLanczos filters each axis separately. The intermediate stores one
// destination-width row for each source row, which bounds working memory while
// applying a scaled Lanczos-3 low-pass filter to avoid downsampling aliasing.
func resizeLanczos(ctx context.Context, source image.Image, width, height int) (*image.RGBA, error) {
	sourceBounds := source.Bounds()
	sourceWidth, sourceHeight := sourceBounds.Dx(), sourceBounds.Dy()
	if width <= 0 || height <= 0 || width > sourceWidth || height > sourceHeight {
		return nil, errors.New("invalid artwork dimensions")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Decode each source pixel once. JPEG's YCbCr At conversion inside every
	// Lanczos tap exceeded the artwork request deadline even for a 600px cover.
	input := image.NewRGBA(image.Rect(0, 0, sourceWidth, sourceHeight))
	draw.Draw(input, input.Bounds(), source, sourceBounds.Min, draw.Src)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	horizontalWeights := axisWeights(sourceWidth, width)
	verticalWeights := axisWeights(sourceHeight, height)
	intermediate := make([]uint8, width*sourceHeight*4)

	for y := 0; y < sourceHeight; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x, weights := range horizontalWeights {
			var red, green, blue, alpha float64
			for _, sample := range weights {
				pixel := input.PixOffset(sample.index, y)
				red += float64(input.Pix[pixel]) * sample.weight
				green += float64(input.Pix[pixel+1]) * sample.weight
				blue += float64(input.Pix[pixel+2]) * sample.weight
				alpha += float64(input.Pix[pixel+3]) * sample.weight
			}
			offset := (y*width + x) * 4
			intermediate[offset] = channelByte(red * 257)
			intermediate[offset+1] = channelByte(green * 257)
			intermediate[offset+2] = channelByte(blue * 257)
			intermediate[offset+3] = channelByte(alpha * 257)
		}
	}

	resized := image.NewRGBA(image.Rect(0, 0, width, height))
	for y, weights := range verticalWeights {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < width; x++ {
			var red, green, blue, alpha float64
			for _, sample := range weights {
				offset := (sample.index*width + x) * 4
				red += float64(intermediate[offset]) * sample.weight
				green += float64(intermediate[offset+1]) * sample.weight
				blue += float64(intermediate[offset+2]) * sample.weight
				alpha += float64(intermediate[offset+3]) * sample.weight
			}
			offset := resized.PixOffset(x, y)
			resized.Pix[offset] = channelByte(red * 257)
			resized.Pix[offset+1] = channelByte(green * 257)
			resized.Pix[offset+2] = channelByte(blue * 257)
			resized.Pix[offset+3] = channelByte(alpha * 257)
		}
	}
	return resized, nil
}

func axisWeights(sourceLength, destinationLength int) [][]sampleWeight {
	weights := make([][]sampleWeight, destinationLength)
	scale := float64(sourceLength) / float64(destinationLength)
	filterScale := max(1, scale)
	support := lanczosRadius * filterScale
	for destination := range weights {
		center := (float64(destination)+0.5)*scale - 0.5
		first := max(0, int(math.Ceil(center-support)))
		last := min(sourceLength-1, int(math.Floor(center+support)))
		weights[destination] = make([]sampleWeight, 0, last-first+1)
		total := 0.0
		for source := first; source <= last; source++ {
			distance := (center - float64(source)) / filterScale
			weight := lanczos(distance)
			if weight == 0 {
				continue
			}
			weights[destination] = append(weights[destination], sampleWeight{index: source, weight: weight})
			total += weight
		}
		for sample := range weights[destination] {
			weights[destination][sample].weight /= total
		}
	}
	return weights
}

func lanczos(value float64) float64 {
	value = math.Abs(value)
	if value == 0 {
		return 1
	}
	if value >= lanczosRadius {
		return 0
	}
	return sinc(value) * sinc(value/lanczosRadius)
}

func sinc(value float64) float64 {
	angle := math.Pi * value
	return math.Sin(angle) / angle
}

func channelByte(value float64) uint8 {
	value = math.Max(0, math.Min(65535, value))
	return uint8(math.Round(value / 257))
}

func encodeJPEGWithinBudget(ctx context.Context, image image.Image) ([]byte, error) {
	for quality := maxJPEGQuality; quality >= minJPEGQuality; quality -= 8 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var output bytes.Buffer
		if err := jpeg.Encode(&output, image, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		if output.Len() <= maxDerivativeBytes {
			return output.Bytes(), nil
		}
	}
	return nil, errors.New("derivative too large")
}
