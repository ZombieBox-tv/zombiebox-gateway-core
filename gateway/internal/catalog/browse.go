// Package catalog owns bounded device-scoped navigation and source retention.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

var ErrExpired = errors.New("browse node expired")

type Backend interface {
	Browse(context.Context, string, domain.Config, string, string, int) (domain.BrowseResult, error)
}

type Entry struct {
	Source   domain.Source
	Revision uint64
	Device   string
	Expires  time.Time
}

type Browser struct {
	backend Backend
	mu      sync.Mutex
	entries map[string]Entry
	clock   func() time.Time
}

func NewBrowser(backend Backend) *Browser {
	return &Browser{backend: backend, entries: map[string]Entry{}, clock: time.Now}
}

func (b *Browser) Source(device, id string) (Entry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[device+":"+id]
	return entry, ok && b.clock().Before(entry.Expires)
}

func (b *Browser) Page(ctx context.Context, device, provider, title string, revision uint64, config domain.Config, parent, query string, offset int) (domain.BrowsePage, error) {
	path := ""
	if parent != "" {
		entry, ok := b.Source(device, parent)
		if !ok || entry.Revision != revision || entry.Source.Item.Provider != provider || entry.Source.BrowsePath == "" {
			return domain.BrowsePage{}, ErrExpired
		}
		path, title = entry.Source.BrowsePath, entry.Source.Item.Title
	}
	result, err := b.backend.Browse(ctx, provider, config, path, query, offset)
	if err != nil {
		return domain.BrowsePage{}, err
	}
	page := domain.BrowsePage{Title: title, Items: []domain.Item{}, NextOffset: result.NextOffset}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	for key, entry := range b.entries {
		if !now.Before(entry.Expires) {
			delete(b.entries, key)
		}
	}
	for i, source := range result.Sources {
		if i >= 40 {
			break
		}
		// Stable opaque IDs permit focus restoration without exposing upstream paths.
		sum := sha256.Sum256([]byte(provider + "\x00" + source.Item.ID + "\x00" + source.BrowsePath))
		id := "browse-" + hex.EncodeToString(sum[:16])
		if source.BrowsePath != "" {
			source.Item.ID = id
			source.Item.BrowseID = id
		} else {
			id = source.Item.ID
		}
		if source.ArtworkURL != "" {
			source.Item.ImageURL = "/v1/artwork/" + id
		}
		if len(b.entries) >= 4096 {
			oldestKey := ""
			var oldest time.Time
			for key, entry := range b.entries {
				if oldestKey == "" || entry.Expires.Before(oldest) {
					oldestKey, oldest = key, entry.Expires
				}
			}
			delete(b.entries, oldestKey)
		}
		b.entries[device+":"+id] = Entry{Source: source, Revision: revision, Device: device, Expires: now.Add(30 * time.Minute)}
		page.Items = append(page.Items, source.Item)
	}
	return page, nil
}
