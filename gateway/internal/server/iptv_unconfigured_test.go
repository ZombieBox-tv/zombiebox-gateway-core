package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

func TestIPTVLifecycleStates(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "iptv-test-device")

	// 1. No source: Enabled=true without URL or PlaylistPath (reproducing sanitized SQLite candidate).
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"iptv": {Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	modulesRes := call(s, "GET", "/v1/modules", "", "iptv-test-device", token, "")
	if modulesRes.Code != 200 {
		t.Fatalf("GET /v1/modules returned %d", modulesRes.Code)
	}
	var modulesData struct {
		Modules []domain.Module `json:"modules"`
	}
	if err := json.Unmarshal(modulesRes.Body.Bytes(), &modulesData); err != nil {
		t.Fatal(err)
	}
	var iptvModule *domain.Module
	for i := range modulesData.Modules {
		if modulesData.Modules[i].ID == "iptv" {
			iptvModule = &modulesData.Modules[i]
			break
		}
	}
	if iptvModule == nil {
		t.Fatal("IPTV module not found in /v1/modules")
	}
	if iptvModule.State != "NEEDS_SETUP" {
		t.Fatalf("expected IPTV state NEEDS_SETUP when unconfigured, got: %s", iptvModule.State)
	}
	if !strings.Contains(iptvModule.Message, "playlist") && !strings.Contains(iptvModule.Message, "Services") {
		t.Fatalf("expected prompt message mentioning playlist/Services, got: %s", iptvModule.Message)
	}

	// Verify /v1/providers reflects enabled=true, configured=false
	provRes := call(s, "GET", "/v1/providers", "", "iptv-test-device", token, "")
	if provRes.Code != 200 {
		t.Fatalf("GET /v1/providers returned %d", provRes.Code)
	}
	if !strings.Contains(provRes.Body.String(), `"id":"iptv"`) {
		t.Fatal("IPTV not in providers response")
	}
	var provData struct {
		Providers []struct {
			ID         string `json:"id"`
			Enabled    bool   `json:"enabled"`
			Configured bool   `json:"configured"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(provRes.Body.Bytes(), &provData); err != nil {
		t.Fatal(err)
	}
	for _, p := range provData.Providers {
		if p.ID == "iptv" {
			if !p.Enabled || p.Configured {
				t.Fatalf("expected enabled=true, configured=false for unconfigured IPTV, got: %+v", p)
			}
		}
	}

	// 2. Source configured but unreachable: distinct DEGRADED / error state.
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"iptv": {Enabled: true, URL: "http://127.0.0.1:1/nonexistent.m3u"},
	}); err != nil {
		t.Fatal(err)
	}
	// Fetch catalog to execute adapter request and populate catalogCache
	_ = call(s, "GET", "/v1/catalog?provider=iptv", "", "iptv-test-device", token, "")

	modulesRes = call(s, "GET", "/v1/modules", "", "iptv-test-device", token, "")
	json.Unmarshal(modulesRes.Body.Bytes(), &modulesData)
	for _, m := range modulesData.Modules {
		if m.ID == "iptv" {
			if m.State != "DEGRADED" {
				t.Fatalf("expected IPTV state DEGRADED for unreachable source, got: %s", m.State)
			}
			if !strings.Contains(m.Message, "Service unavailable") {
				t.Fatalf("expected Service unavailable error message, got: %s", m.Message)
			}
		}
	}

	// 3. Disabled: state is DISABLED.
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"iptv": {Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	modulesRes = call(s, "GET", "/v1/modules", "", "iptv-test-device", token, "")
	json.Unmarshal(modulesRes.Body.Bytes(), &modulesData)
	for _, m := range modulesData.Modules {
		if m.ID == "iptv" {
			if m.State != "DISABLED" {
				t.Fatalf("expected IPTV state DISABLED when disabled, got: %s", m.State)
			}
		}
	}

	// 4. Valid configured source: state is HEALTHY and channels are returned.
	playlistServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:-1 tvg-id=\"news\",News 24\nhttp://example.test/stream.m3u8\n")
	}))
	defer playlistServer.Close()

	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"iptv": {Enabled: true, URL: playlistServer.URL},
	}); err != nil {
		t.Fatal(err)
	}
	// Fetch catalog to populate cache
	catRes := call(s, "GET", "/v1/catalog?provider=iptv", "", "iptv-test-device", token, "")
	if catRes.Code != 200 || !strings.Contains(catRes.Body.String(), "News 24") {
		t.Fatalf("expected News 24 channel in catalog, got: %s", catRes.Body.String())
	}

	modulesRes = call(s, "GET", "/v1/modules", "", "iptv-test-device", token, "")
	json.Unmarshal(modulesRes.Body.Bytes(), &modulesData)
	for _, m := range modulesData.Modules {
		if m.ID == "iptv" {
			if m.State != "HEALTHY" {
				t.Fatalf("expected IPTV state HEALTHY for valid source, got: %s", m.State)
			}
		}
	}
}
