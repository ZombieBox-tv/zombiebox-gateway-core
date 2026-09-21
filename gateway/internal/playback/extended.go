package playback

import (
	"strconv"
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

// A short SDR/30fps progressive sample does not certify HDR, high-rate streams,
// arbitrary containers, or every stream supported by an advertised MIME type.
func extendedVideoCandidate(stream domain.Stream, metadata domain.Metadata, caps domain.Capabilities) bool {
	if stream.ColorTransfer != "bt709" || stream.ColorPrimaries != "bt709" {
		return false
	}
	if caps.SuiteVersion != 2 || caps.CacheKey == "" || stream.Width <= 0 || stream.Height <= 0 || stream.Width > 3840 || stream.Height > 2160 || stream.PixelFormat != "yuv420p" {
		return false
	}
	for _, rate := range []string{stream.FrameRate, stream.AverageFrameRate} {
		parts := strings.Split(rate, "/")
		if len(parts) != 2 {
			return false
		}
		n, e1 := strconv.ParseFloat(parts[0], 64)
		d, e2 := strconv.ParseFloat(parts[1], 64)
		if e1 != nil || e2 != nil || !(n > 0 && d > 0 && n/d <= 30) {
			return false
		}
	}
	bitrate, err := strconv.ParseInt(metadata.Format.BitRate, 10, 64)
	limit := int64(12000000)
	probe := "h264-2160-high"
	if stream.Codec == "hevc" {
		if stream.Profile != "Main" || stream.Level <= 0 || stream.Level > 150 || stream.CodecTag != "hvc1" || !strings.Contains(metadata.Format.Name, "mp4") {
			return false
		}
		probe = "hevc-2160-main"
		if stream.Width <= 1920 && stream.Height <= 1080 {
			probe = "hevc-1080-main"
			limit = 4000000
			if stream.Level > 120 {
				return false
			}
		}
	} else if stream.Codec != "h264" || stream.Profile != "High" || stream.Level <= 0 || stream.Level > 51 {
		return false
	}
	if err != nil || bitrate <= 0 || bitrate > limit {
		return false
	}
	now := time.Now().Unix()
	for _, result := range caps.Probes {
		if result.ID == probe {
			return result.Status == "PASS" && !result.Stalled && result.PositionMS >= 500 && result.TestedAt > now-7*24*60*60 && result.TestedAt <= now+300
		}
	}
	return false
}
