package home

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type memory map[string][]byte

func (m memory) Get(_ context.Context, _, key string, out any) error {
	return json.Unmarshal(m[key], out)
}
func (m memory) Put(_ context.Context, _, key string, value any) error {
	m[key], _ = json.Marshal(value)
	return nil
}
func TestStableHeroRefreshExpiryAndIsolation(t *testing.T) {
	now := time.Unix(1000, 0)
	store := memory{}
	h := New(store, func() time.Time { return now })
	a := domain.Item{ID: "a", Title: "A", Playable: true, ImageURL: "/image/a"}
	b := domain.Item{ID: "b", Title: "B", Playable: true, ImageURL: "/image/b"}
	rows := []domain.Section{{Items: []domain.Item{a, b}}}
	ctx := context.Background()
	if h.Select(ctx, "one", "", rows).Item.ID != "a" {
		t.Fatal("deterministic choice")
	}
	rows[0].Items[1].Description = "better metadata"
	if New(store, h.clock).Select(ctx, "one", "", rows).Item.ID != "a" {
		t.Fatal("TTL survives restart")
	}
	if h.Select(ctx, "two", "", rows).Item.ID != "b" {
		t.Fatal("device isolation")
	}
	now = now.Add(4 * time.Hour)
	if h.Select(ctx, "one", "", rows).Item.ID != "b" {
		t.Fatal("expiry reranks")
	}
	rows[0].Items = []domain.Item{a}
	if h.Select(ctx, "one", "", rows).Item.ID != "a" {
		t.Fatal("removed candidate retained")
	}
	if h.Select(ctx, "one", "", nil) != nil {
		t.Fatal("stale content instead of onboarding")
	}
}
