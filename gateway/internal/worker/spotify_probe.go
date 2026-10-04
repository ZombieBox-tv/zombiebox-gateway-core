package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
)

// SpotifyWorkerProbe contains no account metadata, provider identifiers,
// pairing values, audio payloads, paths, endpoints or credential fields.
type SpotifyWorkerProbe struct {
	Backend               string `json:"backend"`
	AccountReady          bool   `json:"accountReady"`
	AudioReady            bool   `json:"audioReady"`
	AuthorizationRequired bool   `json:"authorizationRequired"`
}

func ProbeSpotifyWorker(ctx context.Context, c Config, client *http.Client) (SpotifyWorkerProbe, error) {
	var result SpotifyWorkerProbe
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil || c.Mode != "spotify" || c.Validate() != nil {
		return result, errors.New("invalid Spotify probe configuration")
	}
	if host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host != "127.0.0.1" && host != "localhost" && host != "host.docker.internal" {
		return result, errors.New("Spotify probe requires a private local endpoint")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/health", nil)
	if err != nil {
		return result, errors.New("Spotify probe unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := client.Do(request)
	if err != nil {
		return result, errors.New("Spotify probe unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, errors.New("Spotify worker unavailable")
	}
	var health struct {
		Backend               string `json:"backend"`
		Ready                 bool   `json:"ready"`
		AccountReady          bool   `json:"accountReady"`
		AudioReady            bool   `json:"audioReady"`
		AuthorizationRequired bool   `json:"authorizationRequired"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&health) != nil || (health.Backend != "soloist" && health.Backend != "go-librespot") {
		return result, errors.New("invalid Spotify health response")
	}
	return SpotifyWorkerProbe{Backend: health.Backend, AccountReady: health.AccountReady || (health.Backend == "go-librespot" && health.Ready), AudioReady: health.AudioReady, AuthorizationRequired: health.AuthorizationRequired}, nil
}
