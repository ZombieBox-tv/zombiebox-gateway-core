package server

import (
	"context"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/providers"
)

// Local planning is conservative: reports describe observed playback, never SDK guesses.
// UNKNOWN remains a candidate. Remote provider URLs are never passed to subprocesses.
func (s *Server) playbackMode(ctx context.Context, source providers.Source, device domain.Device, requested string) (string, error) {
	if requested == "DIRECT_PLAY" || requested == "EXTERNAL_PLAYER" {
		return requested, nil
	}
	if source.Path == "" || s.opt.MediaTools == nil {
		return "DIRECT_PLAY", nil
	}
	metadata, err := s.opt.MediaTools.Probe(ctx, source.Path)
	if err != nil {
		return "", err
	}
	return localMode(metadata, source.MIME, device.Capabilities, requested), nil
}
func localMode(metadata media.Metadata, mime string, capabilities domain.Capabilities, requested string) string {
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
		if stream.Type == "video" && (stream.Codec != "h264" || stream.Width > 1280 || stream.Height > 720 || status("h264-720-main") == "FAIL") {
			native = false
		}
		if stream.Type == "audio" && (stream.Codec != "aac" && stream.Codec != "mp3" || stream.Codec == "aac" && status("aac") == "FAIL") {
			native = false
		}
	}
	if native && mime != "video/x-matroska" && mime != "video/webm" && status("http-progressive") != "FAIL" {
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
