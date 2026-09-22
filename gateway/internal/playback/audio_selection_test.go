package playback

import (
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestSelectedAudioModeIgnoresUnmappedTracksAndHonorsConversionEvidence(t *testing.T) {
	metadata := domain.Metadata{Streams: []domain.Stream{
		{Index: 0, Type: "video", Codec: "h264", Width: 640, Height: 360},
		{Index: 1, Type: "audio", Codec: "aac"},
		{Index: 2, Type: "audio", Codec: "dts"},
	}}
	for _, tc := range []struct {
		name                           string
		id                             int
		position                       int64
		quality, failed, want, current string
	}{
		{name: "compatible selection", id: 1, want: "REMUX"},
		{name: "preserve conversion intent", id: 1, current: "TRANSCODE", want: "TRANSCODE"},
		{name: "DTS selection", id: 2, want: "TRANSCODE"},
		{name: "accurate resume", id: 1, position: 1200, want: "TRANSCODE"},
		{name: "bandwidth cap", id: 1, quality: "LOW", want: "TRANSCODE"},
		{name: "fragment failure", id: 1, failed: "http-fmp4", want: "EXTERNAL_PLAYER"},
		{name: "resume decoder failure", id: 1, position: 1200, failed: "h264-baseline-360", want: "EXTERNAL_PLAYER"},
		{name: "transcode audio failure", id: 2, failed: "aac", want: "EXTERNAL_PLAYER"},
		{name: "non-audio index", id: 0, want: "EXTERNAL_PLAYER"},
		{name: "missing index", id: 99, want: "EXTERNAL_PLAYER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := domain.Capabilities{}
			if tc.failed != "" {
				caps.Probes = []domain.Probe{{ID: tc.failed, Status: "FAIL"}}
			}
			got := SelectedAudioMode(metadata, "video/mp4", caps, domain.MediaSelection{AudioID: &tc.id, PositionMS: tc.position, Quality: tc.quality}, tc.current)
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
			if len(metadata.Streams) != 3 {
				t.Fatal("input metadata mutated")
			}
		})
	}
}
