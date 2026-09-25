// Package inbox owns one explicitly armed media receiver with optional sender handoff.
package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

var ErrBusy = errors.New("receiver busy")
var ErrChanged = errors.New("receiver changed")

type Backend interface {
	Read(context.Context, string) (*domain.Source, domain.NowPlaying, error)
}
type Sessions interface {
	Start(context.Context, string, domain.Source) (domain.Plan, error)
	Stop(string)
}
type Snapshot struct {
	Provider   string             `json:"provider"`
	Enabled    bool               `json:"enabled"`
	Plan       *domain.Plan       `json:"plan"`
	NowPlaying *domain.NowPlaying `json:"nowPlaying,omitempty"`
}

type Service struct {
	mu                       sync.Mutex
	backend                  Backend
	sessions                 Sessions
	clock                    func() time.Time
	poll                     chan struct{}
	owner, provider, blocked string
	selection                selection
	expires                  time.Time
	generation               uint64
	plan                     *domain.Plan
	sourceKey                [32]byte
	cancel                   context.CancelFunc
}

func New(backend Backend, sessions Sessions) *Service {
	return &Service{backend: backend, sessions: sessions, clock: time.Now, poll: make(chan struct{}, 1)}
}

func (s *Service) Claim(owner, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	if s.owner != "" && s.owner != owner {
		return ErrBusy
	}
	s.clear()
	s.owner, s.provider = owner, provider
	s.expires = s.clock().Add(45 * time.Second)
	return nil
}

// Active includes an armed receiver, so another transport cannot steal its screen.
func (s *Service) Active(owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	return s.owner == owner
}

func (s *Service) Owned(owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	return s.owner == owner && (s.provider == "spotify" || ((s.provider == "auto" || s.provider == "universal") && s.selection.current == "spotify"))
}

func (s *Service) Release(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == owner {
		s.clear()
	}
}

// A local stop suppresses the same incoming session until idle or explicit re-arm.
func (s *Service) Dismiss(owner, session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == owner && s.plan != nil && s.plan.SessionID == session {
		s.blocked = s.plan.Item.ID
		if (s.provider == "auto" || s.provider == "universal") && s.selection.blocked != nil {
			s.selection.blocked[s.selection.current] = s.plan.Item.ID
		}
		s.stopPlan()
		return true
	}
	return false
}

func (s *Service) Snapshot(ctx context.Context, owner string) (Snapshot, error) {
	s.mu.Lock()
	s.expire()
	if s.owner != owner {
		s.mu.Unlock()
		return Snapshot{}, nil
	}
	s.expires = s.clock().Add(45 * time.Second)
	provider, generation := s.provider, s.generation
	s.mu.Unlock()
	select {
	case s.poll <- struct{}{}:
		defer func() { <-s.poll }()
	default:
		return Snapshot{}, ErrBusy
	}
	request, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	s.mu.Lock()
	if s.owner != owner || s.generation != generation {
		s.mu.Unlock()
		return Snapshot{}, ErrChanged
	}
	s.cancel = cancel
	s.mu.Unlock()
	values := s.read(request, provider)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	if s.owner != owner || s.generation != generation {
		return Snapshot{}, ErrChanged
	}
	s.cancel = nil
	selected := provider
	if provider == "auto" || provider == "universal" {
		selected = s.selection.choose(values)
	}
	value := values[selected]
	if selected == "" {
		value.status = domain.NowPlaying{Provider: "auto", State: "STOPPED"}
		if values["spotify"].err != nil && values["airplay"].err != nil {
			value.err = values["spotify"].err
		}
	}
	source, status, err := value.source, value.status, value.err
	// Unknown network state never means sender stopped.
	if err != nil {
		return Snapshot{}, err
	}
	if request.Err() != nil {
		return Snapshot{}, request.Err()
	}
	if source == nil {
		s.stopPlan()
		s.blocked = ""
	} else if source.Item.ID != s.blocked && (provider != "auto" || s.selection.blocked[selected] != source.Item.ID) {
		keyData, _ := json.Marshal(struct {
			URL     string
			Headers any
		}{source.URL, source.Headers})
		key := sha256.Sum256(keyData)
		if s.plan == nil || s.plan.Item.ID != source.Item.ID || s.sourceKey != key {
			// Preserve the confirmed stream if replacement allocation fails.
			s.mu.Unlock()
			plan, err := s.sessions.Start(request, owner, *source)
			s.mu.Lock()
			s.expire()
			if s.owner != owner || s.generation != generation {
				if err == nil {
					s.sessions.Stop(plan.SessionID)
				}
				return Snapshot{}, ErrChanged
			}
			if err != nil {
				return Snapshot{}, err
			}
			s.stopPlan()
			s.plan = &plan
			s.sourceKey = key
		}
		s.plan.Item = source.Item
		if provider == "auto" || provider == "universal" {
			s.selection.current = selected
		}
	}
	out := Snapshot{Enabled: true, Provider: provider, NowPlaying: &status}
	if s.plan != nil {
		copy := *s.plan
		out.Plan = &copy
	}
	return out, nil
}

func (s *Service) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
}
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
}
func (s *Service) expire() {
	if s.owner != "" && !s.clock().Before(s.expires) {
		s.clear()
	}
}
func (s *Service) stopPlan() {
	if s.plan != nil {
		s.sessions.Stop(s.plan.SessionID)
		s.plan = nil
	}
}
func (s *Service) clear() {
	s.generation++
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.stopPlan()
	s.owner, s.provider, s.blocked = "", "", ""
	s.selection = selection{}
}

// Listening retains the explicit universal arm independently of playback ownership.
func (s *Service) Listening(owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	return s.owner == owner && s.provider == "universal"
}

func (s *Service) Standby(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != owner || s.provider != "universal" {
		return
	}
	s.generation++
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.selection.blocked == nil {
		s.selection.blocked = map[string]string{}
	}
	for _, provider := range []string{"spotify", "airplay"} {
		playing, known := s.selection.playing[provider]
		if playing || !known {
			s.selection.blocked[provider] = "*"
		}
	}
	s.stopPlan()
	s.selection.current, s.selection.pending = "", ""
}
