// Package youtubeaccount owns Google device authorization and read-only account data.
package youtubeaccount

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const scope = "https://www.googleapis.com/auth/youtube.readonly"

var channelID = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
var playlistID = regexp.MustCompile(`^[A-Za-z0-9_-]{10,100}$`)

var ErrUnavailable = errors.New("YouTube account authorization is not configured")
var ErrExpired = errors.New("authorization expired")
var ErrTooSoon = errors.New("authorization poll too soon")
var ErrUpstream = errors.New("YouTube account service unavailable")
var ErrNotConnected = errors.New("YouTube account is not connected")

type Transport interface {
	Do(*http.Request) (*http.Response, error)
}
type Store interface {
	Get(context.Context, string, string, any) error
	Put(context.Context, string, string, any) error
	Delete(context.Context, string, string) error
}
type Endpoints struct{ Device, Token, Revoke, Data string }

func GoogleEndpoints() Endpoints {
	return Endpoints{
		Device: "https://oauth2.googleapis.com/device/code",
		Token:  "https://oauth2.googleapis.com/token",
		Revoke: "https://oauth2.googleapis.com/revoke",
		Data:   "https://www.googleapis.com/youtube/v3",
	}
}

type Prompt struct {
	UserCode        string `json:"userCode"`
	VerificationURL string `json:"verificationUrl"`
	ExpiresAt       int64  `json:"expiresAt"`
	IntervalSeconds int    `json:"intervalSeconds"`
}
type Status struct {
	Configured bool    `json:"configured"`
	Connected  bool    `json:"connected"`
	State      string  `json:"state"`
	Prompt     *Prompt `json:"prompt,omitempty"`
}
type Entry struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	BrowseID string `json:"browseId,omitempty"`
}
type Page struct {
	Items         []Entry `json:"items"`
	NextPageToken string  `json:"nextPageToken,omitempty"`
}
type tokenState struct {
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	ExpiresAt int64  `json:"expiresAt"`
}
type pending struct {
	DeviceCode string
	Prompt     Prompt
	NextPoll   time.Time
}

type Service struct {
	mu                     sync.Mutex
	clientID, clientSecret string
	store                  Store
	http                   Transport
	endpoints              Endpoints
	now                    func() time.Time
	pending                *pending
}

func New(store Store, httpClient Transport, clientID, clientSecret string, endpoints Endpoints, now func() time.Time) *Service {
	return &Service{store: store, http: httpClient, clientID: strings.TrimSpace(clientID), clientSecret: clientSecret, endpoints: endpoints, now: now}
}

func (s *Service) Configured() bool { return s.clientID != "" }

func (s *Service) Status(ctx context.Context) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{Configured: s.Configured(), State: "disconnected"}
	if !status.Configured {
		status.State = "unconfigured"
		return status
	}
	var token tokenState
	if s.store.Get(ctx, "youtube_account", "household", &token) == nil && token.Refresh != "" {
		status.Connected, status.State = true, "connected"
		return status
	}
	if s.pending != nil && s.now().Unix() < s.pending.Prompt.ExpiresAt {
		prompt := s.pending.Prompt
		status.State, status.Prompt = "pending", &prompt
	}
	return status
}

func (s *Service) Start(ctx context.Context) (Status, error) {
	if !s.Configured() {
		return Status{}, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil && s.now().Unix() < s.pending.Prompt.ExpiresAt {
		prompt := s.pending.Prompt
		return Status{Configured: true, State: "pending", Prompt: &prompt}, nil
	}
	var response struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := s.form(ctx, s.endpoints.Device, url.Values{"client_id": {s.clientID}, "scope": {scope}}, &response); err != nil {
		return Status{}, err
	}
	if response.DeviceCode == "" || response.UserCode == "" || response.ExpiresIn < 30 || response.ExpiresIn > 3600 || response.Interval < 1 || response.Interval > 60 || len(response.UserCode) > 64 || len(response.DeviceCode) > 512 || len(response.VerificationURL) > 200 {
		return Status{}, ErrUpstream
	}
	u, err := url.Parse(response.VerificationURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return Status{}, ErrUpstream
	}
	prompt := Prompt{UserCode: response.UserCode, VerificationURL: response.VerificationURL, ExpiresAt: s.now().Add(time.Duration(response.ExpiresIn) * time.Second).Unix(), IntervalSeconds: response.Interval}
	s.pending = &pending{DeviceCode: response.DeviceCode, Prompt: prompt, NextPoll: s.now().Add(time.Duration(response.Interval) * time.Second)}
	return Status{Configured: true, State: "pending", Prompt: &prompt}, nil
}

func (s *Service) Poll(ctx context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil || s.now().Unix() >= s.pending.Prompt.ExpiresAt {
		s.pending = nil
		return Status{}, ErrExpired
	}
	if s.now().Before(s.pending.NextPoll) {
		return Status{}, ErrTooSoon
	}
	p := s.pending
	p.NextPoll = s.now().Add(time.Duration(p.Prompt.IntervalSeconds) * time.Second)
	values := url.Values{"client_id": {s.clientID}, "device_code": {p.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
	if s.clientSecret != "" {
		values.Set("client_secret", s.clientSecret)
	}
	var result struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int    `json:"expires_in"`
		Scope     string `json:"scope"`
		Error     string `json:"error"`
	}
	if err := s.form(ctx, s.endpoints.Token, values, &result); err != nil {
		return Status{}, err
	}
	switch result.Error {
	case "authorization_pending":
		return Status{Configured: true, State: "pending", Prompt: &p.Prompt}, nil
	case "slow_down":
		p.Prompt.IntervalSeconds += 5
		p.NextPoll = s.now().Add(time.Duration(p.Prompt.IntervalSeconds) * time.Second)
		return Status{Configured: true, State: "pending", Prompt: &p.Prompt}, nil
	case "access_denied", "expired_token":
		s.pending = nil
		return Status{Configured: true, State: "denied"}, nil
	case "":
	default:
		return Status{}, ErrUpstream
	}
	if result.Access == "" || result.Refresh == "" || result.ExpiresIn <= 0 || result.ExpiresIn > 86400 || !strings.Contains(" "+result.Scope+" ", " "+scope+" ") {
		return Status{}, ErrUpstream
	}
	state := tokenState{Access: result.Access, Refresh: result.Refresh, ExpiresAt: s.now().Add(time.Duration(result.ExpiresIn) * time.Second).Unix()}
	if err := s.store.Put(ctx, "youtube_account", "household", state); err != nil {
		return Status{}, err
	}
	s.pending = nil
	return Status{Configured: true, Connected: true, State: "connected"}, nil
}

func (s *Service) Disconnect(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var state tokenState
	if s.store.Get(ctx, "youtube_account", "household", &state) == nil && state.Refresh != "" {
		// The local grant is removed even if Google's revocation endpoint is unavailable.
		var discarded any
		_ = s.form(ctx, s.endpoints.Revoke, url.Values{"token": {state.Refresh}}, &discarded)
	}
	s.pending = nil
	return s.store.Delete(ctx, "youtube_account", "household")
}

func (s *Service) List(ctx context.Context, kind, pageToken string) (Page, error) {
	if !s.Configured() {
		return Page{}, ErrUnavailable
	}
	if kind != "subscriptions" && kind != "playlists" {
		return Page{}, errors.New("invalid list kind")
	}
	if len(pageToken) > 200 {
		return Page{}, errors.New("invalid page token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	access, err := s.access(ctx)
	if err != nil {
		return Page{}, err
	}
	values := url.Values{"part": {"snippet"}, "mine": {"true"}, "maxResults": {"40"}}
	if pageToken != "" {
		values.Set("pageToken", pageToken)
	}
	endpoint := s.endpoints.Data + "/" + kind + "?" + values.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Page{}, ErrUpstream
	}
	request.Header.Set("Authorization", "Bearer "+access)
	var response struct {
		NextPageToken string `json:"nextPageToken"`
		Items         []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				ResourceID  struct {
					ChannelID string `json:"channelId"`
				} `json:"resourceId"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err = s.request(request, &response, false); err != nil {
		return Page{}, err
	}
	page := Page{Items: []Entry{}}
	if len(response.NextPageToken) <= 200 {
		page.NextPageToken = response.NextPageToken
	}
	for _, item := range response.Items {
		id, entryKind := item.ID, "playlist"
		if kind == "subscriptions" {
			id, entryKind = item.Snippet.ResourceID.ChannelID, "channel"
		}
		if (kind == "subscriptions" && !channelID.MatchString(id)) || (kind == "playlists" && !playlistID.MatchString(id)) {
			continue
		}
		page.Items = append(page.Items, Entry{ID: "youtube-" + id, Kind: entryKind, Title: truncate(item.Snippet.Title, 500), Subtitle: truncate(item.Snippet.Description, 200), BrowseID: entryKind + ":" + id})
		if len(page.Items) == 40 {
			break
		}
	}
	return page, nil
}

func (s *Service) access(ctx context.Context) (string, error) {
	var state tokenState
	if s.store.Get(ctx, "youtube_account", "household", &state) != nil || state.Refresh == "" {
		return "", ErrNotConnected
	}
	if state.Access != "" && s.now().Unix()+60 < state.ExpiresAt {
		return state.Access, nil
	}
	values := url.Values{"client_id": {s.clientID}, "refresh_token": {state.Refresh}, "grant_type": {"refresh_token"}}
	if s.clientSecret != "" {
		values.Set("client_secret", s.clientSecret)
	}
	var response struct {
		Access    string `json:"access_token"`
		ExpiresIn int    `json:"expires_in"`
		Error     string `json:"error"`
	}
	if err := s.form(ctx, s.endpoints.Token, values, &response); err != nil {
		return "", err
	}
	if response.Error == "invalid_grant" {
		_ = s.store.Delete(ctx, "youtube_account", "household")
		return "", ErrNotConnected
	}
	if response.Error != "" || response.Access == "" || response.ExpiresIn <= 0 {
		return "", ErrUpstream
	}
	state.Access, state.ExpiresAt = response.Access, s.now().Add(time.Duration(response.ExpiresIn)*time.Second).Unix()
	if err := s.store.Put(ctx, "youtube_account", "household", state); err != nil {
		return "", err
	}
	return state.Access, nil
}

func (s *Service) form(ctx context.Context, endpoint string, values url.Values, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return ErrUpstream
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return s.request(request, out, true)
}

func (s *Service) request(request *http.Request, out any, allowClientError bool) error {
	response, err := s.http.Do(request)
	if err != nil {
		return ErrUpstream
	}
	defer response.Body.Close()
	if response.StatusCode >= 500 || response.StatusCode < 200 || (response.StatusCode >= 300 && response.StatusCode < 400) || (response.StatusCode >= 400 && !allowClientError) {
		return ErrUpstream
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 || json.Unmarshal(body, out) != nil {
		return ErrUpstream
	}
	return nil
}

func truncate(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return string(runes)
}
