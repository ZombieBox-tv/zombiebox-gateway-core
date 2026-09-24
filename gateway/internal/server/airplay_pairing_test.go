package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/providers"
)

func TestAirPlayPairingRequiresPairedDeviceAndPrivateWorker(t *testing.T) {
	const pin = "0427"
	privateToken := strings.Repeat("a", 32)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pairing" || r.Header.Get("Authorization") != "Bearer "+privateToken {
			t.Error("unexpected private worker request")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"pin":"`+pin+`"}`)
	}))
	defer worker.Close()
	s := testServer(t, nil, t.TempDir())
	if w := call(s, "GET", "/v1/airplay/pairing", "", "airplay-tv", "", ""); w.Code != 401 || strings.Contains(w.Body.String(), pin) {
		t.Fatal("anonymous PIN access", w.Code)
	}
	token := pair(t, s, "airplay-tv")
	if w := call(s, "GET", "/v1/airplay/pairing", "", "airplay-tv", token, ""); w.Code != 409 {
		t.Fatal("disabled worker PIN access", w.Code)
	}
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"airplay": {Enabled: true, URL: worker.URL, Token: privateToken}}); err != nil {
		t.Fatal(err)
	}
	w := call(s, "GET", "/v1/airplay/pairing", "", "airplay-tv", token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), pin) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("paired TV did not receive PIN privately", w.Code)
	}
}
