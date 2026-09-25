package playback

import (
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

// isHLS checks if the format or MIME indicates an HTTP Live Streaming manifest.
func isHLS(format, mime string) bool {
	for _, name := range strings.Split(format, ",") {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "hls" || name == "applehttp" {
			return true
		}
	}
	return strings.Contains(strings.ToLower(mime), "mpegurl")
}

// isFMP4 checks if the container uses fragmented MP4 segments rather than MPEG-TS.
// An HLS probe of H.264/AAC MPEG-TS does not certify fragmented MP4 playback.
func isFMP4(metadata domain.Metadata) bool {
	format := strings.ToLower(metadata.Format.Name)
	for _, part := range strings.Split(format, ",") {
		part = strings.TrimSpace(part)
		if part == "mp4" || part == "fmp4" || part == "m4s" || part == "mov" {
			return true
		}
	}
	for _, s := range metadata.Streams {
		tag := strings.ToLower(s.CodecTag)
		if tag == "avc1" || tag == "mp4a" || tag == "hvc1" || tag == "mp4v" {
			return true
		}
	}
	return false
}

// nativeHLSCandidate validates that:
// 1. The stream is an HLS manifest.
// 2. The device has fresh, functional probe evidence for HLS (hls-h264-aac PASS).
// 3. The video codec/profile/resolution has matching functional playback evidence.
// 4. The audio codec is AAC with no active probe failure.
// 5. The container is MPEG-TS, not fragmented MP4 or alternate container.
// 6. Evidence is not stale (suite 2, valid cache key, tested within 7 days, non-stalled).
func nativeHLSCandidate(metadata domain.Metadata, mime string, caps domain.Capabilities) bool {
	if !isHLS(metadata.Format.Name, mime) {
		return false
	}
	if isFMP4(metadata) {
		return false
	}
	if caps.SuiteVersion != 2 || caps.CacheKey == "" {
		return false
	}
	now := time.Now().Unix()
	hlsPass := false
	for _, p := range caps.Probes {
		if p.ID == "hls-h264-aac" {
			if p.Status == "PASS" && !p.Stalled && (p.PositionMS >= 500 || p.Completed) &&
				p.TestedAt > now-7*24*60*60 && p.TestedAt <= now+300 {
				hlsPass = true
			}
			break
		}
	}
	if !hlsPass {
		return false
	}
	status := func(id string) string {
		for _, p := range caps.Probes {
			if p.ID == id {
				return p.Status
			}
		}
		return "UNKNOWN"
	}
	hasVideo, hasAudio := false, false
	for _, stream := range metadata.Streams {
		if stream.Type == "video" {
			hasVideo = true
			if stream.Codec != "h264" || !hlsVideoCandidate(stream, status) {
				return false
			}
		}
		if stream.Type == "audio" {
			hasAudio = true
			if stream.Codec != "aac" || status("aac") == "FAIL" {
				return false
			}
		}
	}
	return hasVideo || hasAudio
}

// The HLS fixture certifies Baseline 360p only. Higher resolutions and other
// profiles need their own advancing decoder probe; a missing probe is not PASS.
func hlsVideoCandidate(stream domain.Stream, status func(string) string) bool {
	if stream.Width <= 0 || stream.Height <= 0 || stream.Width > 1920 || stream.Height > 1080 {
		return false
	}
	profile := strings.ToLower(stream.Profile)
	switch {
	case strings.Contains(profile, "high"):
		if stream.Width > 1280 || stream.Height > 720 {
			return status("h264-1080-high") == "PASS"
		}
		return status("h264-720-high") == "PASS"
	case strings.Contains(profile, "main"):
		if stream.Width > 1280 || stream.Height > 720 {
			return false // No Main 1080p fixture exists yet.
		}
		return status("h264-720-main") == "PASS"
	case strings.Contains(profile, "baseline"):
		if stream.Width <= 640 && stream.Height <= 360 {
			return true // The HLS fixture itself proves this path.
		}
		return stream.Width <= 854 && stream.Height <= 480 && status("h264-baseline-480") == "PASS"
	default:
		return false
	}
}
