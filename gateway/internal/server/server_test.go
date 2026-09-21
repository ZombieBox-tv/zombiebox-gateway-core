package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zombiebox.local/gateway/internal/store"
)

func TestHealthContract(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := newTestServer(db, Options{PairingCode: "123456"})
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/health", 200}, {"POST", "/health", 405}, {"GET", "/v1/home", 401},
	} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(tc.method, tc.path, nil))
		if r.Code != tc.status {
			t.Fatalf("%s %s: got %d", tc.method, tc.path, r.Code)
		}
		if tc.status == http.StatusOK {
			var health Health
			if err := json.Unmarshal(r.Body.Bytes(), &health); err != nil {
				t.Fatal(err)
			}
			if health.Status != "ok" || health.APIVersion != 1 {
				t.Fatalf("invalid health: %+v", health)
			}
		}
	}
}
