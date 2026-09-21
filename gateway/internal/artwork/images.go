// Package artwork creates small JPEG derivatives; provider URLs never reach Android.
package artwork

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}
type entry struct {
	data    []byte
	expires time.Time
	used    time.Time
}
type flight struct {
	done chan struct{}
	data []byte
	err  error
}

type Images struct {
	disk    Cache
	flights map[[32]byte]*flight
	http    HTTPClient
	jobs    chan struct{}
	mu      sync.Mutex
	cache   map[[32]byte]entry
}

func New(client HTTPClient, disk Cache) *Images {
	return &Images{http: client, disk: disk, jobs: make(chan struct{}, 2), cache: map[[32]byte]entry{}, flights: map[[32]byte]*flight{}}
}
func (i *Images) Image(ctx context.Context, source domain.Source, profile domain.ArtworkProfile) (result []byte, err error) {
	if source.ArtworkURL == "" {
		return nil, errors.New("no artwork")
	}
	width, height, valid := profileBounds(profile)
	if !valid {
		return nil, errors.New("invalid artwork profile")
	}
	raw, _ := http.NewRequest("GET", source.ArtworkURL, nil)
	if raw == nil || (raw.URL.Scheme != "http" && raw.URL.Scheme != "https") || raw.URL.User != nil {
		return nil, errors.New("invalid artwork")
	}
	// Include every provider header (cookies and future credentials included), URL,
	// revision and bounded profile. Only the digest becomes a filename.
	identity, _ := json.Marshal(struct {
		Version  int
		URL      string
		Revision string
		Headers  http.Header
		Profile  domain.ArtworkProfile
	}{2, source.ArtworkURL, source.Item.ImageURL, source.ArtworkHeaders, profile})
	key := sha256.Sum256(identity)
	i.mu.Lock()
	if cached, ok := i.cache[key]; ok && time.Now().Before(cached.expires) {
		cached.used = time.Now()
		i.cache[key] = cached
		i.mu.Unlock()
		return cached.data, nil
	}
	if pending, ok := i.flights[key]; ok {
		i.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending.done:
			return pending.data, pending.err
		}
	}
	pending := &flight{done: make(chan struct{})}
	i.flights[key] = pending
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		pending.data, pending.err = result, err
		delete(i.flights, key)
		close(pending.done)
		i.mu.Unlock()
	}()
	if i.disk != nil {
		if data, expires, ok := i.disk.Get(key); ok {
			i.remember(key, data, expires)
			return data, nil
		}
	}
	select {
	case i.jobs <- struct{}{}:
		defer func() { <-i.jobs }()
	default:
		return nil, errors.New("artwork busy")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", source.ArtworkURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = source.ArtworkHeaders.Clone()
	response, err := i.http.Do(req)
	if err != nil {
		return nil, errors.New("artwork unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, errors.New("artwork unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("artwork too large")
	}
	result, err = compress(ctx, data, width, height)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(24 * time.Hour)
	if i.disk != nil {
		i.disk.Put(key, result, expires)
	}
	i.remember(key, result, expires)
	return result, nil
}

func (i *Images) remember(key [32]byte, data []byte, expires time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.cache) >= 32 {
		var oldest [32]byte
		var used time.Time
		for candidate, value := range i.cache {
			if used.IsZero() || value.used.Before(used) {
				oldest, used = candidate, value.used
			}
		}
		delete(i.cache, oldest)
	}
	i.cache[key] = entry{data: data, expires: expires, used: time.Now()}
}
