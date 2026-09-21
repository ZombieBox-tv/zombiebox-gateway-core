package playback

import (
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

func LocalMode(metadata domain.Metadata, mime string, capabilities domain.Capabilities, requested string) string {
	status := func(id string) string {
		for _, p := range capabilities.Probes {
			if p.ID == id {
				return p.Status
			}
		}
		return "UNKNOWN"
	}
	if requested == "REMUX" || requested == "TRANSCODE" {
		return requested
	}
	native := true
	for _, stream := range metadata.Streams {
		if stream.Type == "video" && (stream.Codec != "h264" || !videoCandidate(stream, status)) {
			native = false
		}
		if stream.Type == "audio" && (stream.Codec != "aac" && stream.Codec != "mp3" || stream.Codec == "aac" && status("aac") == "FAIL") {
			native = false
		}
	}
	// Probe evidence takes precedence over a provider's generic video/mp4 label.
	containerNeedsRemux := strings.Contains(metadata.Format.Name, "matroska") || strings.Contains(metadata.Format.Name, "webm") || metadata.Format.Name == "mpegts" || metadata.Format.Name == "hls" || metadata.Format.Name == "dash" || strings.Contains(mime, "mpegurl") || strings.Contains(mime, "dash")
	if native && !containerNeedsRemux && mime != "video/x-matroska" && mime != "video/webm" && status("http-progressive") != "FAIL" {
		return "DIRECT_PLAY"
	}
	// Both conversion modes currently stream fragmented MP4. Known failures require external fallback.
	if status("http-fmp4") == "FAIL" {
		return "EXTERNAL_PLAYER"
	}
	if native {
		return "REMUX"
	}
	if status("h264-baseline-360") == "FAIL" || status("aac") == "FAIL" {
		return "EXTERNAL_PLAYER"
	}
	return "TRANSCODE"
}

// Profiles remain hints until the matching synthetic path has actual evidence.
// A failed Main profile must not reject already-compatible Baseline media.
func videoCandidate(stream domain.Stream, status func(string) string) bool {
	profile := strings.ToLower(stream.Profile)
	probe := ""
	switch {
	case stream.Width > 1920 || stream.Height > 1080:
		return false
	case stream.Width > 1280 || stream.Height > 720:
		return strings.Contains(profile, "high") && status("h264-1080-high") == "PASS"
	case strings.Contains(profile, "high"):
		probe = "h264-720-high"
	case strings.Contains(profile, "main"):
		probe = "h264-720-main"
	case stream.Width <= 640 && stream.Height <= 360:
		probe = "h264-baseline-360"
	case stream.Width <= 854 && stream.Height <= 480:
		probe = "h264-baseline-480"
	}
	return probe == "" || status(probe) != "FAIL"
}
