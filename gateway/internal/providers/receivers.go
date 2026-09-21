package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"zombiebox.local/gateway/internal/domain"
)

type NowPlaying = domain.NowPlaying

type AuthorizationPrompt = domain.AuthorizationPrompt

func (a *Adapters) SpotifyAuthorization(ctx context.Context, c Config) (AuthorizationPrompt, error) {
	out := AuthorizationPrompt{State: "NONE"}
	headers, err := wrapperHeaders(c)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.URL, "/")+"/auth/code", nil)
	if err != nil {
		return out, err
	}
	req.Header = headers
	res, err := a.http.Do(req)
	if err != nil {
		return out, errors.New("authorization unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode == 204 {
		return out, nil
	}
	if res.StatusCode != 200 {
		return out, errors.New("authorization unavailable")
	}
	var raw struct {
		URL, Code string
		ExpiresAt time.Time `json:"expires_at"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&raw) != nil {
		return out, errors.New("invalid authorization")
	}
	u, err := url.Parse(raw.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Host != "spotify.com" && u.Host != "accounts.spotify.com" && u.Host != "www.spotify.com") || len(raw.Code) > 80 || raw.Code == "" || !raw.ExpiresAt.After(time.Now()) {
		return out, errors.New("invalid authorization")
	}
	return AuthorizationPrompt{State: "WAITING", URL: raw.URL, Code: raw.Code, ExpiresAt: raw.ExpiresAt.UTC().Format(time.RFC3339)}, nil
}

func wrapperHeaders(c Config) (http.Header, error) {
	if c.URL == "" || len(c.Token) < 32 {
		return nil, errors.New("wrapper configuration required")
	}
	return http.Header{"Authorization": {"Bearer " + c.Token}}, nil
}

func (a *Adapters) SpotifyStatus(ctx context.Context, c Config) (NowPlaying, error) {
	out := NowPlaying{Provider: "spotify", State: "STOPPED"}
	headers, err := wrapperHeaders(c)
	if err != nil {
		return out, err
	}
	body, err := a.request(ctx, strings.TrimRight(c.URL, "/")+"/status", headers)
	if err != nil {
		return out, err
	}
	var status struct {
		Stopped, Paused, Buffering bool
		Volume                     int
		VolumeSteps                int `json:"volume_steps"`
		Track                      *struct {
			Name     string
			Artists  []string `json:"artist_names"`
			Duration int64
			Position int64
		}
	}
	if json.Unmarshal(body, &status) != nil {
		return out, errors.New("invalid player status")
	}
	if status.VolumeSteps > 0 {
		out.Volume = max(0, min(100, status.Volume*100/status.VolumeSteps))
	}
	if !status.Stopped {
		out.State = "PLAYING"
		if status.Paused {
			out.State = "PAUSED"
		}
		if status.Buffering {
			out.State = "BUFFERING"
		}
	}
	if status.Track != nil {
		out.PositionMS = max(0, status.Track.Position)
		out.Item = &domain.Item{ID: "spotify-connect", Provider: "spotify", Kind: "audio", Title: truncate(status.Track.Name, 500), Subtitle: truncate(strings.Join(status.Track.Artists, ", "), 1000), DurationMS: max(0, status.Track.Duration), Playable: !status.Stopped}
	}
	return out, nil
}

func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) > limit {
		r = r[:limit]
	}
	return string(r)
}

func (a *Adapters) Spotify(ctx context.Context, c Config) ([]Source, error) {
	state, err := a.SpotifyStatus(ctx, c)
	if err != nil {
		return nil, err
	}
	item := domain.Item{ID: "spotify-connect", Provider: "spotify", Kind: "audio", Title: "Spotify Connect", Subtitle: "Select Zombie Box in Spotify, then listen here", Playable: true}
	if state.Item != nil {
		item = *state.Item
		item.Playable = true
	}
	headers, _ := wrapperHeaders(c)
	return []Source{{Item: item, URL: strings.TrimRight(c.URL, "/") + "/audio", Headers: headers, MIME: "audio/mpeg", Live: true}}, nil
}

type PlayerCommand = domain.PlayerCommand

// Translate a finite semantic command set; never expose arbitrary upstream paths,
// Spotify tokens, local audio devices or provider request bodies to the client.
func (a *Adapters) SpotifyCommand(ctx context.Context, c Config, command PlayerCommand) error {
	paths := map[string]string{"pause": "pause", "resume": "resume", "next": "next", "previous": "prev", "stop": "stop", "seek": "seek", "volume": "volume"}
	path, ok := paths[command.Action]
	if !ok || command.PositionMS < 0 || command.PositionMS > 604800000 || command.Volume < 0 || command.Volume > 100 {
		return errors.New("invalid command")
	}
	body := map[string]any{}
	if path == "seek" {
		body["position"] = command.PositionMS
	}
	if path == "volume" {
		body["volume"] = command.Volume
	}
	raw, _ := json.Marshal(body)
	headers, err := wrapperHeaders(c)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.URL, "/")+"/player/"+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header = headers
	req.Header.Set("Content-Type", "application/json")
	res, err := a.http.Do(req)
	if err != nil {
		return errors.New("player unavailable")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != 200 && res.StatusCode != 204 {
		return errors.New("player rejected command")
	}
	return nil
}

func (a *Adapters) AirPlay(ctx context.Context, c Config) ([]Source, error) {
	headers, err := wrapperHeaders(c)
	if err != nil {
		return nil, err
	}
	body, err := a.request(ctx, strings.TrimRight(c.URL, "/")+"/status", headers)
	if err != nil {
		return nil, err
	}
	var status struct {
		Active      bool `json:"active"`
		AudioActive bool `json:"audioActive"`
	}
	if json.Unmarshal(body, &status) != nil {
		return nil, errors.New("invalid receiver status")
	}
	item := domain.Item{ID: "airplay-live", Provider: "airplay", Kind: "video", Title: "AirPlay", Subtitle: "Start Screen Mirroring on your Apple device", Playable: status.Active}
	audio := domain.Item{ID: "airplay-audio", Provider: "airplay", Kind: "audio", Title: "AirPlay audio", Subtitle: "Select Zombie Box as the audio output on your Apple device", Playable: status.AudioActive}
	return []Source{{Item: item, URL: strings.TrimRight(c.URL, "/") + "/stream/index.m3u8", Headers: headers, MIME: "application/vnd.apple.mpegurl", Live: true}, {Item: audio, URL: strings.TrimRight(c.URL, "/") + "/stream/audio.m3u8", Headers: headers, MIME: "application/vnd.apple.mpegurl", Live: true}}, nil
}
