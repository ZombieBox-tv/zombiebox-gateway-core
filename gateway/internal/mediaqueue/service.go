// Package mediaqueue owns one bounded, ephemeral sender playlist on the gateway.
package mediaqueue

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Item struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

type State struct {
	ID    string `json:"id,omitempty"`
	Phase string `json:"phase"`
	Index int    `json:"index"`
	Count int    `json:"count"`
	Title string `json:"title,omitempty"`
}

type run struct {
	owner   string
	state   State
	mediaID string
	cancel  context.CancelFunc
	next    chan bool
}

type Service struct {
	mu      sync.Mutex
	current *run
	closed  bool
	workers sync.WaitGroup
}

func Validate(items []Item) error {
	if len(items) == 0 || len(items) > 16 {
		return errors.New("queue requires 1 to 16 URLs")
	}
	for _, item := range items {
		u, err := url.Parse(item.URL)
		if err != nil || len(item.URL) > 4096 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") || len([]rune(item.Title)) > 120 || strings.IndexFunc(item.Title, unicode.IsControl) >= 0 {
			return errors.New("invalid queue item")
		}
	}
	return nil
}

// Start never fetches in the request goroutine. The caller injects cancellable
// preparation and cleanup; no URL, token or playlist survives gateway restart.
func (s *Service) Start(owner, id string, items []Item, start func(context.Context, Item, string) error, stop func(string), random func() string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || Validate(items) != nil {
		return false
	}
	if s.current != nil && s.current.owner == owner && s.current.state.ID == id {
		return true
	}
	if s.current != nil && active(s.current.state.Phase) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	r := &run{owner: owner, state: State{ID: id, Phase: "PREPARING", Count: len(items)}, cancel: cancel, next: make(chan bool, 1)}
	s.current = r
	items = append([]Item(nil), items...)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer cancel()
		defer func() {
			s.mu.Lock()
			if active(r.state.Phase) {
				r.state.Phase = "STOPPED"
			}
			s.mu.Unlock()
		}()
		for index, item := range items {
			mediaID := random()
			s.mu.Lock()
			if ctx.Err() != nil {
				s.mu.Unlock()
				return
			}
			r.mediaID = mediaID
			r.state.Index, r.state.Title, r.state.Phase = index, item.Title, "PREPARING"
			s.mu.Unlock()
			err := start(ctx, item, mediaID)
			s.mu.Lock()
			if ctx.Err() != nil {
				s.mu.Unlock()
				stop(mediaID)
				return
			}
			if err != nil {
				r.state.Phase = "FAILED"
				s.mu.Unlock()
				stop(mediaID)
				return
			}
			r.state.Phase = "PLAYING"
			s.mu.Unlock()
			advance := false
			select {
			case advance = <-r.next:
			case <-ctx.Done():
			}
			stop(mediaID)
			if !advance || ctx.Err() != nil {
				return
			}
		}
		s.mu.Lock()
		if ctx.Err() == nil {
			r.state.Phase = "FINISHED"
		}
		s.mu.Unlock()
	}()
	return true
}

func active(phase string) bool { return phase == "PREPARING" || phase == "PLAYING" }
func (s *Service) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current != nil && active(s.current.state.Phase)
}
func (s *Service) State(owner string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || s.current.owner != owner {
		return State{Phase: "NONE"}
	}
	return s.current.state
}

// Only the current owned playback completion may advance the list. Stop, revoke,
// failure and receiver replacement cancel it; repeated/stale events do nothing.
func (s *Service) Complete(owner, mediaID string, advance bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.current
	if r == nil || r.owner != owner || r.mediaID != mediaID || !active(r.state.Phase) {
		return
	}
	if !advance {
		r.state.Phase = "STOPPED"
		r.cancel()
		return
	}
	r.mediaID = ""
	select {
	case r.next <- true:
	default:
	}
}
func (s *Service) Cancel(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.current; r != nil && r.owner == owner && active(r.state.Phase) {
		r.state.Phase = "STOPPED"
		r.cancel()
	}
}
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	if r := s.current; r != nil {
		r.state.Phase = "STOPPED"
		r.cancel()
	}
	s.mu.Unlock()
	s.workers.Wait()
}

func (s *Service) Owner() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && active(s.current.state.Phase) {
		return s.current.owner
	}
	return ""
}
