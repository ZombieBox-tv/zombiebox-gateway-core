package server

import (
	"context"

	"zombiebox.local/gateway/internal/playback"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

// Local planning is conservative: reports describe observed playback, never SDK guesses.
// UNKNOWN remains a candidate. Remote provider URLs are never passed to subprocesses.
func (s *Server) playbackMode(ctx context.Context, source providers.Source, device domain.Device, requested string) (string, error) {
	if requested == "DIRECT_PLAY" || requested == "EXTERNAL_PLAYER" {
		return requested, nil
	}
	if source.Path == "" || s.deps.Media == nil {
		return "DIRECT_PLAY", nil
	}
	metadata, err := s.deps.Media.Probe(ctx, source.Path)
	if err != nil {
		return "", err
	}
	return playback.LocalMode(metadata, source.MIME, device.Capabilities, requested), nil
}
