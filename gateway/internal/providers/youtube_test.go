package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestYouTubeWrapperBoundary(t *testing.T) {
	secret := strings.Repeat("s", 32)
	origin := "https://r1.googlevideo.com/videoplayback?signature=private"
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("missing worker authentication")
		}
		if r.URL.Path == "/catalog" {
			if r.URL.Query().Get("q") != "nature & science" {
				t.Error("query encoding")
			}
			_, _ = w.Write([]byte(`{"items":[{"id":"aqz-KE-bpKQ","title":"Example","durationMs":1000},{"id":"invalid"},{"id":"aqz-KE-bpKQ"}]}`))
		} else {
			_ = json.NewEncoder(w).Encode(map[string]string{"url": origin, "mimeType": "video/mp4"})
		}
	}))
	defer wrapper.Close()
	sources, err := YouTube(context.Background(), Config{URL: wrapper.URL, Token: secret, CatalogID: "nature & science"})
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources: %v %v", sources, err)
	}
	resolved, err := Resolve(context.Background(), sources[0])
	if err != nil || resolved.URL != origin || len(resolved.Headers) != 0 {
		t.Fatalf("resolve or credential isolation: %v", err)
	}
	for _, bad := range []string{"http://r1.googlevideo.com/x", "https://r1.googlevideo.com.evil.test/x", "https://user:secret@r1.googlevideo.com/x", "file:///etc/passwd"} {
		origin = bad
		if _, err = Resolve(context.Background(), sources[0]); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
