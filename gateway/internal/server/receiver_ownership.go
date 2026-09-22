package server

import (
	"context"
	"time"
)

// Claim transitions are serialized independently of network/session state locks.
// Older callers retain first-armed exclusion. Replacement is explicit and scoped.
func (s *Server) receiverBusy(device, requested string) bool {
	if requested != "media" && s.mediaReceiverInbox.Active(device) {
		return true
	}
	if requested != "youtube" && s.youtubeReceiver.Active(device) {
		return true
	}
	if requested != "cast" {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.casts {
			if c.receiver == device && time.Now().Before(c.expires) {
				return true
			}
		}
	}
	return false
}

// retireReceivers runs only after the replacement has been prepared successfully.
// Never hold the session mutex across feature or private worker calls.
func (s *Server) retireReceivers(ctx context.Context, device, keep string) {
	if keep != "media" {
		if s.mediaReceiverInbox.Listening(device) {
			s.mediaReceiverInbox.Standby(device)
		} else {
			s.mediaReceiverInbox.Release(device)
		}
	}
	if keep != "youtube" {
		if s.mediaReceiverInbox.Listening(device) {
			s.youtubeReceiver.Suspend(ctx, device)
		} else {
			s.youtubeReceiver.Revoke(ctx, device)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if keep != "cast" {
		for _, cast := range s.casts {
			if cast.receiver == device {
				s.endCastLocked(cast)
			}
		}
	}
	if keep != "youtube" {
		for id, session := range s.sessions {
			if session.device == device && session.receiverID != "" {
				session.cancel()
				delete(s.sessions, id)
			}
		}
	}
}

func (s *Server) receiverHasPlayback(device, requested string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.casts {
		if requested != "cast" && c.receiver == device && c.plan != nil && time.Now().Before(c.expires) {
			return true
		}
	}
	for _, session := range s.sessions {
		if requested != "youtube" && session.device == device && session.receiverID != "" && session.ctx.Err() == nil {
			return true
		}
	}
	return false
}
