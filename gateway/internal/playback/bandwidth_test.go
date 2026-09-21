package playback

import (
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestMeasuredBudgetKeepsHeadroomAndCompatibleLowRateStreams(t *testing.T) {
	video := &domain.Metadata{Streams: []domain.Stream{{Type: "video"}}}
	video.Format.BitRate = "8000000"
	for _, test := range []struct {
		kbps    int64
		quality string
	}{{0, ""}, {700, "LOW"}, {3000, "STANDARD"}, {20000, ""}} {
		if actual := NetworkQuality(video, test.kbps); actual != test.quality {
			t.Fatalf("%d: %s", test.kbps, actual)
		}
	}
	video.Format.BitRate = "150000"
	if NetworkQuality(video, 700) != "" {
		t.Fatal("unnecessary conversion of low-rate video")
	}
	video.Streams[0].Type = "audio"
	video.Format.BitRate = "320000"
	if NetworkQuality(video, 200) != "" {
		t.Fatal("video conversion of audio")
	}
	if NetworkQuality(nil, 700) != "" {
		t.Fatal("unknown metadata treated as video")
	}
}
