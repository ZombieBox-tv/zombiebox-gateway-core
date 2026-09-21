// Package home owns stable, semantic Home composition policies.
package home

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type Persistence interface {
	Get(context.Context, string, string, any) error
	Put(context.Context, string, string, any) error
}

type Selection struct {
	ID     string
	Until  time.Time
	Recent []string
}

type Heroes struct {
	mu    sync.Mutex
	store Persistence
	clock func() time.Time
}

func New(store Persistence, clock func() time.Time) *Heroes {
	return &Heroes{store: store, clock: clock}
}

// Select retains identity for three hours, but never retains stale metadata or an
// item removed from the current authorized catalog. Only IDs are persisted.
func (h *Heroes) Select(ctx context.Context, device, scope string, sections []domain.Section) *domain.Hero {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := device + ":" + scope
	var previous Selection
	_ = h.store.Get(ctx, "home-hero", key, &previous)
	candidates := map[string]domain.Item{}
	continuing := map[string]bool{}
	for _, section := range sections {
		for _, item := range section.Items {
			if !item.Playable || item.Title == "" {
				continue
			}
			if section.Type == "continue_watching" {
				continuing[item.ID] = true
			}
			if old, exists := candidates[item.ID]; exists && old.PositionMS > item.PositionMS {
				item.PositionMS = old.PositionMS
			}
			candidates[item.ID] = item
		}
	}
	if item, ok := candidates[previous.ID]; ok && h.clock().Before(previous.Until) {
		return hero(item)
	}
	items := make([]domain.Item, 0, len(candidates))
	// Prefer complete metadata when duplicate provider rows describe the same title.
	for _, item := range candidates {
		items = append(items, item)
	}
	score := func(item domain.Item) int {
		value := 0
		if continuing[item.ID] {
			value += 60
		}
		if item.ImageURL != "" {
			value += 30
		}
		if item.Description != "" {
			value += 10
		}
		if item.Subtitle != "" {
			value += 5
		}
		if item.Kind == "channel" {
			value -= 10
		}
		for _, id := range previous.Recent {
			if id == item.ID {
				value -= 25
				break
			}
		}
		return value
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := score(items[i]), score(items[j])
		if a != b {
			return a > b
		}
		return items[i].ID < items[j].ID
	})
	seen := map[string]bool{}
	unique := items[:0]
	for _, item := range items {
		identity := strings.ToLower(strings.TrimSpace(item.Title)) + ":" + item.Kind
		if seen[identity] {
			continue
		}
		seen[identity] = true
		unique = append(unique, item)
	}
	if len(unique) == 0 {
		return nil
	} // Client renders its localized onboarding Hero.
	item := unique[0]
	recent := []string{item.ID}
	for _, id := range previous.Recent {
		if id != item.ID && len(recent) < 8 {
			recent = append(recent, id)
		}
	}
	_ = h.store.Put(ctx, "home-hero", key, Selection{ID: item.ID, Until: h.clock().Add(3 * time.Hour), Recent: recent})
	return hero(item)
}

func hero(item domain.Item) *domain.Hero {
	return &domain.Hero{Item: item, Description: item.Description, BackdropURL: item.ImageURL}
}
