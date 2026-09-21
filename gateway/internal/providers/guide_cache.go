package providers

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type guideEntry struct {
	programmes     map[string][]domain.Programme
	fetched, retry time.Time
	loading        bool
}

type guideCache struct {
	mu      sync.Mutex
	entries map[[32]byte]guideEntry
}

// Guide refresh is independent of the thirty-second catalog cache. Failed fetches
// retain bounded stale metadata, never make live channel playback unavailable.
func (a *Adapters) guide(ctx context.Context, address string) (map[string][]domain.Programme, string) {
	key := sha256.Sum256([]byte(address))
	now := time.Now()
	a.guides.mu.Lock()
	if a.guides.entries == nil {
		a.guides.entries = map[[32]byte]guideEntry{}
	}
	entry := a.guides.entries[key]
	if entry.loading || now.Before(entry.retry) {
		a.guides.mu.Unlock()
		return guideValue(entry, now)
	}
	if len(a.guides.entries) >= 4 {
		for k, v := range a.guides.entries {
			if k != key && !v.loading {
				delete(a.guides.entries, k)
				break
			}
		}
	}
	if len(a.guides.entries) >= 4 {
		if _, exists := a.guides.entries[key]; !exists {
			a.guides.mu.Unlock()
			return nil, "UNAVAILABLE"
		}
	}
	entry.loading = true
	a.guides.entries[key] = entry
	a.guides.mu.Unlock()
	body, err := a.request(ctx, address, nil)
	if err == nil {
		var parsed map[string][]domain.Programme
		parsed, err = ParseXMLTV(body, now)
		if err == nil {
			entry.programmes = parsed
			entry.fetched = now
		}
	}
	entry.loading = false
	entry.retry = now.Add(30 * time.Second)
	if err == nil {
		entry.retry = now.Add(5 * time.Minute)
	}
	a.guides.mu.Lock()
	a.guides.entries[key] = entry
	a.guides.mu.Unlock()
	return guideValue(entry, time.Now())
}

func guideValue(entry guideEntry, now time.Time) (map[string][]domain.Programme, string) {
	age := now.Sub(entry.fetched)
	if entry.programmes == nil || age > 30*time.Minute {
		return nil, "UNAVAILABLE"
	}
	if age >= 5*time.Minute {
		return entry.programmes, "STALE"
	}
	return entry.programmes, "FRESH"
}
