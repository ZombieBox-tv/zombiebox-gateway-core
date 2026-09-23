package youtubeaccount

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu      sync.Mutex
	records map[string]json.RawMessage
}

func (s *memoryStore) Get(_ context.Context, _, id string, out any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.records[id]
	if data == nil {
		return ErrNotConnected
	}
	return json.Unmarshal(data, out)
}
func (s *memoryStore) Put(_ context.Context, _, id string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(value)
	if err == nil {
		s.records[id] = data
	}
	return err
}
func (s *memoryStore) Delete(_ context.Context, _, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, id)
	return nil
}

func TestDeviceFlowRefreshAndReadOnlyLists(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	store := &memoryStore{records: map[string]json.RawMessage{}}
	polls, refreshes, revokes := 0, 0, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			if r.FormValue("scope") != scope || r.FormValue("client_id") != "client-id" {
				t.Error("wrong scope or client id")
			}
			io.WriteString(w, `{"device_code":"private-device-code","user_code":"ABCD-EFGH","verification_url":"https://www.google.com/device","expires_in":300,"interval":5}`)
		case "/token":
			switch r.FormValue("grant_type") {
			case "urn:ietf:params:oauth:grant-type:device_code":
				polls++
				if polls == 1 {
					w.WriteHeader(428)
					io.WriteString(w, `{"error":"authorization_pending"}`)
					return
				}
				io.WriteString(w, `{"access_token":"private-access","refresh_token":"private-refresh","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.readonly"}`)
			case "refresh_token":
				refreshes++
				if r.FormValue("refresh_token") != "private-refresh" {
					t.Error("wrong refresh token")
				}
				io.WriteString(w, `{"access_token":"renewed-access","expires_in":3600}`)
			default:
				t.Error("wrong grant type")
			}
		case "/v3/subscriptions":
			if r.Header.Get("Authorization") != "Bearer private-access" || r.URL.Query().Get("mine") != "true" {
				t.Error("missing scoped bearer")
			}
			io.WriteString(w, `{"items":[{"snippet":{"title":"Subscribed channel","resourceId":{"channelId":"UCabcdefghijklmnopqrstuv"}}}],"nextPageToken":"next"}`)
		case "/v3/playlists":
			if r.Header.Get("Authorization") != "Bearer renewed-access" {
				t.Error("refresh not applied")
			}
			io.WriteString(w, `{"items":[{"id":"PLabcdefghijkl","snippet":{"title":"My playlist"}}]}`)
		case "/revoke":
			revokes++
			if r.FormValue("token") != "private-refresh" {
				t.Error("wrong revocation token")
			}
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	endpoints := Endpoints{upstream.URL + "/device", upstream.URL + "/token", upstream.URL + "/revoke", upstream.URL + "/v3"}
	svc := New(store, upstream.Client(), "client-id", "", endpoints, func() time.Time { return clock })
	ctx := context.Background()
	start, err := svc.Start(ctx)
	if err != nil || start.Prompt == nil || start.Prompt.UserCode != "ABCD-EFGH" {
		t.Fatalf("start: %+v %v", start, err)
	}
	if encoded, _ := json.Marshal(start); strings.Contains(string(encoded), "private-device-code") {
		t.Fatal("device code exposed")
	}
	if _, err = svc.Poll(ctx); err != ErrTooSoon {
		t.Fatalf("poll before interval: %v", err)
	}
	clock = clock.Add(5 * time.Second)
	if pending, err := svc.Poll(ctx); err != nil || pending.State != "pending" {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	clock = clock.Add(5 * time.Second)
	if connected, err := svc.Poll(ctx); err != nil || !connected.Connected {
		t.Fatalf("connect: %+v %v", connected, err)
	}
	if page, err := svc.List(ctx, "subscriptions", ""); err != nil || page.NextPageToken != "next" || len(page.Items) != 1 || page.Items[0].BrowseID != "channel:UCabcdefghijklmnopqrstuv" {
		t.Fatalf("subscriptions: %+v %v", page, err)
	}
	clock = clock.Add(3700 * time.Second)
	restarted := New(store, upstream.Client(), "client-id", "", endpoints, func() time.Time { return clock })
	if page, err := restarted.List(ctx, "playlists", ""); err != nil || len(page.Items) != 1 || refreshes != 1 {
		t.Fatalf("refresh/list: %+v %v", page, err)
	}
	if err := restarted.Disconnect(ctx); err != nil || revokes != 1 || restarted.Status(ctx).Connected {
		t.Fatalf("disconnect: %v", err)
	}
}

func TestUnavailableAndInvalidPage(t *testing.T) {
	svc := New(&memoryStore{records: map[string]json.RawMessage{}}, &http.Client{}, "", "", GoogleEndpoints(), time.Now)
	if _, err := svc.Start(context.Background()); err != ErrUnavailable {
		t.Fatal(err)
	}
	if _, err := svc.List(context.Background(), "subscriptions", ""); err != ErrUnavailable {
		t.Fatal(err)
	}
	if _, err := url.Parse(GoogleEndpoints().Device); err != nil {
		t.Fatal(err)
	}
}

func TestListRefreshesRejectedAccessOnce(t *testing.T) {
	for _, retryAccepted := range []bool{true, false} {
		t.Run(map[bool]string{true: "recovered", false: "bounded"}[retryAccepted], func(t *testing.T) {
			store := &memoryStore{records: map[string]json.RawMessage{}}
			ctx := t.Context()
			if err := store.Put(ctx, "youtube_account", "household", tokenState{Access: "rejected", Refresh: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
				t.Fatal(err)
			}
			lists, refreshes := 0, 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token":
					refreshes++
					io.WriteString(w, `{"access_token":"renewed","expires_in":3600}`)
				case "/v3/playlists":
					lists++
					if r.Header.Get("Authorization") == "Bearer rejected" || !retryAccepted {
						w.WriteHeader(http.StatusUnauthorized)
						io.WriteString(w, `{"error":"invalid_token"}`)
						return
					}
					io.WriteString(w, `{"items":[{"id":"PLabcdefghijkl","snippet":{"title":"Recovered"}}]}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			}))
			defer upstream.Close()
			svc := New(store, upstream.Client(), "client-id", "", Endpoints{Token: upstream.URL + "/token", Data: upstream.URL + "/v3"}, time.Now)
			page, err := svc.List(ctx, "playlists", "")
			if lists != 2 || refreshes != 1 {
				t.Fatalf("unbounded or missing retry: lists=%d refreshes=%d", lists, refreshes)
			}
			if retryAccepted && (err != nil || len(page.Items) != 1) {
				t.Fatalf("recovery failed: %+v %v", page, err)
			}
			if !retryAccepted && err != ErrUpstream {
				t.Fatalf("repeated rejection must fail closed: %v", err)
			}
		})
	}
}
