package server

import (
	"context"
	"errors"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/playback"
	"zombiebox.local/gateway/internal/providers"
)

// Observed capabilities select media policy. Remote process inputs are scoped
// loopback relays; provider URLs and headers never enter subprocess arguments.
type playbackDecision struct {
	mode       string
	metadata   *domain.Metadata
	audioID    *int
	subtitleID *int
}

func (s *Server) playbackMode(ctx context.Context, source providers.Source, device domain.Device, requested string) (playbackDecision, error) {
	device.Capabilities = devices.CurrentCapabilities(device)
	youtube := source.Item.Provider == "youtube" && media.YouTubeRangeOrigin(source.URL)
	if (source.AudioURL != "" || youtube) && (requested == "DIRECT_PLAY" || requested == "EXTERNAL_PLAYER") {
		return playbackDecision{}, errors.New("adaptive stream requires mux")
	}
	if requested == "DIRECT_PLAY" || requested == "EXTERNAL_PLAYER" {
		return playbackDecision{mode: requested}, nil
	}
	// Keep unmanifested live relay in Auto, except opaque live MP3. MP3 uses
	// capability evidence to choose direct, remux or the AudioTrack fallback.
	if source.Live && media.ManifestKind(source) == "" && (requested == "" || requested == "AUTO") &&
		(!media.IsLiveMP3Source(source) || playback.LiveMP3DirectProven(device.Capabilities)) {
		return playbackDecision{mode: "DIRECT_PLAY"}, nil
	}
	if source.Path == "" {
		if s.deps.RemoteMedia == nil || !media.RemoteCandidate(source) {
			if source.AudioURL != "" || youtube {
				return playbackDecision{}, errors.New("remote media unavailable")
			}
			if media.IsLiveMP3Source(source) {
				return playbackDecision{}, errors.New("remote media unavailable")
			}
			return playbackDecision{mode: "DIRECT_PLAY"}, nil
		}
		metadata, err := s.deps.RemoteMedia.ProbeRemote(ctx, source)
		if err != nil {
			if requested == "" || requested == "AUTO" {
				if source.AudioURL == "" && !youtube && media.ManifestKind(source) == "" {
					if !media.IsLiveMP3Source(source) || playback.LiveMP3DirectProven(device.Capabilities) {
						return playbackDecision{mode: "DIRECT_PLAY"}, nil
					}
				}
			}
			return playbackDecision{}, err
		}
		if source.AudioURL != "" {
			mode := playback.LocalMode(metadata, source.MIME, device.Capabilities, requested)
			if mode == "DIRECT_PLAY" {
				for _, probe := range device.Capabilities.Probes {
					if probe.ID == "http-fmp4" && probe.Status == "FAIL" {
						return playbackDecision{}, errors.New("adaptive media unsupported")
					}
				}
				mode = "REMUX"
			}
			if mode == "EXTERNAL_PLAYER" {
				return playbackDecision{}, errors.New("adaptive media unsupported")
			}
			return playbackDecision{mode: mode, metadata: &metadata}, nil
		}
		return trackDecision(metadata, requested, source, device), nil
	}
	if s.deps.Media == nil {
		return playbackDecision{mode: "DIRECT_PLAY"}, nil
	}
	metadata, err := s.deps.Media.Probe(ctx, source.Path)
	if err != nil {
		return playbackDecision{}, err
	}
	return trackDecision(metadata, requested, source, device), nil
}

func trackDecision(metadata domain.Metadata, requested string, source domain.Source, device domain.Device) playbackDecision {
	mode := playback.LocalModeSource(metadata, source, device.Capabilities, requested)
	decision := playbackDecision{mode: mode, metadata: &metadata}
	// Split YouTube inputs and live streams have no stable single-input track IDs.
	if source.Live || source.AudioURL != "" {
		return decision
	}
	decision.audioID, decision.subtitleID = playback.PreferredTracks(playback.WithSubtitles(metadata, source), device.Preferences)
	decision.mode = playback.PreferredAudioMode(metadata, source.MIME, device.Capabilities, requested, decision.audioID)
	if decision.mode == "EXTERNAL_PLAYER" {
		decision.audioID, decision.subtitleID = nil, nil
	}
	return decision
}
