package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/providers"
)

func TestBrowserOwnershipAndBoundedFrames(t *testing.T) {
	large := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("w", 32) {
			t.Error("private token missing")
		}
		if strings.HasSuffix(r.URL.Path, "/frame") {
			w.Write([]byte{0xff, 0xd8, 0xff})
			if large {
				w.Write(make([]byte, 1<<20))
			}
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	s := testServer(t, nil, "")
	s.SeedProviders(context.Background(), map[string]providers.Config{"rebrowser": {Enabled: true, URL: upstream.URL, Token: strings.Repeat("w", 32)}})
	one, two := pair(t, s, "browser-one"), pair(t, s, "browser-two")
	w := call(s, "POST", "/v1/browser", `{"url":"https://example.org"}`, "browser-one", one, "")
	var session struct{ SessionID string }
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &session) != nil {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	if call(s, "POST", "/v1/browser", `{"url":"https://example.org"}`, "browser-two", two, "").Code != 409 {
		t.Fatal("concurrency limit")
	}
	path := "/v1/browser/" + session.SessionID
	if call(s, "GET", path+"/frame", "", "browser-two", two, "").Code != 404 {
		t.Fatal("frame ownership")
	}
	if call(s, "DELETE", path, "", "browser-two", two, "").Code != 404 {
		t.Fatal("close ownership")
	}
	if w = call(s, "GET", path+"/frame", "", "browser-one", one, ""); w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal("frame delivery")
	}
	large = true
	if call(s, "GET", path+"/frame", "", "browser-one", one, "").Code != 502 {
		t.Fatal("unbounded bitmap")
	}
	if call(s, "POST", path+"/input", `{"action":"evaluate","text":"script"}`, "browser-one", one, "").Code != 400 {
		t.Fatal("arbitrary script accepted")
	}
	if call(s, "DELETE", path, "", "browser-one", one, "").Code != 200 {
		t.Fatal("close failed")
	}
	if call(s, "GET", path+"/frame", "", "browser-one", one, "").Code != 404 {
		t.Fatal("stopped session survived")
	}
}

func TestBrowserPointerValidationAndWorkerLoss(t *testing.T) {
	gone := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gone {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	s := testServer(t, nil, "")
	s.SeedProviders(context.Background(), map[string]providers.Config{"rebrowser": {Enabled: true, URL: upstream.URL, Token: strings.Repeat("w", 32)}})
	token := pair(t, s, "pointer-device")
	create := func() string {
		response := call(s, "POST", "/v1/browser", `{"url":"https://example.org"}`, "pointer-device", token, "")
		if response.Code != 201 {
			t.Fatal(response.Code, response.Body)
		}
		var body struct{ SessionID string }
		json.Unmarshal(response.Body.Bytes(), &body)
		return "/v1/browser/" + body.SessionID
	}
	path := create()
	for _, body := range []string{`{"action":"click"}`, `{"action":"click","x":960,"y":0}`, `{"action":"move","x":-1,"y":2}`} {
		if response := call(s, "POST", path+"/input", body, "pointer-device", token, ""); response.Code != 400 {
			t.Fatal(response.Code)
		}
	}
	if response := call(s, "POST", path+"/input", `{"action":"click","x":959,"y":539}`, "pointer-device", token, ""); response.Code != 200 {
		t.Fatal(response.Code)
	}
	gone = true
	if response := call(s, "GET", path+"/frame", "", "pointer-device", token, ""); response.Code != 410 {
		t.Fatal(response.Code)
	}
	gone = false
	create()
}
