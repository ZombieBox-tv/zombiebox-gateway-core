package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpotifyHealthSeparatesPairingFromAccountReadiness(t *testing.T) {
	for _, test := range []struct {
		name, mode         string
		codeStatus, status int
		ready, pairing     bool
		shouldFail         bool
	}{
		{"device code pending", "device_auth", 200, 503, false, true, false},
		{"device account authorized", "device_auth", 204, 200, true, false, false},
		{"local phone pairing pending", "zeroconf", 204, 503, false, true, false},
		{"local account authorized", "zeroconf", 204, 200, true, false, false},
		{"device code unavailable", "device_auth", 204, 503, false, false, true},
		{"no account in successful status", "zeroconf", 204, 200, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("credentials:\n  type: "+test.mode+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth/code" {
					w.WriteHeader(test.codeStatus)
					_, _ = io.WriteString(w, `{"code":"private-pairing-code"}`)
					return
				}
				w.WriteHeader(test.status)
				if test.name != "no account in successful status" {
					_, _ = io.WriteString(w, `{"username":"private-account"}`)
				}
			}))
			defer upstream.Close()
			health, err := spotifyHealth(context.Background(), &http.Client{Timeout: time.Second}, upstream.URL, dir)
			if (err != nil) != test.shouldFail || health.Ready != test.ready || health.AuthorizationRequired != test.pairing || health.AuthMode != test.mode {
				t.Fatalf("health = %+v, error = %v", health, err)
			}
		})
	}
}

func TestSpotifyHealthReportsOnlyTracklessBuffering(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("credentials:\n  type: zeroconf\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var trackLoaded atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/code" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if trackLoaded.Load() {
			_, _ = io.WriteString(w, `{"username":"private-account","buffering":true,"track":{"name":"private-title"}}`)
		} else {
			_, _ = io.WriteString(w, `{"username":"private-account","buffering":true,"track":null}`)
		}
	}))
	defer upstream.Close()
	client := &http.Client{Timeout: time.Second}
	first, err := spotifyHealth(context.Background(), client, upstream.URL, dir)
	if err != nil || !first.Ready || !first.BufferingWithoutTrack {
		t.Fatalf("trackless buffering missing: %+v %v", first, err)
	}
	trackLoaded.Store(true)
	second, err := spotifyHealth(context.Background(), client, upstream.URL, dir)
	if err != nil || !second.Ready || second.BufferingWithoutTrack {
		t.Fatalf("loaded track still marked stalled: %+v %v", second, err)
	}
}
