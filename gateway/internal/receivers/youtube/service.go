// Package youtube owns receiver leases and commands independently of HTTP handlers.
package youtube

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

var (
	ErrUnsupported = errors.New("receiver_unavailable")
	ErrUnavailable = errors.New("receiver_unavailable")
	ErrInUse       = errors.New("receiver_in_use")
	ErrDisabled    = errors.New("provider_disabled")
	ErrNotFound    = errors.New("receiver_not_found")
	ErrBusy        = errors.New("receiver_busy")
	ErrInvalid     = errors.New("invalid_receiver_state")
)

// Backend is the replaceable private worker boundary consumed by this feature.
type Backend interface {
	OpenReceiver(context.Context, domain.Config, string) (domain.YouTubeReceiverState, error)
	PollReceiver(context.Context, domain.Config, string) (domain.YouTubeReceiverState, error)
	AcknowledgeReceiver(context.Context, domain.Config, string, domain.ReceiverAcknowledgement) error
	CloseReceiver(context.Context, domain.Config, string) error
}
type lease struct {
	id, device string
	config     domain.Config
	expires    time.Time
	busy       bool
	source     *domain.Source
}
type Service struct {
	mu      sync.Mutex
	backend Backend
	config  func(context.Context, string) domain.Config
	newID   func() string
	owner   *lease
	closed  bool
}

func New(backend Backend, config func(context.Context, string) domain.Config, newID func() string) *Service {
	return &Service{backend: backend, config: config, newID: newID}
}
func (s *Service) Active(device string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner != nil && s.owner.device == device && (s.owner.busy || time.Now().Before(s.owner.expires))
}

func (s *Service) Open(ctx context.Context, device string) (domain.YouTubeReceiverState, error) {
	empty := domain.YouTubeReceiverState{}
	s.mu.Lock()
	if s.backend == nil {
		s.mu.Unlock()
		return empty, ErrUnsupported
	}
	if s.closed {
		s.mu.Unlock()
		return empty, ErrUnavailable
	}
	if s.owner != nil && (s.owner.busy || time.Now().Before(s.owner.expires)) {
		s.mu.Unlock()
		return empty, ErrInUse
	}
	config, provider := s.config(ctx, "youtube_receiver"), s.config(ctx, "youtube")
	if !config.Enabled || !provider.Enabled {
		s.mu.Unlock()
		return empty, ErrDisabled
	}
	owner := &lease{id: s.newID(), device: device, config: config, expires: time.Now().Add(45 * time.Second), busy: true}
	s.owner = owner
	s.mu.Unlock()
	state, err := s.backend.OpenReceiver(ctx, config, owner.id)
	s.mu.Lock()
	owner.busy = false
	if err != nil || !ValidState(state, owner.id) || s.closed {
		s.owner = nil
		s.mu.Unlock()
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.backend.CloseReceiver(cleanup, config, owner.id)
		return empty, ErrUnavailable
	}
	owner.expires = time.Now().Add(45 * time.Second)
	s.mu.Unlock()
	return state, nil
}
func (s *Service) acquire(device, id string, allowExpired bool) (*lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.owner
	if owner == nil || owner.device != device || owner.id != id || (!allowExpired && time.Now().After(owner.expires)) {
		return nil, ErrNotFound
	}
	if owner.busy {
		return nil, ErrBusy
	}
	owner.busy = true
	return owner, nil
}
func (s *Service) release(owner *lease) {
	s.mu.Lock()
	owner.busy = false
	s.mu.Unlock()
}
func (s *Service) Poll(ctx context.Context, device, id string) (domain.YouTubeReceiverState, error) {
	empty := domain.YouTubeReceiverState{}
	owner, err := s.acquire(device, id, false)
	if err != nil {
		return empty, err
	}
	defer s.release(owner)
	state, err := s.backend.PollReceiver(ctx, owner.config, id)
	if err != nil || !ValidState(state, id) {
		return empty, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != owner {
		return empty, ErrNotFound
	}
	owner.expires = time.Now().Add(45 * time.Second)
	if command := state.Command; command != nil && command.Action == "play" {
		config := s.config(ctx, "youtube")
		if !config.Enabled {
			return empty, ErrDisabled
		}
		source := domain.Source{
			Item: domain.Item{
				ID:       "youtube-" + command.VideoID,
				Provider: "youtube",
				Kind:     "video",
				Title:    "YouTube",
				Playable: true,
			},
			URL:     strings.TrimRight(config.URL, "/") + "/resolve/" + command.VideoID,
			Headers: http.Header{"Authorization": {"Bearer " + config.Token}},
			MIME:    "application/x-zombie-youtube",
		}
		owner.source = &source
		command.ItemID = source.Item.ID
	}
	return state, nil
}
func (s *Service) Acknowledge(ctx context.Context, device, id string, state domain.ReceiverAcknowledgement) error {
	owner, err := s.acquire(device, id, false)
	if err != nil {
		return err
	}
	defer s.release(owner)
	if !ValidAcknowledgement(state) {
		return ErrInvalid
	}
	if s.backend.AcknowledgeReceiver(ctx, owner.config, id, state) != nil {
		return ErrUnavailable
	}
	return nil
}
func (s *Service) Stop(ctx context.Context, device, id string) error {
	owner, err := s.acquire(device, id, true)
	if err != nil {
		return err
	}
	defer s.release(owner)
	err = s.backend.CloseReceiver(ctx, owner.config, id)
	s.mu.Lock()
	if s.owner == owner {
		s.owner = nil
	}
	s.mu.Unlock()
	if err != nil {
		return ErrUnavailable
	}
	return nil
}
func (s *Service) Source(device, id string) *domain.Source {
	source, _ := s.SourceWithLease(device, id)
	return source
}
func (s *Service) SourceWithLease(device, id string) (*domain.Source, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.owner
	if owner == nil || owner.device != device || time.Now().After(owner.expires) || owner.source == nil || owner.source.Item.ID != id {
		return nil, ""
	}
	copy := *owner.source
	return &copy, owner.id
}
func (s *Service) Close(ctx context.Context) {
	s.mu.Lock()
	s.closed = true
	owner := s.owner
	s.owner = nil
	s.mu.Unlock()
	if owner != nil && s.backend != nil {
		_ = s.backend.CloseReceiver(ctx, owner.config, owner.id)
	}
}

// Revoke detaches first, so an in-flight poll cannot resurrect an old lease.
// The caller serializes new claims; private worker cleanup is bounded separately.
func (s *Service) Revoke(ctx context.Context, device string) {
	s.mu.Lock()
	owner := s.owner
	if owner == nil || owner.device != device {
		s.mu.Unlock()
		return
	}
	s.owner = nil
	s.mu.Unlock()
	if s.backend != nil {
		cleanup, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_ = s.backend.CloseReceiver(cleanup, owner.config, owner.id)
	}
}

func (s *Service) SourceLease(device, item string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := s.owner
	if owner == nil || owner.device != device || time.Now().After(owner.expires) || owner.source == nil || owner.source.Item.ID != item {
		return ""
	}
	return owner.id
}
