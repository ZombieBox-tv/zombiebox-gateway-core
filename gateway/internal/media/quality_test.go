package media

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if !strings.Contains(flags["-format_whitelist"], "matroska") || strings.Contains(flags["-format_whitelist"], "concat") || strings.Contains(flags["-format_whitelist"], "hls") {
		t.Fatal("unsafe local demuxers", flags)
	}
	if flags["-b:v"] != "400k" || flags["-maxrate"] != "500k" || flags["-b:a"] != "64k" || flags["-threads"] != "2" {
		t.Fatal(flags)
	}
	if err := tools.ConvertSelected(context.Background(), input, "TRANSCODE", domain.MediaSelection{Quality: "-custom"}, io.Discard); err == nil {
		t.Fatal("arbitrary profile accepted")
	}
}

func TestSplitRenditionUsesAudioInputDespitePreviousTrackIndex(t *testing.T) {
	runner := &qualityRunner{}
	tools := NewWithRunner("ffmpeg", "ffprobe", runner)
	previousTrack := 1
	selection := domain.MediaSelection{Quality: "720p", AudioID: &previousTrack}
	if err := tools.convert(t.Context(), "http://127.0.0.1/video", "http://127.0.0.1/audio", true, false, false, "REMUX", selection, io.Discard); err != nil {
		t.Fatal(err)
	}
	for index := 0; index+1 < len(runner.args); index++ {
		if runner.args[index] != "-map" {
			continue
		}
		if runner.args[index+1] == "1:a:0" {
			return
		}
		if runner.args[index+1] == "0:1" {
			t.Fatal("stale audio index mapped from the video-only input")
		}
	}
	t.Fatal("split audio input was not mapped")
}
