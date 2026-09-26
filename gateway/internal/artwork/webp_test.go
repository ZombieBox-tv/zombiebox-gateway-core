package artwork

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type fakeWebPEncoder struct {
	data    []byte
	err     error
	calls   int
	quality int
}

func (f *fakeWebPEncoder) Encode(_ context.Context, _ image.Image, quality int) ([]byte, error) {
	f.calls++
	f.quality = quality
	return append([]byte(nil), f.data...), f.err
}

type fakeArtworkProcess func(context.Context, string, []string, io.Writer) error

func (f fakeArtworkProcess) Run(ctx context.Context, executable string, args []string, output io.Writer) error {
	return f(ctx, executable, args, output)
}

type execArtworkProcess struct{}

func (execArtworkProcess) Run(ctx context.Context, executable string, args []string, output io.Writer) error {
	command := exec.CommandContext(ctx, executable, args...)
	command.Stdout = output
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	return command.Run()
}

func validWebPFixture() []byte {
	data := make([]byte, 30)
	copy(data[:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8 ")
	binary.LittleEndian.PutUint32(data[16:20], uint32(len(data)-20))
	copy(data[20:], []byte{0, 0, 0x9d, 0x01, 0x2a, 1, 0, 1, 0, 0})
	return data
}

func jpegFixture(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 24)), nil); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestWebPPreferenceUsesLossyOutputAndFallsBackToJPEG(t *testing.T) {
	valid := validWebPFixture()
	tooLarge := bytes.Repeat([]byte{0x55}, maxDerivativeBytes+1)
	for _, test := range []struct {
		name    string
		encoder WebPEncoder
		want    ImageFormat
	}{
		{name: "valid WebP", encoder: &fakeWebPEncoder{data: valid}, want: FormatWebP},
		{name: "encoder absent", want: FormatJPEG},
		{name: "encoder failure", encoder: &fakeWebPEncoder{err: errors.New("unavailable")}, want: FormatJPEG},
		{name: "invalid WebP", encoder: &fakeWebPEncoder{data: []byte("not webp")}, want: FormatJPEG},
		{name: "oversized WebP", encoder: &fakeWebPEncoder{data: tooLarge}, want: FormatJPEG},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := compressAs(t.Context(), jpegFixture(t), 32, 24, FormatWebP, test.encoder)
			if err != nil {
				t.Fatal(err)
			}
			if got := encodedFormat(data); got != test.want {
				t.Fatalf("format = %q, want %q", got, test.want)
			}
			if encoder, ok := test.encoder.(*fakeWebPEncoder); ok && encoder.calls != 1 {
				t.Fatalf("encoder calls = %d, want 1", encoder.calls)
			}
			if test.name == "valid WebP" {
				if got := test.encoder.(*fakeWebPEncoder).quality; got != 80 {
					t.Fatalf("WebP quality = %d, want lossy quality 80", got)
				}
			}
		})
	}
}

func TestInvalidArtworkIsRejectedBeforeWebPEncoder(t *testing.T) {
	encoder := &fakeWebPEncoder{data: validWebPFixture()}
	if _, err := compressAs(t.Context(), []byte("not an image"), 32, 24, FormatWebP, encoder); err == nil {
		t.Fatal("invalid input accepted")
	}
	if encoder.calls != 0 {
		t.Fatalf("encoder called %d times for invalid source", encoder.calls)
	}
}

func TestImagesCacheSeparatesPreferredFormats(t *testing.T) {
	original := jpegFixture(t)
	calls := 0
	client := doFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(original))}, nil
	})
	encoder := &fakeWebPEncoder{data: validWebPFixture()}
	images := NewWithWebPEncoder(client, nil, encoder)
	source := domain.Source{ArtworkURL: "https://artwork.invalid/cover"}
	for _, format := range []ImageFormat{FormatJPEG, FormatWebP, FormatJPEG, FormatWebP} {
		data, actual, err := images.ImageAs(t.Context(), source, domain.ArtworkLandscapeSmall, format)
		if err != nil || actual != format || encodedFormat(data) != format {
			t.Fatalf("preferred %s returned %s (%v)", format, actual, err)
		}
	}
	if calls != 2 || encoder.calls != 1 {
		t.Fatalf("format cache separation: upstream calls=%d, WebP encodes=%d", calls, encoder.calls)
	}
}

func TestImagesRejectsInvalidMemoryAndDiskCacheHits(t *testing.T) {
	original := jpegFixture(t)
	calls := 0
	client := doFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(original))}, nil
	})
	images := New(client, invalidImageCache{})
	source := domain.Source{ArtworkURL: "https://artwork.invalid/corrupt-cache"}
	if _, _, err := images.ImageAs(t.Context(), source, domain.ArtworkLandscapeSmall, FormatJPEG); err != nil {
		t.Fatal(err)
	}
	for key, cached := range images.cache {
		cached.data = []byte("corrupt cache entry")
		images.cache[key] = cached
	}
	data, actual, err := images.ImageAs(t.Context(), source, domain.ArtworkLandscapeSmall, FormatJPEG)
	if err != nil || actual != FormatJPEG || encodedFormat(data) != FormatJPEG || calls != 2 {
		t.Fatalf("corrupt cache entry was used: actual=%s calls=%d err=%v", actual, calls, err)
	}
}

type invalidImageCache struct{}

func (invalidImageCache) Get([32]byte) ([]byte, time.Time, bool) {
	return []byte("corrupt disk entry"), time.Now().Add(time.Hour), true
}

func (invalidImageCache) Put([32]byte, []byte, time.Time) {}

func TestDiskCacheSeparatesJPEGAndWebPRequests(t *testing.T) {
	original := jpegFixture(t)
	calls := 0
	client := doFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(original))}, nil
	})
	disk, err := NewDiskCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	source := domain.Source{ArtworkURL: "https://artwork.invalid/persisted"}
	first := NewWithWebPEncoder(client, disk, &fakeWebPEncoder{data: validWebPFixture()})
	for _, format := range []ImageFormat{FormatJPEG, FormatWebP} {
		if _, actual, err := first.ImageAs(t.Context(), source, domain.ArtworkLandscapeSmall, format); err != nil || actual != format {
			t.Fatalf("initial %s request returned %s (%v)", format, actual, err)
		}
	}
	restarted := NewWithWebPEncoder(client, disk, &fakeWebPEncoder{data: validWebPFixture()})
	for _, format := range []ImageFormat{FormatWebP, FormatJPEG} {
		if _, actual, err := restarted.ImageAs(t.Context(), source, domain.ArtworkLandscapeSmall, format); err != nil || actual != format {
			t.Fatalf("cached %s request returned %s (%v)", format, actual, err)
		}
	}
	if calls != 2 {
		t.Fatalf("disk cache did not separate then reuse formats; upstream calls=%d", calls)
	}
}

func TestFFmpegWebPEncoderUsesBoundedLossyProcessAndPrivateTemporaryInput(t *testing.T) {
	var tempPath string
	runner := fakeArtworkProcess(func(_ context.Context, executable string, args []string, output io.Writer) error {
		if executable != "ffmpeg" {
			t.Fatalf("executable = %q", executable)
		}
		arguments := strings.Join(args, " ")
		for _, expected := range []string{"-max_alloc 67108864", "-threads 1", "-frames:v 1", "-lossless 0", "-quality 80", "-pix_fmt yuv420p", "-f webp pipe:1"} {
			if !strings.Contains(arguments, expected) {
				t.Errorf("missing FFmpeg option %q in %q", expected, arguments)
			}
		}
		for index, argument := range args {
			if argument == "-i" && index+1 < len(args) {
				tempPath = args[index+1]
				break
			}
		}
		if tempPath == "" {
			t.Fatal("missing private PNG input path")
		}
		info, err := os.Stat(tempPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("temporary input permissions: info=%v err=%v", info, err)
		}
		file, err := os.Open(tempPath)
		if err != nil {
			t.Fatal(err)
		}
		config, err := png.DecodeConfig(file)
		_ = file.Close()
		if err != nil || config.Width != 240 || config.Height != 135 {
			t.Fatalf("unexpected bounded input: %dx%d %v", config.Width, config.Height, err)
		}
		_, err = output.Write(validWebPFixture())
		return err
	})
	encoder := NewFFmpegWebPEncoder("ffmpeg", runner)
	data, err := encoder.Encode(t.Context(), image.NewRGBA(image.Rect(0, 0, 240, 135)), 80)
	if err != nil || encodedFormat(data) != FormatWebP {
		t.Fatalf("encode returned %s (%v)", encodedFormat(data), err)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary input was not removed: %v", err)
	}
}

func TestFFmpegWebPEncoderRejectsOversizedOutputAndHonorsCancellation(t *testing.T) {
	encoder := NewFFmpegWebPEncoder("ffmpeg", fakeArtworkProcess(func(_ context.Context, _ string, _ []string, output io.Writer) error {
		_, _ = output.Write(make([]byte, maxDerivativeBytes+1))
		return nil
	}))
	if _, err := encoder.Encode(t.Context(), image.NewRGBA(image.Rect(0, 0, 8, 8)), 80); err == nil {
		t.Fatal("oversized process output accepted")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := false
	blockingEncoder := NewFFmpegWebPEncoder("ffmpeg", fakeArtworkProcess(func(ctx context.Context, _ string, _ []string, _ io.Writer) error {
		called = true
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}))
	if _, err := blockingEncoder.Encode(ctx, image.NewRGBA(image.Rect(0, 0, 8, 8)), 80); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled process returned %v", err)
	}
	if !called {
		t.Fatal("process runner was not called")
	}
}

func TestFFmpegWebPEncoderWithInstalledLibWebP(t *testing.T) {
	executable, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is optional")
	}
	encoders, err := exec.Command(executable, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil || !strings.Contains(string(encoders), "libwebp") {
		t.Skip("installed ffmpeg does not expose libwebp")
	}
	encoder := NewFFmpegWebPEncoder(executable, execArtworkProcess{})
	data, err := encoder.Encode(t.Context(), image.NewRGBA(image.Rect(0, 0, 240, 135)), 80)
	if err != nil {
		t.Fatal(err)
	}
	if encodedFormat(data) != FormatWebP || len(data) > maxDerivativeBytes {
		t.Fatalf("unexpected actual FFmpeg output: format=%s bytes=%d", encodedFormat(data), len(data))
	}
}

func TestNewFFmpegWebPEncoderIsOptionalWithoutExecutableOrRunner(t *testing.T) {
	if NewFFmpegWebPEncoder("", fakeArtworkProcess(func(context.Context, string, []string, io.Writer) error { return nil })) != nil {
		t.Fatal("empty executable should disable WebP")
	}
	if NewFFmpegWebPEncoder("ffmpeg", nil) != nil {
		t.Fatal("missing process runner should disable WebP")
	}
	if _, err := (FFmpegWebPEncoder{}).Encode(context.Background(), image.NewRGBA(image.Rect(0, 0, 1, 1)), 80); err == nil {
		t.Fatal("unconfigured encoder unexpectedly worked")
	}
}
