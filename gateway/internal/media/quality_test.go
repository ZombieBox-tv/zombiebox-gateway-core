package media

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

type qualityRunner struct{ args []string }

func (r *qualityRunner) Run(_ context.Context, _ string, args []string, _ io.Writer) error {
	r.args = args
	return nil
}
func TestLowBandwidthProfileIsBoundedAndRejectsArbitraryInput(t *testing.T) {
	input := filepath.Join(t.TempDir(), "media.mp4")
	if err := os.WriteFile(input, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &qualityRunner{}
	tools := NewWithRunner("ffmpeg", "ffprobe", runner)
	if err := tools.ConvertSelected(context.Background(), input, "TRANSCODE", domain.MediaSelection{Quality: "LOW"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	flags := map[string]string{}
	for i := 0; i+1 < len(runner.args); i++ {
		flags[runner.args[i]] = runner.args[i+1]
	}
	if flags["-b:v"] != "400k" || flags["-maxrate"] != "500k" || flags["-b:a"] != "64k" || flags["-threads"] != "2" {
		t.Fatal(flags)
	}
	if err := tools.ConvertSelected(context.Background(), input, "TRANSCODE", domain.MediaSelection{Quality: "-custom"}, io.Discard); err == nil {
		t.Fatal("arbitrary profile accepted")
	}
}
