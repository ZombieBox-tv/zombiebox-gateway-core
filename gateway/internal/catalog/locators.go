package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

// Persistence stores navigation descriptors, never media URLs or credentials.
// A provider/account digest prevents reuse across configuration changes.
type Persistence interface {
	Get(context.Context, string, string, any) error
	Put(context.Context, string, string, any) error
	List(context.Context, string) ([]json.RawMessage, error)
	Count(context.Context, string) (int, error)
	Delete(context.Context, string, string) error
}

type Locator struct {
	Instance                                                 string
	Revision                                                 uint64
	Key, Provider, ConfigKey, ID, Title, Path, Parent, Query string
	Offset                                                   int
	Expires                                                  time.Time
}

func configKey(config domain.Config) string {
	data, _ := json.Marshal(config)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func NewPersistentBrowser(backend Backend, persistence Persistence) *Browser {
	browser := NewBrowser(backend)
	browser.persistence = persistence
	return browser
}

func (b *Browser) locator(ctx context.Context, device, id string) (Locator, bool) {
	var locator Locator
	if b.persistence == nil || b.persistence.Get(ctx, "browse-locators", device+":"+id, &locator) != nil || !b.clock().Before(locator.Expires) {
		return locator, false
	}
	return locator, true
}

func (b *Browser) Provider(ctx context.Context, device, id string) string {
	if entry, ok := b.Source(device, id); ok {
		return entry.Source.Item.Provider
	}
	locator, _ := b.locator(ctx, device, id)
	return locator.Provider
}

// Known validates a saved semantic item without fetching provider pages on Home.
func (b *Browser) Known(ctx context.Context, device, id string, config domain.Config, revision uint64) bool {
	locator, ok := b.locator(ctx, device, id)
	return ok && locator.ConfigKey == configKey(config) && (locator.Instance != b.instance || locator.Revision == revision)
}

// Resolve renews the original provider page after cache expiry or gateway restart.
// It does not persist signed streams, auth headers or upstream response objects.
func (b *Browser) Resolve(ctx context.Context, device, id string, config domain.Config, revision uint64) (Entry, bool) {
	if entry, ok := b.Source(device, id); ok && entry.Revision == revision && entry.ConfigKey == configKey(config) {
		return entry, true
	}
	locator, ok := b.locator(ctx, device, id)
	if !ok || (locator.Instance == b.instance && locator.Revision != revision) || locator.ConfigKey != configKey(config) || b.backend == nil {
		return Entry{}, false
	}
	result, err := b.backend.Browse(ctx, locator.Provider, config, locator.Parent, locator.Query, locator.Offset)
	if err != nil {
		return Entry{}, false
	}
	for _, source := range result.Sources {
		if source.Item.ID != locator.ID || source.BrowsePath != locator.Path {
			continue
		}
		return Entry{Source: source, Revision: revision, ConfigKey: locator.ConfigKey, Device: device, Expires: b.clock().Add(30 * time.Minute)}, true
	}
	return Entry{}, false
}

func (b *Browser) saveLocator(ctx context.Context, locator Locator) {
	if b.persistence == nil {
		return
	}
	count, err := b.persistence.Count(ctx, "browse-locators")
	if err != nil {
		return
	}
	if count >= 4096 {
		entries, err := b.persistence.List(ctx, "browse-locators")
		if err != nil {
			return
		}
		var oldest Locator
		for _, raw := range entries {
			var candidate Locator
			if json.Unmarshal(raw, &candidate) == nil && (oldest.Key == "" || candidate.Expires.Before(oldest.Expires)) {
				oldest = candidate
			}
		}
		if oldest.Key == "" || b.persistence.Delete(ctx, "browse-locators", oldest.Key) != nil {
			return
		}
	}
	_ = b.persistence.Put(ctx, "browse-locators", locator.Key, locator)
}
