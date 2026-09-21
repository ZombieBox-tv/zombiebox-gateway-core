package providers

import (
	"context"
	"errors"

	"zombiebox.local/gateway/internal/domain"
)

// Reception reads fresh activity independently of the slower Home catalog cache.
func (a *Adapters) Reception(ctx context.Context, provider string, config Config) (*Source, domain.NowPlaying, error) {
	if provider == "spotify" {
		state, err := a.SpotifyStatus(ctx, config)
		if err != nil || state.State == "STOPPED" {
			return nil, state, err
		}
		source := spotifySource(config, state)
		return &source, state, nil
	}
	if provider == "airplay" {
		sources, err := a.AirPlay(ctx, config)
		state := domain.NowPlaying{Provider: provider, State: "STOPPED"}
		if err != nil {
			return nil, state, err
		}
		for _, source := range sources {
			if source.Item.Playable {
				state.State = "PLAYING"
				state.Item = &source.Item
				return &source, state, nil
			}
		}
		return nil, state, nil
	}
	return nil, domain.NowPlaying{}, errors.New("receiver unavailable")
}
