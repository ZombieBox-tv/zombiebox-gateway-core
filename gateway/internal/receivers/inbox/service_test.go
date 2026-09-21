package inbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type backendFunc func(context.Context, string) (*domain.Source, domain.NowPlaying, error)

func (f backendFunc) Read(ctx context.Context, p string) (*domain.Source, domain.NowPlaying, error) {
	return f(ctx, p)
}

type sessions struct {
	started int
	stopped []string
}

func (s *sessions) Start(owner string, source domain.Source) (domain.Plan, error) {
	s.started++
	return domain.Plan{SessionID: fmt.Sprint(s.started), Item: source.Item}, nil
}
func (s *sessions) Stop(id string) { s.stopped = append(s.stopped, id) }

func TestOwnershipMetadataDismissAndIdleRearm(t *testing.T) {
	source := &domain.Source{URL: "http://private/audio", Item: domain.Item{ID: "spotify-connect", Title: "First"}}
	unavailable := false
	backend := backendFunc(func(context.Context, string) (*domain.Source, domain.NowPlaying, error) {
		if unavailable {
			return nil, domain.NowPlaying{}, errors.New("network")
		}
		return source, domain.NowPlaying{State: "PLAYING"}, nil
	})
	streams := &sessions{}
	service := New(backend, streams)
	if err := service.Claim("one", "spotify"); err != nil {
		t.Fatal(err)
	}
	if err := service.Claim("two", "spotify"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if value, err := service.Snapshot(context.Background(), "two"); err != nil || value.Enabled {
		t.Fatal("foreign lease visible", value, err)
	}
	first, err := service.Snapshot(context.Background(), "one")
	if err != nil || first.Plan == nil {
		t.Fatal(first, err)
	}
	source.Item.Title = "Second"
	second, _ := service.Snapshot(context.Background(), "one")
	if streams.started != 1 || second.Plan.Item.Title != "Second" || first.Plan.Item.Title != "First" {
		t.Fatal("metadata restarted audio or mutated old snapshot")
	}
	unavailable = true
	if _, err := service.Snapshot(context.Background(), "one"); err == nil || len(streams.stopped) != 0 {
		t.Fatal("network error ended playback")
	}
	unavailable = false
	if !service.Dismiss("one", first.Plan.SessionID) {
		t.Fatal("dismiss rejected")
	}
	blocked, _ := service.Snapshot(context.Background(), "one")
	if blocked.Plan != nil {
		t.Fatal("dismissed stream restarted")
	}
	source = nil
	service.Snapshot(context.Background(), "one")
	source = &domain.Source{URL: "http://private/audio", Item: domain.Item{ID: "spotify-connect"}}
	resumed, _ := service.Snapshot(context.Background(), "one")
	if resumed.Plan == nil || streams.started != 2 {
		t.Fatal("new sender activity not accepted")
	}
	service.Release("two")
	if !service.Owned("one") {
		t.Fatal("foreign release stole lease")
	}
	service.Release("one")
	if len(streams.stopped) != 2 {
		t.Fatal(streams.stopped)
	}
}

func TestLeaseExpiryAndInputChangesReleaseStreams(t *testing.T) {
	source := domain.Source{URL: "http://origin/one", Item: domain.Item{ID: "airplay-live"}}
	streams := &sessions{}
	service := New(backendFunc(func(context.Context, string) (*domain.Source, domain.NowPlaying, error) {
		return &source, domain.NowPlaying{}, nil
	}), streams)
	now := time.Now()
	service.clock = func() time.Time { return now }
	service.Claim("one", "airplay")
	service.Snapshot(context.Background(), "one")
	source.URL = "http://origin/two"
	service.Snapshot(context.Background(), "one")
	if streams.started != 2 || len(streams.stopped) != 1 {
		t.Fatal("changed origin kept stale session")
	}
	now = now.Add(46 * time.Second)
	service.Sweep()
	if len(streams.stopped) != 2 {
		t.Fatal("expired session survived")
	}
	if err := service.Claim("two", "airplay"); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseCancelsInflightReadAndPreventsLateSession(t *testing.T) {
	entered := make(chan struct{})
	backend := backendFunc(func(ctx context.Context, _ string) (*domain.Source, domain.NowPlaying, error) {
		close(entered)
		<-ctx.Done()
		return &domain.Source{Item: domain.Item{ID: "late"}}, domain.NowPlaying{}, nil
	})
	streams := &sessions{}
	service := New(backend, streams)
	service.Claim("one", "spotify")
	done := make(chan error, 1)
	go func() { _, err := service.Snapshot(context.Background(), "one"); done <- err }()
	<-entered
	service.Release("one")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("late response accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("request not cancelled")
	}
	if streams.started != 0 {
		t.Fatal("created abandoned session")
	}
}
