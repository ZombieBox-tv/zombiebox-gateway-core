package youtube

import (
	"context"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

type delayedBackend struct{ started, finish chan struct{} }

func (b *delayedBackend) OpenReceiver(_ context.Context, _ domain.Config, id string) (domain.YouTubeReceiverState, error) {
	return domain.YouTubeReceiverState{ReceiverID: id, State: "READY"}, nil
}
func (b *delayedBackend) PollReceiver(_ context.Context, _ domain.Config, id string) (domain.YouTubeReceiverState, error) {
	close(b.started)
	<-b.finish
	return domain.YouTubeReceiverState{ReceiverID: id, State: "READY", Command: &domain.ReceiverCommand{ID: strings.Repeat("a", 32), Action: "play", VideoID: "abcdefghijk"}}, nil
}
func (b *delayedBackend) AcknowledgeReceiver(context.Context, domain.Config, string, domain.ReceiverAcknowledgement) error {
	return nil
}
func (b *delayedBackend) CloseReceiver(context.Context, domain.Config, string) error { return nil }

func TestRevokedPollCannotResurrectSourceOrLease(t *testing.T) {
	backend := &delayedBackend{make(chan struct{}), make(chan struct{})}
	service := New(backend, func(context.Context, string) domain.Config {
		return domain.Config{Enabled: true, URL: "https://fixture.invalid"}
	}, func() string { return "lease" })
	if _, err := service.Open(t.Context(), "device"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := service.Poll(t.Context(), "device", "lease"); done <- err }()
	<-backend.started
	service.Revoke(t.Context(), "device")
	close(backend.finish)
	if err := <-done; err != ErrNotFound {
		t.Fatal("late poll accepted", err)
	}
	if service.Active("device") || service.Source("device", "youtube-abcdefghijk") != nil {
		t.Fatal("revoked receiver resurrected")
	}
}

type suspendBackend struct{ delayedBackend }

func (b *suspendBackend) SuspendReceiver(context.Context, domain.Config, string, string) error {
	return nil
}

func TestSuspendedListenerRetainsLeaseButFencesLatePlay(t *testing.T) {
	backend := &suspendBackend{delayedBackend{make(chan struct{}), make(chan struct{})}}
	service := New(backend, func(context.Context, string) domain.Config {
		return domain.Config{Enabled: true, URL: "https://fixture.invalid"}
	}, func() string { return "lease" })
	service.Open(t.Context(), "device")
	done := make(chan error, 1)
	go func() { _, err := service.Poll(t.Context(), "device", "lease"); done <- err }()
	<-backend.started
	service.Suspend(t.Context(), "device")
	close(backend.finish)
	if err := <-done; err != ErrBusy {
		t.Fatal("late pre-handoff command accepted", err)
	}
	if !service.Active("device") || service.Source("device", "youtube-abcdefghijk") != nil {
		t.Fatal("lost listener or retained source")
	}
}
