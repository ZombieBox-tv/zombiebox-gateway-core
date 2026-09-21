package playback

import (
	"strconv"

	"zombiebox.local/gateway/internal/domain"
)

// NetworkQuality applies 30% headroom to measured gateway-to-client goodput.
// Unknown source bitrate is not proof that conversion is necessary. LOW links
// conservatively cap video; known audio-only streams retain their native path.
func NetworkQuality(metadata *domain.Metadata, kbps int64) string {
	if kbps <= 0 {
		return ""
	}
	video := false
	var bitrate int64
	if metadata != nil {
		bitrate, _ = strconv.ParseInt(metadata.Format.BitRate, 10, 64)
		for _, stream := range metadata.Streams {
			video = video || stream.Type == "video"
		}
	}
	if !video {
		return ""
	}
	budget := kbps * 700
	if bitrate > 0 && bitrate <= budget {
		return ""
	}
	if budget < 1500000 {
		return "LOW"
	}
	if bitrate > budget {
		return "STANDARD"
	}
	return ""
}
