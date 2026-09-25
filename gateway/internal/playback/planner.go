package playback

import (
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func LocalMode(metadata domain.Metadata, mime string, capabilities domain.Capabilities, requested string) string {
	if requested == "REMUX" || requested == "TRANSCODE" {
		return requested
	}
	now := time.Now().Unix()
	status := func(id string) string {
		return probeStatus(capabilities, id, now)
	}

	// 1. Direct play check
	if isHLS(metadata.Format.Name, mime) {
		if nativeHLSCandidate(metadata, mime, capabilities) {
			return "DIRECT_PLAY"
		}
	} else {
		if progressiveDirectCandidate(metadata, mime, capabilities, status) {
			return "DIRECT_PLAY"
		}
	}

	// 2. Direct play is not available; evaluate conversion fallback:
	// Both conversion modes (REMUX and TRANSCODE) stream fragmented MP4.
	// Known fMP4 failure requires external player fallback.
	if status("http-fmp4") == "FAIL" {
		return "EXTERNAL_PLAYER"
	}

	// REMUX fallback when source codecs can be copied without transcoding:
	if canRemux(metadata, mime, capabilities, status) {
		return "REMUX"
	}

	// TRANSCODE fallback outputs H.264 Baseline 360p and AAC.
	// Known failures for baseline H.264 or AAC require external player.
	if status("h264-baseline-360") == "FAIL" || status("aac") == "FAIL" {
		return "EXTERNAL_PLAYER"
	}

	return "TRANSCODE"
}

// LocalModeSource accounts for the progressive ADTS path used by live, audio-only
// AAC HLS. That path requires fresh advancing ADTS evidence and does not require
// the fragmented-MP4 probe used by the general REMUX pipeline.
func LocalModeSource(metadata domain.Metadata, source domain.Source, capabilities domain.Capabilities, requested string) string {
	mode := LocalMode(metadata, source.MIME, capabilities, requested)
	if source.Live && isHLS(metadata.Format.Name, source.MIME) && (requested == "" || requested == "AUTO" || requested == "REMUX") {
		hasAudio := false
		compatible := true
		for _, stream := range metadata.Streams {
			if stream.Type == "video" || (stream.Type == "audio" && stream.Codec != "aac") {
				compatible = false
			}
			hasAudio = hasAudio || stream.Type == "audio"
		}
		if compatible && hasAudio {
			if mode == "REMUX" || mode == "EXTERNAL_PLAYER" {
				if probeStatus(capabilities, "aac-adts", time.Now().Unix()) == "PASS" {
					return "REMUX"
				}
				return "EXTERNAL_PLAYER"
			}
		}
	}
	return mode
}

func progressiveDirectCandidate(metadata domain.Metadata, mime string, capabilities domain.Capabilities, status func(string) string) bool {
	if needsCompatibleContainer(metadata.Format.Name, mime) {
		return false
	}
	switch strings.ToLower(mime) {
	case "video/x-matroska", "video/webm", "video/x-msvideo", "video/x-flv", "video/x-ms-asf":
		return false
	}
	hasVideo, hasAudio := false, false
	hasExtended := false
	for _, stream := range metadata.Streams {
		if stream.Type == "video" {
			hasVideo = true
			compatible := false
			if stream.Codec == "hevc" || stream.Width > 1920 || stream.Height > 1080 {
				compatible = extendedVideoCandidate(stream, metadata, capabilities)
				if compatible {
					hasExtended = true
				}
			} else if stream.Codec == "h264" {
				compatible = videoCandidate(stream, status)
			}
			if !compatible {
				return false
			}
		}
		if stream.Type == "audio" {
			hasAudio = true
			if stream.Codec == "aac" {
				if status("aac") != "PASS" {
					return false
				}
			} else if stream.Codec != "mp3" {
				return false
			}
		}
	}
	if !hasVideo && !hasAudio {
		return false
	}
	// For standard progressive streams, require fresh PASS on http-progressive.
	// For extended UHD/HEVC streams certified by their own progressive sample probe,
	// ensure http-progressive has not failed.
	if hasExtended {
		if status("http-progressive") == "FAIL" {
			return false
		}
	} else {
		if status("http-progressive") != "PASS" {
			return false
		}
	}
	return true
}

func canRemux(metadata domain.Metadata, mime string, capabilities domain.Capabilities, status func(string) string) bool {
	hasVideo, hasAudio := false, false
	for _, stream := range metadata.Streams {
		if stream.Type == "video" {
			hasVideo = true
			compatible := false
			if stream.Codec == "hevc" || stream.Width > 1920 || stream.Height > 1080 {
				compatible = extendedVideoCandidate(stream, metadata, capabilities)
			} else if stream.Codec == "h264" {
				compatible = videoRemuxCandidate(stream, status)
			}
			if !compatible {
				return false
			}
		}
		if stream.Type == "audio" {
			hasAudio = true
			if stream.Codec != "aac" && stream.Codec != "mp3" {
				return false
			}
			if stream.Codec == "aac" && status("aac") == "FAIL" {
				return false
			}
		}
	}
	return hasVideo || hasAudio
}

// videoCandidate validates that a video stream has fresh, positive PASS evidence
// for direct playback. Unprobed or stale streams do NOT pass direct codec decisions.
func videoCandidate(stream domain.Stream, status func(string) string) bool {
	if stream.Width <= 0 || stream.Height <= 0 || stream.Width > 1920 || stream.Height > 1080 {
		return false
	}
	profile := strings.ToLower(stream.Profile)
	switch {
	case stream.Width > 1280 || stream.Height > 720:
		return strings.Contains(profile, "high") && status("h264-1080-high") == "PASS"
	case strings.Contains(profile, "high"):
		return status("h264-720-high") == "PASS"
	case strings.Contains(profile, "main") || (profile == "" && (stream.Width > 854 || stream.Height > 480)):
		return status("h264-720-main") == "PASS"
	case strings.Contains(profile, "baseline") || profile == "":
		if stream.Width <= 640 && stream.Height <= 360 {
			return status("h264-baseline-360") == "PASS"
		}
		if stream.Width <= 854 && stream.Height <= 480 {
			return status("h264-baseline-480") == "PASS"
		}
		return false
	default:
		return false
	}
}

// videoRemuxCandidate determines if a video stream can be copied without transcoding.
// Higher resolutions (1080p) require positive decode proof to protect legacy receivers.
// Moderate resolutions (720p, 480p, 360p) allow remuxing unless known to have failed or stalled.
func videoRemuxCandidate(stream domain.Stream, status func(string) string) bool {
	if stream.Width <= 0 || stream.Height <= 0 || stream.Width > 1920 || stream.Height > 1080 {
		return false
	}
	profile := strings.ToLower(stream.Profile)
	switch {
	case stream.Width > 1280 || stream.Height > 720:
		return strings.Contains(profile, "high") && status("h264-1080-high") == "PASS"
	case strings.Contains(profile, "high"):
		return status("h264-720-high") != "FAIL"
	case strings.Contains(profile, "main") || (profile == "" && (stream.Width > 854 || stream.Height > 480)):
		return status("h264-720-main") != "FAIL"
	case strings.Contains(profile, "baseline") || profile == "":
		if stream.Width <= 640 && stream.Height <= 360 {
			return status("h264-baseline-360") != "FAIL"
		}
		if stream.Width <= 854 && stream.Height <= 480 {
			return status("h264-baseline-480") != "FAIL"
		}
		return status("h264-720-main") != "FAIL"
	default:
		return false
	}
}

// Codec support does not imply support for the source container on the receiver.
func needsCompatibleContainer(format, mime string) bool {
	for _, name := range strings.Split(format, ",") {
		switch strings.TrimSpace(strings.ToLower(name)) {
		case "matroska", "webm", "mpegts", "mpeg", "hls", "dash", "avi", "flv", "asf":
			return true
		}
	}
	switch strings.ToLower(mime) {
	case "video/x-matroska", "video/webm", "video/x-msvideo", "video/x-flv", "video/x-ms-asf":
		return true
	}
	return strings.Contains(strings.ToLower(mime), "mpegurl") || strings.Contains(strings.ToLower(mime), "dash")
}
