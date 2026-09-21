package playback

import (
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestExtendedVideoRequiresBoundedFreshEvidence(t *testing.T) {
	base := domain.Stream{Type: "video", Codec: "hevc", Profile: "Main", Level: 150, Width: 3840, Height: 2160, PixelFormat: "yuv420p", FrameRate: "30/1", AverageFrameRate: "30/1", CodecTag: "hvc1", ColorTransfer: "bt709", ColorPrimaries: "bt709"}
	for _, tc := range []struct {
		name string
		edit func(*domain.Stream, *domain.Capabilities, *domain.Metadata)
		want string
	}{
		{"measured UHD", func(*domain.Stream, *domain.Capabilities, *domain.Metadata) {}, "DIRECT_PLAY"},
		{"HDR Main10", func(s *domain.Stream, _ *domain.Capabilities, _ *domain.Metadata) {
			s.Profile = "Main 10"
			s.PixelFormat = "yuv420p10le"
		}, "TRANSCODE"},
		{"60 fps", func(s *domain.Stream, _ *domain.Capabilities, _ *domain.Metadata) { s.FrameRate = "60/1" }, "TRANSCODE"},
		{"unknown rate", func(s *domain.Stream, _ *domain.Capabilities, _ *domain.Metadata) { s.AverageFrameRate = "0/0" }, "TRANSCODE"},
		{"untested bitrate", func(_ *domain.Stream, _ *domain.Capabilities, m *domain.Metadata) { m.Format.BitRate = "30000000" }, "TRANSCODE"},
		{"HEV1 tag", func(s *domain.Stream, _ *domain.Capabilities, _ *domain.Metadata) { s.CodecTag = "hev1" }, "TRANSCODE"},
		{"MKV is not progressive evidence", func(_ *domain.Stream, _ *domain.Capabilities, m *domain.Metadata) { m.Format.Name = "matroska" }, "TRANSCODE"},
		{"expired", func(_ *domain.Stream, c *domain.Capabilities, _ *domain.Metadata) {
			c.Probes[0].TestedAt = time.Now().Add(-8 * 24 * time.Hour).Unix()
		}, "TRANSCODE"},
		{"no evidence", func(_ *domain.Stream, c *domain.Capabilities, _ *domain.Metadata) { c.Probes = nil }, "TRANSCODE"},
		{"prepared only", func(_ *domain.Stream, c *domain.Capabilities, _ *domain.Metadata) { c.Probes[0].PositionMS = 0 }, "TRANSCODE"},
		{"failed", func(_ *domain.Stream, c *domain.Capabilities, _ *domain.Metadata) { c.Probes[0].Status = "FAIL" }, "TRANSCODE"},
		{"stale fingerprint", func(_ *domain.Stream, c *domain.Capabilities, _ *domain.Metadata) { c.CacheKey = "" }, "TRANSCODE"},
		{"larger than UHD", func(s *domain.Stream, _ *domain.Capabilities, _ *domain.Metadata) { s.Width = 4096 }, "TRANSCODE"},
		{"H264 UHD", func(s *domain.Stream, c *domain.Capabilities, _ *domain.Metadata) {
			s.Codec = "h264"
			s.Profile = "High"
			s.Level = 51
			c.Probes[0].ID = "h264-2160-high"
		}, "DIRECT_PLAY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := base
			caps := domain.Capabilities{SuiteVersion: 2, CacheKey: "current", Probes: []domain.Probe{{ID: "hevc-2160-main", Status: "PASS", PositionMS: 1500, TestedAt: time.Now().Unix()}}}
			metadata := domain.Metadata{}
			metadata.Format.Name = "mov,mp4,m4a,3gp,3g2,mj2"
			metadata.Format.BitRate = "11000000"
			tc.edit(&stream, &caps, &metadata)
			metadata.Streams = []domain.Stream{stream}
			if got := LocalMode(metadata, "video/mp4", caps, ""); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
