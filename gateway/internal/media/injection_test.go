package media

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type runFunc func(context.Context, string, []string, io.Writer) error

func (f runFunc) Run(c context.Context, p string, a []string, w io.Writer) error {
	return f(c, p, a, w)
}
func TestProbeCancellationReleasesInjectedRunnerCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	os.WriteFile(path, []byte{0}, 0600)
	runner := runFunc(func(ctx context.Context, _ string, _ []string, w io.Writer) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := io.WriteString(w, `{"streams":[{"codec_type":"video","codec_name":"h264"}]}`)
		return err
	})
	tools := NewWithRunner("missing-ffmpeg", "missing-ffprobe", runner)
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := tools.Probe(ctx, path); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	result, err := tools.Probe(t.Context(), path)
	if err != nil || len(result.Streams) != 1 {
		t.Fatalf("slot leaked: %v %v", result, err)
	}
}
