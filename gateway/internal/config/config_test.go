package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRejectsTyposAndMissingEnabledSecrets(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"iptv":{"enabled":true,"url":"env:IPTV_URL"}}`, true},
		{`{"plex":{"enabled":true,"token":"env:MISSING"}}`, false},
		{`{"plex":{"enabled":false,"token":"env:MISSING"}}`, true},
		{`{"iptv":{"enable":true}}`, false}, {`{"typo":{}}`, false}, {`{} {}`, false}, {`null`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.json")
			os.WriteFile(p, []byte(tc.body), 0600)
			_, err := Load(p, func(key string) (string, bool) { return "https://example.test/list?private-key", key == "IPTV_URL" })
			if (err == nil) != tc.valid {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
