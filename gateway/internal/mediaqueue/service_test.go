package mediaqueue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOrderedCompletionIsolationAndCancellation(t *testing.T) {
	var s Service
	defer s.Close()
	started, stopped := make(chan string, 4), make(chan string, 4)
	n := 0
	id := func() string { n++; return string(rune('a' + n)) }
	items := []Item{{URL: "https://example.com/one", Title: "One"}, {URL: "https://example.com/two", Title: "Two"}}
	if !s.Start("owner", "queue", items, func(ctx context.Context, item Item, id string) error { started <- id; return nil }, func(id string) { stopped <- id }, id) {
		t.Fatal("start rejected")
	}
	first := receive(t, started)
	s.Complete("other", first, true)
	s.Complete("owner", "stale", true)
	select {
	case <-stopped:
		t.Fatal("foreign completion advanced")
	default:
	}
	s.Complete("owner", first, true)
	s.Complete("owner", first, true)
	if receive(t, stopped) != first {
		t.Fatal("wrong cleanup")
	}
	second := receive(t, started)
	s.Cancel("owner")
	if receive(t, stopped) != second {
		t.Fatal("wrong cancellation")
	}
	if s.Busy() || s.State("other").Phase != "NONE" {
		t.Fatal("state leaked or not stopped")
	}
}

func TestFailureStopsWithoutSkippingAndCloseCancelsPreparation(t *testing.T) {
	var s Service
	started, stopped := make(chan string, 1), make(chan string, 1)
	items := []Item{{URL: "https://example.com/one"}}
	s.Start("owner", "queue", items, func(ctx context.Context, item Item, id string) error { started <- id; <-ctx.Done(); return ctx.Err() }, func(id string) { stopped <- id }, func() string { return "a" })
	receive(t, started)
	s.Close()
	receive(t, stopped)
	if s.Busy() {
		t.Fatal("close retained busy state")
	}
	var failed Service
	defer failed.Close()
	failed.Start("owner", "queue", items, func(context.Context, Item, string) error { return errors.New("unavailable") }, func(id string) { stopped <- id }, func() string { return "b" })
	receive(t, stopped)
	if failed.State("owner").Phase != "FAILED" {
		t.Fatal("failed item not surfaced")
	}
}

func TestQueueValidation(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "https://user:pass@example.com/a", "https://example.com/a#fragment"} {
		if Validate([]Item{{URL: raw}}) == nil {
			t.Fatal("accepted", raw)
		}
	}
	if Validate(make([]Item, 17)) == nil {
		t.Fatal("unbounded queue")
	}
}

func receive(t *testing.T, c <-chan string) string {
	t.Helper()
	select {
	case value := <-c:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("queue did not progress")
		return ""
	}
}
