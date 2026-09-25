package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type spotifyHealthResult struct {
	Ready                 bool   `json:"ready"`
	AuthorizationRequired bool   `json:"authorizationRequired"`
	AuthMode              string `json:"authMode,omitempty"`
	BufferingWithoutTrack bool   `json:"bufferingWithoutTrack,omitempty"`
	Stopped               bool   `json:"stopped,omitempty"`
}

// Spotify's pairing code endpoint contains a secret. Health only inspects its
// status code and never copies its body or the code into a response or log.
func spotifyHealth(ctx context.Context, client *http.Client, upstream, stateDir string) (spotifyHealthResult, error) {
	health := spotifyHealthResult{AuthMode: spotifyAuthMode(stateDir)}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream+"/auth/code", nil)
	if err != nil {
		return health, err
	}
	res, err := client.Do(req)
	if err != nil {
		return health, err
	}
	_ = res.Body.Close()
	if res.StatusCode == http.StatusOK {
		health.AuthorizationRequired = true
		health.Stopped = true
		return health, nil
	}
	if res.StatusCode != http.StatusNoContent {
		return health, errors.New("Spotify daemon unavailable")
	}

	// A 204 means either an authorized session or Zeroconf waiting for the
	// phone. Confirm a real player session before claiming account readiness.
	statusCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err = http.NewRequestWithContext(statusCtx, http.MethodGet, upstream+"/status", nil)
	if err != nil {
		return health, err
	}
	res, err = client.Do(req)
	if err == nil {
		defer res.Body.Close()
		if res.StatusCode == http.StatusOK {
			// The pinned daemon's status includes a username only when a real
			// account session exists. Never expose that identity in health.
			var status struct {
				Username  string    `json:"username"`
				Stopped   bool      `json:"stopped"`
				Buffering bool      `json:"buffering"`
				Track     *struct{} `json:"track"`
			}
			if json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&status) == nil && strings.TrimSpace(status.Username) != "" {
				health.Ready = true
				health.Stopped = status.Stopped
				health.BufferingWithoutTrack = !status.Stopped && status.Buffering && status.Track == nil
				return health, nil
			}
		} else if res.StatusCode == http.StatusNoContent {
			health.Stopped = true
		}
	}
	if health.AuthMode == "zeroconf" {
		health.AuthorizationRequired = true
		health.Stopped = true
		return health, nil
	}
	return health, errors.New("Spotify session unavailable")
}

// Read only the packaged authentication mode, not account state or credentials.
// Unknown/custom YAML remains unknown instead of inventing a pairing method.
func spotifyAuthMode(stateDir string) string {
	f, err := os.Open(filepath.Join(stateDir, "config.yml"))
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, 64<<10))
	inCredentials := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "credentials:" {
			inCredentials = true
			continue
		}
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			inCredentials = false
		}
		if !inCredentials || !strings.HasPrefix(line, "  type:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "  type:"))
		value = strings.TrimSpace(strings.SplitN(value, "#", 2)[0])
		value = strings.Trim(value, `"'`)
		if value == "zeroconf" || value == "device_auth" {
			return value
		}
		return ""
	}
	return ""
}
