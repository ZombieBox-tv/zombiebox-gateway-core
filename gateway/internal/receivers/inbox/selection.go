package inbox

import (
	"context"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type observation struct {
	source *domain.Source
	status domain.NowPlaying
	err    error
}

// Two confirmed polls of newly active playback precede a handoff. Metadata
// updates do not count as a new sender, and uncertain reads never mean idle.
type selection struct {
	current string
	pending string
	playing map[string]bool
	blocked map[string]string
}

func (s *Service) read(ctx context.Context, provider string) map[string]observation {
	providers := []string{provider}
	if provider == "auto" {
		providers = []string{"spotify", "airplay"}
	}
	result := make(map[string]observation, len(providers))
	for _, id := range providers {
		request, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
		source, status, err := s.backend.Read(request, id)
		if err == nil {
			err = request.Err()
		}
		cancel()
		result[id] = observation{source, status, err}
	}
	return result
}

func (p *selection) choose(values map[string]observation) string {
	if p.playing == nil {
		p.playing = map[string]bool{}
		p.blocked = map[string]string{}
	}
	candidate := ""
	for _, id := range []string{"spotify", "airplay"} {
		value := values[id]
		if value.err != nil {
			continue
		}
		if value.source == nil {
			delete(p.blocked, id)
		}
		active := value.source != nil && value.status.State == "PLAYING"
		if active && p.blocked[id] != value.source.Item.ID && id != p.current && (!p.playing[id] || p.pending == id || p.current == "") {
			candidate = id
		}
		p.playing[id] = active
	}
	// Prefer the existing session for an unknown or paused current sender.
	current := values[p.current]
	if candidate == "" && p.current != "" && current.err == nil && current.source == nil {
		for _, id := range []string{"spotify", "airplay"} {
			value := values[id]
			if value.err == nil && value.source != nil && value.status.State == "PLAYING" && p.blocked[id] != value.source.Item.ID {
				candidate = id
			}
		}
	}
	if candidate != "" && (p.current == "" || p.pending == candidate) {
		p.pending = candidate
		return candidate
	}
	if candidate == "" && p.pending != "" && values[p.pending].err != nil {
		return p.current
	}
	p.pending = candidate
	return p.current
}
