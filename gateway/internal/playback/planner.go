package playback

import (
	"net/url"
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
	if !isHLS(metadata.Format.Name, mime) && canHybrid(metadata, mime, capabilities, status) {
		return "HYBRID"
	}

	// TRANSCODE fallback outputs H.264 Baseline 360p and AAC.
	// Known failures for baseline H.264 or AAC require external player.
	if status("h264-baseline-360") == "FAIL" || status("aac") == "FAIL" {
		return "EXTERNAL_PLAYER"
	}

	return "TRANSCODE"
}

// LocalModeSource chooses evidence-backed live audio transports separately
// from the general fragmented-MP4 conversion path. Native HLS and probed
// chunked MP3 stay first; live audio falls back to PCM only on a local PASS.
func LocalModeSource(metadata domain.Metadata, source domain.Source, capabilities domain.Capabilities, requested string) string {
	mode := LocalMode(metadata, source.MIME, capabilities, requested)
	if isLiveMP3Audio(metadata, source) && (requested == "" || requested == "AUTO" || requested == "REMUX" || requested == "TRANSCODE") {
		if requested == "REMUX" {
			return mode
		}
		status := func(id string) string {
			return probeStatus(capabilities, id, time.Now().Unix())
		}
		if requested != "TRANSCODE" && status("mp3-chunked") == "PASS" {
			return "DIRECT_PLAY"
		}
		// The current fMP4 probe carries H.264/AAC and cannot certify MP3 in MP4.
		if status("audio-track-pcm-stream") == "PASS" {
			return "PCM_STREAM"
		}
		return "EXTERNAL_PLAYER"
	}
	if source.Live && isHLS(metadata.Format.Name, source.MIME) && (requested == "" || requested == "AUTO" || requested == "REMUX" || requested == "TRANSCODE") {
		hasAudio := false
		compatible := true
		for _, stream := range metadata.Streams {
			if stream.Type == "video" || (stream.Type == "audio" && stream.Codec != "aac") {
				compatible = false
			}
			hasAudio = hasAudio || stream.Type == "audio"
		}
		if compatible && hasAudio {
			if mode == "REMUX" || mode == "EXTERNAL_PLAYER" || mode == "TRANSCODE" {
				if requested != "TRANSCODE" && probeStatus(capabilities, "aac-adts", time.Now().Unix()) == "PASS" {
					return "REMUX"
				}
				if requested != "REMUX" && probeStatus(capabilities, "audio-track-pcm-stream", time.Now().Unix()) == "PASS" {
					return "PCM_STREAM"
				}
				return "EXTERNAL_PLAYER"
			}
		}
	}
	return mode
}

// LiveMP3DirectProven reports whether the current device has recent advancing
// evidence for playing a chunked MP3 stream without conversion.
func LiveMP3DirectProven(capabilities domain.Capabilities) bool {
	return probeStatus(capabilities, "mp3-chunked", time.Now().Unix()) == "PASS"
}

func isLiveMP3Audio(metadata domain.Metadata, source domain.Source) bool {
	if !source.Live || source.AudioURL != "" || !strings.EqualFold(strings.TrimSpace(strings.SplitN(source.MIME, ";", 2)[0]), "audio/mpeg") {
		return false
	}
	parsed, err := url.Parse(source.URL)
	if err == nil {
		path := strings.ToLower(parsed.Path)
		if strings.HasSuffix(path, ".m3u8") || strings.HasSuffix(path, ".mpd") {
			return false
		}
	}
	hasAudio := false
	for _, stream := range metadata.Streams {
		if stream.Type == "video" || (stream.Type == "audio" && stream.Codec != "mp3") {
			return false
		}
		hasAudio = hasAudio || stream.Type == "audio"
	}
	return hasAudio
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

func canHybrid(metadata domain.Metadata, mime string, capabilities domain.Capabilities, status func(string) string) bool {
	hasVideo, hasAudio, needsAudioEncode := false, false, false
	for _, stream := range metadata.Streams {
		if stream.Type == "video" {
			// HYBRID is the video-copy path, so it requires fresh positive
			// decoder evidence rather than the legacy REMUX fallback policy.
			if stream.Codec != "h264" || !videoCandidate(stream, status) {
				return false
			}
			hasVideo = true
		}
		if stream.Type == "audio" {
			hasAudio = true
			if stream.Codec == "aac" || stream.Codec == "mp3" {
				continue
			}
			needsAudioEncode = true
		}
	}
	// AAC output must have fresh, advancing decoder evidence. Missing, stale,
	// or failed evidence remains on the full transcode fallback.
	return hasVideo && hasAudio && needsAudioEncode && status("aac") == "PASS"
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
