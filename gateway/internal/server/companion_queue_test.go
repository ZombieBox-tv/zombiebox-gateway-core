package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/companion"
	"zombiebox.local/gateway/internal/companionmedia"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/mediaqueue"
)

func TestURLQueueAdvancesOnlyOnOwnedCompletion(t *testing.T) {
	s := testServer(t, nil, "")
	disk, err := companionmedia.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	defer s.Close()
	s.deps.Uploads = disk
	s.deps.Media = &trackMedia{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials forwarded")
		}
		w.Write([]byte("\x00\x00\x00\x18ftypisomcontents"))
	}))
	defer upstream.Close()
	// Test-only injected transport; production rejects loopback URLs at dial time.
	s.deps.PublicMediaHTTP = upstream.Client()
	tv := pair(t, s, "queue-television")
	call(s, "PUT", "/v1/device/preferences", `{"mode":"TV","uiLanguage":"en","subtitleMode":"auto","allowCasting":true}`, "queue-television", tv, "")
	inv, _ := s.companions.Invite(t.Context(), "queue-television", "TV")
	req, token, _ := s.companions.Join(t.Context(), "127.0.0.1", companion.Join{InvitationID: inv.ID, Secret: inv.Secret, Name: "Phone"})
	body, _ := json.Marshal(map[string]any{"id": strings.Repeat("b", 32), "items": []mediaqueue.Item{{URL: upstream.URL, Title: "First"}, {URL: upstream.URL, Title: "Second"}}})
	w := call(s, "POST", "/v1/companion/media/queue", string(body), req.ID, token, "")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	wait := func(title string) domain.Plan {
		t.Helper()
		until := time.Now().Add(2 * time.Second)
		for time.Now().Before(until) {
			w = call(s, "GET", "/v1/cast/active", "", "queue-television", tv, "")
			var a struct{ Plan domain.Plan }
			json.Unmarshal(w.Body.Bytes(), &a)
			if a.Plan.Item.Title == title {
				return a.Plan
			}
			time.Sleep(time.Millisecond * 5)
		}
		t.Fatal("queue did not publish", title, w.Body)
		return domain.Plan{}
	}
	first := wait("First")
	w = call(s, "PUT", "/v1/playback/"+first.SessionID+"/progress", `{"state":"ENDED","positionMs":10,"durationMs":10}`, "queue-television", tv, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	second := wait("Second")
	w = call(s, "DELETE", "/v1/playback/"+first.SessionID, "", "queue-television", tv, "")
	if w.Code != 404 {
		t.Fatal("stale cleanup accepted", w.Code)
	}
	if s.mediaQueue.State(req.ID).Phase != "PLAYING" {
		t.Fatal("stale cleanup cancelled next item")
	}
	w = call(s, "PUT", "/v1/playback/"+first.SessionID+"/progress", `{"state":"ENDED"}`, "queue-television", tv, "")
	if w.Code != 404 {
		t.Fatal("stale completion accepted")
	}
	w = call(s, "DELETE", "/v1/cast/queue", "", "queue-television", tv, "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		w = call(s, "GET", second.URL, "", "", "", "")
		if w.Code == 401 {
			return
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatal("stopped queue stream retained")
}
