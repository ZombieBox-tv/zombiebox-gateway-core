package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthContract(t *testing.T) {
	h := Handler()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/health", 200}, {"POST", "/health", 405}, {"GET", "/v1/home", 404},
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
