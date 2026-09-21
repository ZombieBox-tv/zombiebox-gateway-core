package inbox

import (
	"context"
	"errors"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

type replaceSessions struct {
	sessions
	fail bool
}

func (s *replaceSessions) Start(owner string, source domain.Source) (domain.Plan, error) {
	if s.fail {
		return domain.Plan{}, ErrBusy
	}
	return s.sessions.Start(owner, source)
}

func TestAutomaticHandoffDebouncesAndPreservesConfirmedSession(t *testing.T) {
	values := map[string]observation{}
	active := func(id string) observation {
		return observation{source: &domain.Source{URL: "http://private/" + id, Item: domain.Item{ID: id, Provider: id}}, status: domain.NowPlaying{Provider: id, State: "PLAYING"}}
	}
	values["spotify"] = active("spotify")
	streams := &replaceSessions{}
	service := New(backendFunc(func(_ context.Context, id string) (*domain.Source, domain.NowPlaying, error) {
		value := values[id]
		return value.source, value.status, value.err
	}), streams)
	if err := service.Claim("tv", "auto"); err != nil {
		t.Fatal(err)
	}
	first, err := service.Snapshot(context.Background(), "tv")
	if err != nil || first.Plan == nil || first.Plan.Item.Provider != "spotify" || !service.Owned("tv") {
		t.Fatal(first, err)
	}
	values["airplay"] = active("airplay")
	next, err := service.Snapshot(context.Background(), "tv")
	if err != nil || next.Plan.SessionID != first.Plan.SessionID {
		t.Fatal("one observation stole playback", next, err)
	}
	streams.fail = true
	if _, err := service.Snapshot(context.Background(), "tv"); err == nil || len(streams.stopped) != 0 {
		t.Fatal("failed replacement stopped confirmed playback", err)
	}
	streams.fail = false
	switched, err := service.Snapshot(context.Background(), "tv")
	if err != nil || switched.Plan.Item.Provider != "airplay" || len(streams.stopped) != 1 || service.Owned("tv") {
		t.Fatal(switched, err, streams.stopped)
	}
	for range 3 {
		current, err := service.Snapshot(context.Background(), "tv")
		if err != nil || current.Plan.SessionID != switched.Plan.SessionID {
			t.Fatal("steady senders oscillated", current, err)
		}
	}
	values["airplay"] = observation{err: errors.New("unknown network state")}
	if _, err := service.Snapshot(context.Background(), "tv"); err == nil || len(streams.stopped) != 1 {
		t.Fatal("network error treated as stop", err)
	}
	values["airplay"] = observation{}
	service.Snapshot(context.Background(), "tv")
	restored, err := service.Snapshot(context.Background(), "tv")
	if err != nil || restored.Plan == nil || restored.Plan.Item.Provider != "spotify" {
		t.Fatal(restored, err)
	}
	if !service.Dismiss("tv", restored.Plan.SessionID) {
		t.Fatal("dismiss failed")
	}
	for range 3 {
		blocked, _ := service.Snapshot(context.Background(), "tv")
		if blocked.Plan != nil {
			t.Fatal("dismissed source restarted")
		}
	}
}
