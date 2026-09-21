package companion

import (
	"context"
	"time"
)

type receipt struct {
	Target  string
	Grant   string
	Expires time.Time
}

func validCommand(action, provider string) bool {
	if action == "PROVIDER" {
		switch provider {
		case "youtube", "plex", "stremio", "jellyfin", "iptv", "spotify", "airplay":
			return true
		}
		return false
	}
	if provider != "" {
		return false
	}
	switch action {
	case "UP", "DOWN", "LEFT", "RIGHT", "OK", "BACK", "HOME", "PLAY_PAUSE", "STOP", "NEXT", "SEEK_BACK", "SEEK_FORWARD", "VOLUME_UP", "VOLUME_DOWN", "MUTE":
		return true
	}
	return false
}

func (s *Service) Send(ctx context.Context, grant Grant, action, provider string) (Command, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validCommand(action, provider) {
		return Command{}, ErrInvalid
	}
	if !s.Active(ctx, grant.ID, grant.TargetID) {
		return Command{}, ErrDenied
	}
	if s.now().Sub(s.online[grant.TargetID]) > 5*time.Second {
		return Command{}, ErrBusy
	}
	rate := s.commandRates[grant.ID]
	if !s.now().Before(rate.until) {
		rate.count = 0
		rate.until = s.now().Add(time.Second)
	}
	if rate.count >= 5 {
		return Command{}, ErrBusy
	}
	rate.count++
	s.commandRates[grant.ID] = rate
	s.expire()
	queue := s.queues[grant.TargetID]
	if len(queue) >= 16 {
		return Command{}, ErrBusy
	}
	command := Command{ID: s.random(16), GrantID: grant.ID, Action: action, Provider: provider, Expires: s.now().Add(2 * time.Second), RemainingMS: 2000}
	s.queues[grant.TargetID] = append(queue, command)
	s.results[grant.ID] = Result{ID: command.ID, Status: "QUEUED"}
	s.publish(grant.TargetID)
	return command, nil
}

func (s *Service) expire() {
	now := s.now()
	for target, commands := range s.queues {
		pending := commands[:0]
		for _, command := range commands {
			if now.Before(command.Expires) {
				pending = append(pending, command)
			} else if s.results[command.GrantID].ID == command.ID {
				s.results[command.GrantID] = Result{ID: command.ID, Status: "EXPIRED"}
			}
		}
		if len(pending) == 0 {
			delete(s.queues, target)
		} else {
			s.queues[target] = pending
		}
	}
	for id, value := range s.delivered {
		if !now.Before(value.Expires) {
			if s.results[value.Grant].ID == id {
				s.results[value.Grant] = Result{ID: id, Status: "EXPIRED"}
			}
			delete(s.delivered, id)
		}
	}
}

// Poll consumes commands at most once. Lost replies expire instead of replaying
// playback toggles after reconnect. The target reports execution separately.
func (s *Service) Poll(ctx context.Context, target string, active bool) []Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	if active {
		s.online[target] = s.now()
	} else {
		delete(s.online, target)
	}
	out := []Command{}
	for _, command := range s.queues[target] {
		if !active || !s.Active(ctx, command.GrantID, target) {
			s.results[command.GrantID] = Result{ID: command.ID, Status: "BUSY"}
			continue
		}
		command.RemainingMS = command.Expires.Sub(s.now()).Milliseconds()
		if command.RemainingMS <= 0 || len(s.delivered) >= 1024 {
			continue
		}
		s.delivered[command.ID] = receipt{Target: target, Grant: command.GrantID, Expires: command.Expires}
		s.results[command.GrantID] = Result{ID: command.ID, Status: "DELIVERED"}
		out = append(out, command)
	}
	delete(s.queues, target)
	return out
}

func (s *Service) Acknowledge(target, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	value, ok := s.delivered[id]
	if !ok || value.Target != target {
		return ErrDenied
	}
	switch status {
	case "EXECUTED", "BUSY", "UNSUPPORTED":
	default:
		return ErrInvalid
	}
	delete(s.delivered, id)
	if s.results[value.Grant].ID == id {
		s.results[value.Grant] = Result{ID: id, Status: status}
	}
	return nil
}

func (s *Service) RemoteStatus(target, grant string) (bool, Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	return s.now().Sub(s.online[target]) <= 5*time.Second, s.results[grant]
}
