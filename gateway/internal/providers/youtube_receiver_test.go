package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"zombiebox.local/gateway/internal/domain"
)

func TestReceiverAdapterUsesPrivateAuthenticationAndSemanticCommands(t *testing.T) {
	token := strings.Repeat("private", 6)
	id := strings.Repeat("a", 32)
	seen := []string{}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing private authentication")
			w.WriteHeader(401)
			return
		}
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Method == "POST" && r.URL.Path == "/receiver" {
			var request map[string]string
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["receiverId"] != id {
				t.Error("invalid open body")
			}
		}
		json.NewEncoder(w).Encode(domain.YouTubeReceiverState{ReceiverID: id, State: "READY", TVCode: "123456"})
	}))
	defer worker.Close()
	adapter := New(worker.Client(), worker.Client())
	config := Config{Enabled: true, URL: worker.URL, Token: token}
	if state, err := adapter.OpenReceiver(t.Context(), config, id); err != nil || state.TVCode != "123456" {
		t.Fatal(state, err)
	}
	if _, err := adapter.PollReceiver(t.Context(), config, id); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AcknowledgeReceiver(t.Context(), config, id, domain.ReceiverAcknowledgement{State: "STOPPED"}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.CloseReceiver(t.Context(), config, id); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "POST /receiver,GET /receiver/"+id+",POST /receiver/"+id+"/state,DELETE /receiver/"+id {
		t.Fatal(seen)
	}
}
func TestReceiverAdapterPropagatesCancellation(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer worker.Close()
	adapter := New(worker.Client(), worker.Client())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := adapter.OpenReceiver(ctx, Config{Enabled: true, URL: worker.URL, Token: strings.Repeat("x", 32)}, strings.Repeat("a", 32)); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}
