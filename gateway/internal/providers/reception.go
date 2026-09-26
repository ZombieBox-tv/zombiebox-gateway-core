package providers

import (
	"context"
	"errors"

	"zombiebox.local/gateway/internal/domain"
)

// Reception reads fresh activity independently of the slower Home catalog cache.
func (a *Adapters) Reception(ctx context.Context, provider string, config Config) (*Source, domain.NowPlaying, error) {
	if provider == "spotify" {
		state, hasTrack, err := a.spotifyStatus(ctx, config)
		if err != nil || state.State == "STOPPED" || !hasTrack {
			return nil, state, err
		}
		source := spotifySource(config, state)
		return &source, state, nil
	}
	if provider == "airplay" {
		sources, connected, audioActive, hasTrackMetadata, progress, err := a.airplaySources(ctx, config)
		state := domain.NowPlaying{Provider: provider, State: "STOPPED"}
		if err != nil {
			return nil, state, err
		}
		// A mirrored screen carries audio too. A ready video source is stronger
		// evidence than the simultaneously active audio-only receiver stream.
		for _, source := range sources {
			if source.Item.Kind == "video" && source.Item.Playable {
				state.State = "PLAYING"
				state.Item = &source.Item
				return &source, state, nil
			}
		}
		if audioActive && (hasTrackMetadata || connected) {
			for _, source := range sources {
				if source.Item.Kind == "audio" && source.Item.Playable {
					state.State = "PLAYING"
					state.Item = &source.Item
					applyAirplayProgress(&state, progress)
					return &source, state, nil
				}
			}
		}
		for _, source := range sources {
			if source.Item.Playable {
				state.State = "PLAYING"
				state.Item = &source.Item
				applyAirplayProgress(&state, progress)
				return &source, state, nil
			}
		}
		if connected {
			// Connection presence does not prove playback or imply sender pause.
			// BUFFERING preserves an existing listener plan while RTP is absent.
			state.State = "BUFFERING"
			for _, source := range sources {
				if source.Item.Kind == "audio" {
					state.Item = &source.Item
					break
				}
			}
		}
		applyAirplayProgress(&state, progress)
		return nil, state, nil
	}
	return nil, domain.NowPlaying{}, errors.New("receiver unavailable")
}

func applyAirplayProgress(state *domain.NowPlaying, progress airplaySenderProgress) {
	if !progress.Known || state.Item == nil || state.Item.Kind != "audio" {
		return
	}
	state.PositionKnown = true
	state.PositionMS = progress.PositionMS
	state.DurationMS = progress.DurationMS
	state.PositionAgeMS = progress.AgeMS
	state.Item.DurationMS = progress.DurationMS
}
