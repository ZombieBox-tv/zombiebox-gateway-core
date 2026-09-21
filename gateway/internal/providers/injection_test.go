package providers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type doFunc func(*http.Request) (*http.Response, error)

func (f doFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func TestIndependentAdaptersUseInjectedTransport(t *testing.T) {
	var wg sync.WaitGroup
	for _, title := range []string{"First", "Second"} {
		wg.Add(1)
		go func(title string) {
			defer wg.Done()
			transport := doFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "fixture.invalid" || r.Header.Get("Authorization") != "Bearer private" {
					t.Error("request policy lost")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("#EXTM3U\n#EXTINF:-1," + title + "\nhttps://fixture.invalid/media\n"))}, nil
			})
			adapters := New(transport, transport)
			sources, err := adapters.Fetch(context.Background(), "iptv", Config{Enabled: true, URL: "https://fixture.invalid/list", Token: "private"}, "")
			if err != nil || len(sources) != 1 || sources[0].Item.Title != title {
				t.Errorf("registry crossed instance boundaries: %v %v", sources, err)
			}
		}(title)
	}
	wg.Wait()
}
