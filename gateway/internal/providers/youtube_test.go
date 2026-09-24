package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type youtubeHTTPClient func(*http.Request) (*http.Response, error)

func (client youtubeHTTPClient) Do(request *http.Request) (*http.Response, error) {
	return client(request)
}

func TestYouTubeResolutionUsesPrivateTransport(t *testing.T) {
	metadata := youtubeHTTPClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("resolution used the short metadata transport")
		return nil, errors.New("unexpected transport")
	})
	private := youtubeHTTPClient(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer private" {
			t.Fatal("missing wrapper authorization")
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"url":"https://r1.googlevideo.com/video","mimeType":"video/mp4"}`)),
		}, nil
	})
	adapter := New(metadata, private)
	source := Source{URL: "http://wrapper.local/resolve/aqz-KE-bpKQ", MIME: "application/x-zombie-youtube", Headers: http.Header{"Authorization": {"Bearer private"}}}
	resolved, err := adapter.Resolve(context.Background(), source)
	if err != nil || resolved.MIME != "video/mp4" || resolved.Headers != nil {
		t.Fatalf("resolution failed: %v", err)
	}
}

func TestYouTubeResolutionRetriesOnlyTransientBusy(t *testing.T) {
	source := Source{URL: "http://wrapper.local/resolve/aqz-KE-bpKQ", MIME: "application/x-zombie-youtube"}
	requests := 0
	private := youtubeHTTPClient(func(*http.Request) (*http.Response, error) {
		requests++
		status, body := http.StatusServiceUnavailable, `{"error":"busy"}`
		if requests == 3 {
			status, body = http.StatusOK, `{"url":"https://r1.googlevideo.com/video","mimeType":"video/mp4"}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	adapter := New(private, private)
	resolved, err := adapter.Resolve(context.Background(), source)
	if err != nil || requests != 3 || resolved.URL != "https://r1.googlevideo.com/video" {
		t.Fatalf("busy retry: requests=%d url=%q err=%v", requests, resolved.URL, err)
	}
	requests = 0
	private = youtubeHTTPClient(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader(`{"error":"provider_unavailable"}`))}, nil
	})
	adapter = New(private, private)
	if _, err := adapter.Resolve(context.Background(), source); err == nil || requests != 1 {
		t.Fatalf("non-busy failure was retried: requests=%d err=%v", requests, err)
	}
}

func TestYouTubeResolutionBusyRetryHonorsCancellation(t *testing.T) {
	requests := 0
	private := youtubeHTTPClient(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{"error":"busy"}`))}, nil
	})
	adapter := New(private, private)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := adapter.Resolve(ctx, Source{URL: "http://wrapper.local/resolve/aqz-KE-bpKQ", MIME: "application/x-zombie-youtube"})
	if !errors.Is(err, context.Canceled) || requests > 1 {
		t.Fatalf("cancelled busy retry: requests=%d err=%v", requests, err)
	}
}

func TestYouTubeWrapperBoundary(t *testing.T) {
	secret := strings.Repeat("s", 32)
	origin := "https://r1.googlevideo.com/videoplayback?signature=private"
	audioOrigin := "https://r2.googlevideo.com/audio?signature=private"
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
			_ = json.NewEncoder(w).Encode(map[string]string{"url": origin, "audioUrl": audioOrigin, "mimeType": "video/mp4"})
		}
	}))
	defer wrapper.Close()
	sources, err := testAdapters.YouTube(context.Background(), Config{URL: wrapper.URL, Token: secret, CatalogID: "nature & science"})
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources: %v %v", sources, err)
	}
	resolved, err := testAdapters.Resolve(context.Background(), sources[0])
	if err != nil || resolved.URL != origin || resolved.AudioURL != audioOrigin || len(resolved.Headers) != 0 || len(resolved.AudioHeaders) != 0 {
		t.Fatalf("resolve or credential isolation: %v", err)
	}
	for _, bad := range []string{"http://r1.googlevideo.com/x", "https://r1.googlevideo.com.evil.test/x", "https://user:secret@r1.googlevideo.com/x", "file:///etc/passwd"} {
		origin = bad
		if _, err = testAdapters.Resolve(context.Background(), sources[0]); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	origin = "https://r1.googlevideo.com/video"
	audioOrigin = "https://private.example/audio"
	if _, err = testAdapters.Resolve(context.Background(), sources[0]); err == nil {
		t.Fatal("untrusted audio origin accepted")
	}
}
