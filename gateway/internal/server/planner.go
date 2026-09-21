package server

import (
	"context"
	"errors"

	"zombiebox.local/gateway/internal/media"

	"zombiebox.local/gateway/internal/playback"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

// Observed capabilities select media policy. Remote process inputs are scoped
// loopback relays; provider URLs and headers never enter subprocess arguments.
func (s *Server) playbackMode(ctx context.Context, source providers.Source, device domain.Device, requested string) (string, error) {
	if source.AudioURL != "" && (requested == "DIRECT_PLAY" || requested == "EXTERNAL_PLAYER") {
		return "", errors.New("adaptive stream requires mux")
	}
	if requested == "DIRECT_PLAY" || requested == "EXTERNAL_PLAYER" {
		return requested, nil
	}
	// Keep live direct relay in Auto; an explicit compatible retry may convert TS.
	if source.Live && media.ManifestKind(source) == "" && (requested == "" || requested == "AUTO") {
		return "DIRECT_PLAY", nil
	}
	if source.Path == "" {
		if s.deps.RemoteMedia == nil || !media.RemoteCandidate(source) {
			if source.AudioURL != "" {
				return "", errors.New("remote media unavailable")
			}
			return "DIRECT_PLAY", nil
		}
		metadata, err := s.deps.RemoteMedia.ProbeRemote(ctx, source)
		if err != nil {
			if requested == "" || requested == "AUTO" {
				if source.AudioURL == "" {
					return "DIRECT_PLAY", nil
				}
			}
			return "", err
		}
		mode := playback.LocalMode(metadata, source.MIME, device.Capabilities, requested)
		if source.AudioURL != "" {
			if mode == "DIRECT_PLAY" {
				for _, probe := range device.Capabilities.Probes {
					if probe.ID == "http-fmp4" && probe.Status == "FAIL" {
						return "", errors.New("adaptive media unsupported")
					}
				}
				mode = "REMUX"
			}
			if mode == "EXTERNAL_PLAYER" {
				return "", errors.New("adaptive media unsupported")
			}
		}
		return mode, nil
	}
	if s.deps.Media == nil {
		return "DIRECT_PLAY", nil
	}
	metadata, err := s.deps.Media.Probe(ctx, source.Path)
	if err != nil {
		return "", err
	}
	return playback.LocalMode(metadata, source.MIME, device.Capabilities, requested), nil
}
