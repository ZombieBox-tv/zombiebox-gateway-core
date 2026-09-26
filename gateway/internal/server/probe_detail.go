package server

import (
	"regexp"
	"strings"
)

// Probe details originate on a device. Keep only known, bounded diagnostic
// fields before persisting or exporting them; never echo arbitrary client text.
var (
	probeMediaError = regexp.MustCompile(`^what=-?[0-9]{1,10},extra=-?[0-9]{1,10}@(prepare|playback)( http=([1-5][0-9]{2}|failed|timeout|io_error|error)(,[a-z0-9.-]{1,32}/[a-z0-9.+-]{1,32})?)?$`)
	probeTimeout    = regexp.MustCompile(`^timeout@(prepare|playback)( http=([1-5][0-9]{2}|failed|timeout|io_error|error)(,[a-z0-9.-]{1,32}/[a-z0-9.+-]{1,32})?)?$`)
	probeSeekPos    = regexp.MustCompile(`^seek_pos:[0-9]{1,5}$`)
	probeMIMEs      = map[string]bool{
		"application/octet-stream":      true,
		"application/vnd.apple.mpegurl": true,
		"application/x-mpegurl":         true,
		"audio/aac":                     true,
		"audio/mp4":                     true,
		"audio/mpeg":                    true,
		"text/html":                     true,
		"text/plain":                    true,
		"video/mp2t":                    true,
		"video/mp4":                     true,
	}
)

func safeProbeDetail(detail string) string {
	if len(detail) > 120 {
		return ""
	}
	if probeMediaError.MatchString(detail) || probeTimeout.MatchString(detail) || probeSeekPos.MatchString(detail) {
		if mimeStart := strings.LastIndex(detail, " http="); mimeStart >= 0 {
			if comma := strings.IndexByte(detail[mimeStart:], ','); comma >= 0 {
				cut := mimeStart + comma
				if !probeMIMEs[detail[cut+1:]] {
					return detail[:cut]
				}
			}
		}
		return detail
	}
	switch detail {
	case "pause_failed", "surface_invalid", "missing_texture", "missing_surface",
		"operation_incomplete", "no_advance", "insufficient_frames",
		"payload_mismatch", "multicast_timeout":
		return detail
	}
	for _, prefix := range []string{"resume_error:", "surface_error:", "tick_error:", "start_failed:", "seek_error:", "unsupported_kind:", "exception:", "multicast_error:", "prerequisite_unmet:"} {
		if strings.HasPrefix(detail, prefix) {
			return strings.TrimSuffix(prefix, ":")
		}
	}
	return ""
}
