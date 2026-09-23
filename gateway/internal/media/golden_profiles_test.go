package media

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercise actual FFmpeg/FFprobe output across the baseline local formats and
// resolutions. Device decoder support is a separate physical capability probe.
func TestGoldenLocalMediaProfiles(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}
	profiles := []struct {
		name, size, extension, audio string
		width, height                int
	}{
		{"mp4-480-aac", "854x480", ".mp4", "aac", 854, 480},
		{"mkv-720-ac3", "1280x720", ".mkv", "ac3", 1280, 720},
		{"ts-1080-aac", "1920x1080", ".ts", "aac", 1920, 1080},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "sample"+profile.extension)
			cmd := exec.CommandContext(ctx, ffmpeg,
				"-v", "error", "-f", "lavfi", "-i", "color=c=green:s="+profile.size+":r=10",
				"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "0.5",
				"-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-c:a", profile.audio, path)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("generate %s: %v %s", profile.name, err, output)
			}
			metadata, err := New(ffmpeg, ffprobe).Probe(ctx, path)
			if err != nil || len(metadata.Streams) != 2 {
				t.Fatalf("probe %s: %v %+v", profile.name, err, metadata)
			}
			video, audio := metadata.Streams[0], metadata.Streams[1]
			if video.Codec != "h264" || video.Width != profile.width || video.Height != profile.height || audio.Codec != profile.audio {
				t.Fatalf("unexpected golden media profile: video=%+v audio=%+v", video, audio)
			}
		})
	}
}
