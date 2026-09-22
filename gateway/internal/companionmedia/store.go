// Package companionmedia owns bounded, temporary phone uploads, never a public catalog.
package companionmedia

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const MaxBytes int64 = 256 << 20

var ErrBusy = errors.New("upload capacity exhausted")
var ErrInvalid = errors.New("invalid media upload")
var ErrMissing = errors.New("upload unavailable")
var ID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type Asset struct{ ID, Owner, Path, MIME, Kind string }
type entry struct {
	asset          Asset
	expires        time.Time
	ready, discard bool
}
type Store struct {
	mu      sync.Mutex
	root    string
	entries map[string]*entry
	closed  bool
}

// The directory is private to one gateway process. Only our ephemeral filenames
// are removed at startup; neither the media library nor other state is touched.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if !f.IsDir() && regexp.MustCompile(`^upload-[0-9a-f]{32}\.media$`).MatchString(f.Name()) {
			if err := os.Remove(filepath.Join(root, f.Name())); err != nil {
				return nil, err
			}
		}
	}
	return &Store{root: root, entries: map[string]*entry{}}, nil
}

func (s *Store) Put(ctx context.Context, owner, id string, size int64, input io.Reader) (Asset, error) {
	if size < 1 {
		return Asset{}, ErrInvalid
	}
	return s.put(ctx, owner, id, size, input)
}

// PutStream admits an unknown Content-Length only for the bounded URL downloader.
func (s *Store) PutStream(ctx context.Context, owner, id string, input io.Reader) (Asset, error) {
	return s.put(ctx, owner, id, -1, input)
}

func (s *Store) put(ctx context.Context, owner, id string, size int64, input io.Reader) (Asset, error) {
	if !ID.MatchString(id) || (size < 1 && size != -1) || size > MaxBytes {
		return Asset{}, ErrInvalid
	}
	s.mu.Lock()
	if s.closed || len(s.entries) >= 2 || s.entries[id] != nil {
		s.mu.Unlock()
		return Asset{}, ErrBusy
	}
	asset := Asset{ID: id, Owner: owner, Path: filepath.Join(s.root, "upload-"+id+".media")}
	e := &entry{asset: asset, expires: time.Now().Add(10 * time.Minute)}
	s.entries[id] = e
	s.mu.Unlock()
	successful := false
	defer func() {
		if !successful {
			s.mu.Lock()
			_ = os.Remove(asset.Path)
			delete(s.entries, id)
			s.mu.Unlock()
		}
	}()
	f, err := os.OpenFile(asset.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Asset{}, err
	}
	defer f.Close()
	limit := size
	if size == -1 {
		limit = MaxBytes
	}
	prefix := make([]byte, min(limit, 512))
	read, readErr := io.ReadFull(input, prefix)
	if size == -1 && (readErr == io.ErrUnexpectedEOF || readErr == io.EOF) {
		prefix = prefix[:read]
	} else if readErr != nil {
		return Asset{}, ErrInvalid
	}
	asset.MIME, asset.Kind = identify(prefix)
	if asset.MIME == "" {
		return Asset{}, ErrInvalid
	}
	if _, err = f.Write(prefix); err != nil {
		return Asset{}, err
	}
	n, err := io.CopyBuffer(f, io.LimitReader(&contextReader{ctx, input}, limit-int64(len(prefix))+1), make([]byte, 32<<10))
	if err != nil || (size != -1 && n != size-int64(len(prefix))) || n > limit-int64(len(prefix)) || ctx.Err() != nil {
		return Asset{}, ErrInvalid
	}
	if err = f.Close(); err != nil {
		return Asset{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.entries[id] != e || e.discard {
		return Asset{}, ErrMissing
	}
	e.asset, e.ready = asset, true
	successful = true
	return asset, nil
}
func (s *Store) Get(owner, id string) (Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil || e.asset.Owner != owner || !e.ready || e.discard || time.Now().After(e.expires) {
		return Asset{}, ErrMissing
	}
	return e.asset, nil
}
func (s *Store) Retain(owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil || e.asset.Owner != owner || !e.ready || e.discard || time.Now().After(e.expires) {
		return ErrMissing
	}
	e.expires = time.Now().Add(6 * time.Hour)
	return nil
}
func (s *Store) Remove(owner, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil && e.asset.Owner == owner {
		s.remove(id, e)
	}
}
func (s *Store) RemoveOwner(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.entries {
		if e.asset.Owner == owner {
			s.remove(id, e)
		}
	}
}
func (s *Store) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.entries {
		if time.Now().After(e.expires) {
			s.remove(id, e)
		}
	}
}
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for id, e := range s.entries {
		s.remove(id, e)
	}
}
func (s *Store) remove(id string, e *entry) {
	_ = os.Remove(e.asset.Path)
	if e.ready {
		delete(s.entries, id)
	} else {
		e.discard = true
	}
}

type contextReader struct {
	ctx   context.Context
	input io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.input.Read(p)
}
