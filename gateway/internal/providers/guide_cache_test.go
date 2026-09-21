package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuideCachesAndBoundsStaleFallback(t *testing.T) {
	var requests atomic.Int32
	now := time.Now()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprintf(w, `<tv><programme channel="one" start="%s" stop="%s"><title>News</title></programme></tv>`, now.Add(-time.Minute).Format("20060102150405 -0700"), now.Add(time.Hour).Format("20060102150405 -0700"))
	}))
	defer upstream.Close()
	a := New(upstream.Client(), upstream.Client())
	for i := 0; i < 2; i++ {
		guide, state := a.guide(context.Background(), upstream.URL)
		if state != "FRESH" || len(guide["one"]) != 1 {
			t.Fatal(state, guide)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("guide refetched with every catalog")
	}
	for _, entry := range a.guides.entries {
		if _, state := guideValue(entry, now.Add(6*time.Minute)); state != "STALE" {
			t.Fatal(state)
		}
		if guide, state := guideValue(entry, now.Add(31*time.Minute)); state != "UNAVAILABLE" || guide != nil {
			t.Fatal("unbounded stale metadata")
		}
	}
}
