package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"zombiebox.local/gateway/internal/providers"
)

func TestIntegrationAvailabilityAndControlAuthorization(t *testing.T) {
	commands := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			commands++
			return
		}
		io.WriteString(w, `{"stopped":true,"volume_steps":100}`)
	}))
	defer upstream.Close()
	s := testServer(t, nil, "")
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"spotify": {Enabled: true, URL: upstream.URL, Token: strings.Repeat("s", 32)}, "airplay": {Enabled: true, URL: "http://127.0.0.1:1", Token: strings.Repeat("a", 32)}}); err != nil {
		t.Fatal(err)
	}
	token := pair(t, s, "service-client")
	if call(s, "GET", "/v1/integrations", "", "service-client", "bad", "").Code != 401 {
		t.Fatal("unauthenticated inventory")
	}
	w := call(s, "GET", "/v1/integrations", "", "service-client", token, "")
	var response struct{ Integrations []integrationStatus }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatalf("inventory: %s", w.Body)
	}
	states := map[string]string{}
	for _, item := range response.Integrations {
		states[item.ID] = item.State
	}
	if len(states) != 13 || states["spotify"] != "READY" || states["airplay"] != "UNAVAILABLE" || states["rebrowser"] != "DISABLED" || states["local"] != "READY" {
		t.Fatal(states)
	}
	if strings.Contains(w.Body.String(), upstream.URL) || strings.Contains(w.Body.String(), strings.Repeat("s", 32)) {
		t.Fatal("configuration leaked")
	}
	if call(s, "POST", "/v1/player/spotify", `{"action":"pause"}`, "service-client", token, "").Code != 403 || commands != 0 {
		t.Fatal("shared player controlled without admin")
	}
	if call(s, "POST", "/v1/player/spotify", `{"action":"pause"}`, "service-client", token, "123456").Code != 200 || commands != 1 {
		t.Fatal("command not forwarded")
	}
	if call(s, "POST", "/v1/player/spotify", `{"action":"output"}`, "service-client", token, "123456").Code != 400 || commands != 1 {
		t.Fatal("arbitrary command forwarded")
	}
	if call(s, "GET", "/v1/home", "", "service-client", token, "").Code != 200 {
		t.Fatal("optional failure blocked Home")
	}
}
