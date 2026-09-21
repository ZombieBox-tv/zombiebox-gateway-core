package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

func TestDiagnosticsExcludesSecretsAndOtherDevices(t *testing.T) {
	s := testServer(t, nil, "")
	token := pair(t, s, "diagnostic-owner")
	pair(t, s, "diagnostic-other")
	secret := "private-diagnostic-secret"
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"plex": {Enabled: true, URL: "https://example.invalid/" + secret, Token: secret}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"diagnostic-owner", "diagnostic-other"} {
		progress := domain.Progress{Item: domain.Item{ID: secret, Title: secret, Provider: id}, State: "FAILED", UpdatedAt: 123}
		if err := s.db.Put(context.Background(), "progress:"+id, secret, progress); err != nil {
			t.Fatal(err)
		}
	}
	response := call(s, "GET", "/v1/diagnostics", "", "diagnostic-owner", token, "")
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(response.Code, response.Body)
	}
	body := response.Body.String()
	for _, excluded := range []string{secret, token, "diagnostic-other", `"installationId":`, `"fingerprint":`, `"deviceToken":`} {
		if strings.Contains(body, excluded) {
			t.Fatalf("diagnostic export contains excluded field/value %q", excluded)
		}
	}
	var report struct {
		RecentFailures []struct{ Provider string }
		Probes         []domain.Probe
	}
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.RecentFailures) != 1 || report.RecentFailures[0].Provider != "diagnostic-owner" || report.Probes == nil {
		t.Fatal(body)
	}
	if response := call(s, "GET", "/v1/diagnostics", "", "diagnostic-owner", "wrong-token", ""); response.Code != 401 {
		t.Fatal(response.Code)
	}
}
