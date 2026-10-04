package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

const soloistBackendTestToken = "fake-soloist-backend-token-0123456789"

type soloistBackendTestPeer func(connection int, socket *websocket.Conn)

func newSoloistBackendTestHarness(t *testing.T, peer soloistBackendTestPeer) (*SoloistBackend, string, context.CancelFunc) {
	t.Helper()
	var connection atomic.Int32
	upstream := httptest.NewServer(websocket.Handler(func(socket *websocket.Conn) {
		defer socket.Close()
		peer(int(connection.Add(1)), socket)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	backend, err := NewSoloistBackend(ctx, localWSEndpoint(upstream.URL), soloistBackendTestToken)
	if err != nil {
		cancel()
		upstream.Close()
		t.Fatalf("construct local backend: %v", err)
	}
	httpServer := httptest.NewServer(backend.Handler())
	t.Cleanup(func() {
		cancel()
		_ = backend.Close()
		select {
		case <-backend.Done():
		case <-time.After(2 * time.Second):
			t.Errorf("backend reconnect loop did not stop")
		}
		httpServer.Close()
		upstream.Close()
	})
	return backend, httpServer.URL, cancel
}

func soloistTestAuthAndQuery(connection int, socket *websocket.Conn, status string) {
	_ = connection
	if sendSoloistFixture(socket, `{"type":"auth_state","logged_in":true,"is_active":true,"device_name":"Private TV Device"}`) != nil {
		return
	}
	for {
		frame, err := receiveSoloistFixture(socket)
		if err != nil {
			return
		}
		var header struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(frame), &header) != nil {
			return
		}
		if header.Type == "command" && header.Command == "get_state" {
			_ = sendSoloistFixture(socket, soloistPlaybackFixture(status, true, 65))
			continue
		}
		if header.Type == "command" {
			_ = sendSoloistFixture(socket, fmt.Sprintf(`{"type":"command_result","command":%q}`, header.Command))
		}
	}
}

func soloistPlaybackFixture(status string, withTrack bool, volume int) string {
	item := `"item":null`
	if withTrack {
		item = `"item":{"uri":"spotify:track:private-track-id","entity_type":"track","decorations":{"identity":{"name":"Fixture Song"},"visual_identity":{"cover":[{"url":"https://i.scdn.co/image/fake-private-artwork","size":"large"}]},"creators":[{"entity":{"uri":"spotify:artist:private-artist-one","entity_type":"artist","decorations":{"identity":{"name":"Artist One"}}}},{"entity":{"uri":"spotify:artist:private-artist-two","entity_type":"artist","decorations":{"identity":{"name":"Artist Two"}}}}],"playback":{"duration_ms":30000}}}`
	}
	return fmt.Sprintf(`{"type":"playback_state","status":%q,%s,"position":{"position_ms":2500,"timestamp_ms":%d,"speed":0.0},"volume":%d,"is_active":true,"context":{"uri":"spotify:playlist:private-context"}}`, status, item, time.Now().UnixMilli(), volume)
}

func sendSoloistFixture(socket *websocket.Conn, frame string) error {
	return websocket.Message.Send(socket, frame)
}

func receiveSoloistFixture(socket *websocket.Conn) (string, error) {
	var frame string
	err := websocket.Message.Receive(socket, &frame)
	return frame, err
}

func requestSoloistBackend(base, method, path, token, body string) (int, string, error) {
	var payload io.Reader
	if body != "" {
		payload = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, base+path, payload)
	if err != nil {
		return 0, "", err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	return response.StatusCode, string(data), err
}

func doSoloistBackendRequest(t *testing.T, base, method, path, token, body string) (int, string) {
	t.Helper()
	status, response, err := requestSoloistBackend(base, method, path, token, body)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	return status, response
}

func waitForSoloistBackend(t *testing.T, backend *SoloistBackend, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-backend.Done():
			t.Fatal("Soloist backend stopped before reaching expected state")
		case <-timer.C:
			t.Fatal("timed out waiting for Soloist backend state")
		case <-ticker.C:
		}
	}
}

func TestSoloistBackendAuthStatusAndUnsupportedAudioAreSafe(t *testing.T) {
	backend, base, _ := newSoloistBackendTestHarness(t, func(id int, socket *websocket.Conn) {
		soloistTestAuthAndQuery(id, socket, "playing")
	})
	if _, err := NewSoloistBackend(context.Background(), "ws://127.0.0.1:9090", "too-short"); !errors.Is(err, ErrSoloistBackendConfig) {
		t.Fatalf("short bearer token was accepted: %v", err)
	}
	waitForSoloistBackend(t, backend, func() bool {
		snapshot := backend.state.Snapshot()
		return snapshot.Authenticated && snapshot.Track != nil && snapshot.VolumeKnown
	})

	for _, test := range []struct {
		path  string
		token string
	}{
		{path: "/status"},
		{path: "/health", token: "wrong-bearer-value"},
	} {
		status, _ := doSoloistBackendRequest(t, base, http.MethodGet, test.path, test.token, "")
		if status != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s returned %d", test.path, status)
		}
	}

	status, body := doSoloistBackendRequest(t, base, http.MethodGet, "/health", soloistBackendTestToken, "")
	if status != http.StatusOK {
		t.Fatalf("health returned %d: %s", status, body)
	}
	for _, secret := range []string{"Private TV Device", "private-track-id", "private-artist-one", "private-context", "fake-private-artwork"} {
		if strings.Contains(body, secret) {
			t.Fatalf("health diagnostics leaked upstream data %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, `"accountReady":true`) || !strings.Contains(body, `"ready":false`) || !strings.Contains(body, `"audio_available":false`) {
		t.Fatalf("health omitted fixed readiness/audio diagnostics: %s", body)
	}

	status, body = doSoloistBackendRequest(t, base, http.MethodGet, "/status", soloistBackendTestToken, "")
	if status != http.StatusOK {
		t.Fatalf("status returned %d: %s", status, body)
	}
	for _, expected := range []string{`"stopped":false`, `"paused":false`, `"buffering":false`, `"audio_available":false`, `"name":"Fixture Song"`, `"artist_names":["Artist One","Artist Two"]`, `"duration":30000`, `"position":`, `"volume":65`, `"volume_steps":100`, `https://i.scdn.co/image/fake-private-artwork`} {
		if !strings.Contains(body, expected) {
			t.Errorf("safe status omitted %q: %s", expected, body)
		}
	}
	for _, secret := range []string{"spotify:", "Private TV Device", "username", "private-track-id", "private-artist-one", "private-context"} {
		if strings.Contains(body, secret) {
			t.Errorf("status leaked an upstream identity %q: %s", secret, body)
		}
	}

	for path, reason := range map[string]string{"/audio": "audio unavailable", "/auth/code": "authorization unavailable"} {
		status, body = doSoloistBackendRequest(t, base, http.MethodGet, path, soloistBackendTestToken, "")
		if status != http.StatusServiceUnavailable || !strings.Contains(body, reason) {
			t.Errorf("%s did not return its fixed unsupported reason: %d %s", path, status, body)
		}
		if strings.Contains(body, "Private") || strings.Contains(body, "spotify:") {
			t.Errorf("%s leaked upstream data: %s", path, body)
		}
	}
}

func TestSoloistBackendOmitsUnknownVolumeFields(t *testing.T) {
	backend, base, _ := newSoloistBackendTestHarness(t, func(_ int, socket *websocket.Conn) {
		if sendSoloistFixture(socket, `{"type":"auth_state","logged_in":true,"is_active":false}`) != nil {
			return
		}
		for {
			frame, err := receiveSoloistFixture(socket)
			if err != nil {
				return
			}
			var query struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(frame), &query) != nil {
				return
			}
			if query.Type == "command" && query.Command == "get_state" {
				_ = sendSoloistFixture(socket, `{"type":"playback_state","status":"paused","item":null}`)
			}
		}
	})
	waitForSoloistBackend(t, backend, func() bool { return backend.state.Snapshot().Authenticated })
	_, body := doSoloistBackendRequest(t, base, http.MethodGet, "/status", soloistBackendTestToken, "")
	if strings.Contains(body, `"volume"`) || strings.Contains(body, `"volume_steps"`) {
		t.Fatalf("unobserved volume was presented as known: %s", body)
	}
}

func TestSoloistBackendInactiveTransferReportsStopped(t *testing.T) {
	backend, base, _ := newSoloistBackendTestHarness(t, func(_ int, socket *websocket.Conn) {
		if sendSoloistFixture(socket, `{"type":"auth_state","logged_in":true,"is_active":true}`) != nil {
			return
		}
		for {
			frame, err := receiveSoloistFixture(socket)
			if err != nil {
				return
			}
			var request struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(frame), &request) != nil {
				return
			}
			if request.Type == "command" && request.Command == "get_state" {
				if sendSoloistFixture(socket, soloistPlaybackFixture("playing", true, 65)) != nil {
					return
				}
				_ = sendSoloistFixture(socket, `{"type":"device_changed","is_active":false,"device_name":"Private Transfer Device"}`)
			}
		}
	})
	waitForSoloistBackend(t, backend, func() bool {
		snapshot := backend.state.Snapshot()
		return snapshot.Track != nil && snapshot.ActivityKnown && !snapshot.Active
	})

	status, body := doSoloistBackendRequest(t, base, http.MethodGet, "/status", soloistBackendTestToken, "")
	if status != http.StatusOK {
		t.Fatalf("status returned %d: %s", status, body)
	}
	for _, expected := range []string{`"stopped":true`, `"paused":false`, `"buffering":false`, `"name":"Fixture Song"`} {
		if !strings.Contains(body, expected) {
			t.Errorf("inactive transfer status omitted %q: %s", expected, body)
		}
	}
	if strings.Contains(body, "Private Transfer Device") {
		t.Fatalf("device identity leaked after transfer: %s", body)
	}
}

func TestSoloistBackendRetiresSocketAtAuthenticationBoundaryAndRefreshesState(t *testing.T) {
	logout := make(chan struct{})
	queries := make(chan int, 4)
	backend, _, _ := newSoloistBackendTestHarness(t, func(id int, socket *websocket.Conn) {
		if sendSoloistFixture(socket, `{"type":"auth_state","logged_in":true,"is_active":true}`) != nil {
			return
		}
		for {
			frame, err := receiveSoloistFixture(socket)
			if err != nil {
				return
			}
			var request struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(frame), &request) != nil {
				return
			}
			if request.Type != "command" || request.Command != "get_state" {
				return
			}
			queries <- id
			fixture := soloistPlaybackFixture("playing", true, 65)
			if id > 1 {
				fixture = strings.ReplaceAll(fixture, "Fixture Song", "New Account Track")
				fixture = strings.ReplaceAll(fixture, "private-track-id", "private-new-track-id")
			}
			if sendSoloistFixture(socket, fixture) != nil {
				return
			}
			if id == 1 {
				<-logout
				_ = sendSoloistFixture(socket, `{"type":"auth_state","logged_in":false,"is_active":false}`)
				return
			}
		}
	})
	waitForSoloistBackend(t, backend, func() bool { return backend.state.Snapshot().Track != nil })
	select {
	case id := <-queries:
		if id != 1 {
			t.Fatalf("initial get_state query used connection %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("initial authenticated connection did not receive get_state")
	}
	close(logout)
	waitForSoloistBackend(t, backend, func() bool {
		current := backend.currentConnection()
		return current != nil && current.session != (SoloistSession{}) && backend.state.Snapshot().Track != nil && backend.state.Snapshot().Track.Title == "New Account Track"
	})
	select {
	case id := <-queries:
		if id < 2 {
			t.Fatalf("authentication boundary reused connection %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnected authenticated socket did not receive get_state")
	}
}

func TestSoloistBackendQueryErrorCannotAcknowledgeControl(t *testing.T) {
	commands := make(chan string, 4)
	querySeen := make(chan struct{}, 4)
	_, base, _ := newSoloistBackendTestHarness(t, func(_ int, socket *websocket.Conn) {
		if sendSoloistFixture(socket, `{"type":"auth_state","logged_in":true,"is_active":true}`) != nil {
			return
		}
		for {
			frame, err := receiveSoloistFixture(socket)
			if err != nil {
				return
			}
			var request struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(frame), &request) != nil || request.Type != "command" {
				return
			}
			if request.Command == "get_state" {
				querySeen <- struct{}{}
				_ = sendSoloistFixture(socket, `{"type":"error","message":"private query diagnostic"}`)
				continue
			}
			commands <- request.Command
		}
	})
	select {
	case <-querySeen:
	case <-time.After(time.Second):
		t.Fatal("backend did not send fixed get_state query")
	}
	status, body := doSoloistBackendRequest(t, base, http.MethodPost, "/player/pause", soloistBackendTestToken, "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("control was treated as accepted before query state: %d %s", status, body)
	}
	if strings.Contains(body, "private query diagnostic") {
		t.Fatalf("raw query error leaked through HTTP: %s", body)
	}
	select {
	case command := <-commands:
		t.Fatalf("control was sent before query completed: %q", command)
	default:
	}
}

func TestSoloistBackendControlsRequireStrictBodiesAndMatchingAcceptedCommands(t *testing.T) {
	commands := make(chan struct {
		connection int
		command    string
	}, 16)
	backend, base, _ := newSoloistBackendTestHarness(t, func(id int, socket *websocket.Conn) {
		if sendSoloistFixture(socket, `{"type":"auth_state","logged_in":true,"is_active":true}`) != nil {
			return
		}
		for {
			frame, err := receiveSoloistFixture(socket)
			if err != nil {
				return
			}
			var message struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(frame), &message) != nil {
				return
			}
			if message.Type == "command" && message.Command == "get_state" {
				_ = sendSoloistFixture(socket, soloistPlaybackFixture("paused", true, 20))
				continue
			}
			if message.Type == "command" {
				commands <- struct {
					connection int
					command    string
				}{connection: id, command: message.Command}
				_ = sendSoloistFixture(socket, fmt.Sprintf(`{"type":"command_result","command":%q}`, message.Command))
			}
		}
	})
	waitForSoloistBackend(t, backend, func() bool { return backend.state.Snapshot().Track != nil })

	valid := []struct {
		action  string
		body    string
		command string
	}{
		{action: "pause", body: "", command: "pause"},
		{action: "resume", body: `{}`, command: "play"},
		{action: "next", body: `{}`, command: "skip_next"},
		{action: "prev", body: "", command: "skip_prev"},
		{action: "seek", body: `{"position":1234}`, command: "seek"},
		{action: "volume", body: `{"volume":55}`, command: "set_volume"},
		{action: "stop", body: `{}`, command: "pause"},
	}
	for _, test := range valid {
		status, body := doSoloistBackendRequest(t, base, http.MethodPost, "/player/"+test.action, soloistBackendTestToken, test.body)
		if status != http.StatusNoContent {
			t.Fatalf("valid %s returned %d: %s", test.action, status, body)
		}
		select {
		case got := <-commands:
			if got.connection == 0 || got.command != test.command {
				t.Fatalf("%s mapped to unexpected command on connection %d: %q", test.action, got.connection, got.command)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not reach the local controller", test.action)
		}
	}

	invalid := []struct {
		action string
		body   string
	}{
		{action: "pause", body: `{"extra":1}`},
		{action: "pause", body: `{} {}`},
		{action: "pause", body: `{"extra":1,"extra":2}`},
		{action: "seek", body: `{"position":-1}`},
		{action: "seek", body: `{"position":1.5}`},
		{action: "seek", body: `{"position":30001}`},
		{action: "seek", body: `{"position":10,"extra":1}`},
		{action: "volume", body: `{"volume":101}`},
		{action: "volume", body: `{"volume":1.5}`},
		{action: "volume", body: `{"volume":50,"extra":1}`},
		{action: "volume", body: strings.Repeat("x", maxSoloistBackendRequestBytes+1)},
	}
	for _, test := range invalid {
		status, body := doSoloistBackendRequest(t, base, http.MethodPost, "/player/"+test.action, soloistBackendTestToken, test.body)
		if status != http.StatusBadRequest {
			t.Errorf("invalid %s payload returned %d: %s", test.action, status, body)
		}
	}
	status, _ := doSoloistBackendRequest(t, base, http.MethodPost, "/player/passthrough", soloistBackendTestToken, `{}`)
	if status != http.StatusNotFound {
		t.Errorf("unallowlisted control returned %d", status)
	}
	select {
	case command := <-commands:
		t.Fatalf("invalid payload reached the upstream: %q", command.command)
	case <-time.After(40 * time.Millisecond):
	}
}

func TestSoloistBackendIgnoresUnrelatedCommandResult(t *testing.T) {
	unrelated := make(chan struct{})
	allowMatching := make(chan struct{})
	var release sync.Once
	backend, base, _ := newSoloistBackendTestHarness(t, func(_ int, socket *websocket.Conn) {
		if sendSoloistFixture(socket, `{"type":"auth_state","logged_in":true,"is_active":true}`) != nil {
			return
		}
		for {
			frame, err := receiveSoloistFixture(socket)
			if err != nil {
				return
			}
			var message struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(frame), &message) != nil {
				return
			}
			if message.Type == "command" && message.Command == "get_state" {
				_ = sendSoloistFixture(socket, soloistPlaybackFixture("paused", false, 20))
				continue
			}
			if message.Type == "command" {
				_ = sendSoloistFixture(socket, `{"type":"command_result","command":"skip_next"}`)
				close(unrelated)
				<-allowMatching
				_ = sendSoloistFixture(socket, fmt.Sprintf(`{"type":"command_result","command":%q}`, message.Command))
			}
		}
	})
	t.Cleanup(func() { release.Do(func() { close(allowMatching) }) })
	waitForSoloistBackend(t, backend, func() bool { return backend.state.Snapshot().Authenticated })
	type response struct {
		status int
		body   string
		err    error
	}
	result := make(chan response, 1)
	go func() {
		status, body, err := requestSoloistBackend(base, http.MethodPost, "/player/pause", soloistBackendTestToken, "")
		result <- response{status: status, body: body, err: err}
	}()
	select {
	case <-unrelated:
	case <-time.After(2 * time.Second):
		t.Fatal("fake Soloist did not emit unrelated result")
	}
	select {
	case response := <-result:
		t.Fatalf("unrelated command_result accepted pause: %+v", response)
	case <-time.After(40 * time.Millisecond):
	}
	release.Do(func() { close(allowMatching) })
	select {
	case response := <-result:
		if response.err != nil || response.status != http.StatusNoContent {
			t.Fatalf("matching result did not accept pause: %+v", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("matching acknowledgment did not finish the request")
	}
}

func TestSoloistBackendErrorEventDoesNotLeakRawMessage(t *testing.T) {
	backend, base, _ := newSoloistBackendTestHarness(t, func(_ int, socket *websocket.Conn) {
		if sendSoloistFixture(socket, `{"type":"auth_state","logged_in":true,"is_active":true}`) != nil {
			return
		}
		for {
			frame, err := receiveSoloistFixture(socket)
			if err != nil {
				return
			}
			var message struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(frame), &message) != nil {
				return
			}
			if message.Type == "command" && message.Command == "get_state" {
				_ = sendSoloistFixture(socket, soloistPlaybackFixture("paused", false, 10))
			} else {
				_ = sendSoloistFixture(socket, `{"type":"error","message":"secret-account diagnostic text"}`)
			}
		}
	})
	waitForSoloistBackend(t, backend, func() bool { return backend.state.Snapshot().Authenticated })
	status, body := doSoloistBackendRequest(t, base, http.MethodPost, "/player/pause", soloistBackendTestToken, "")
	if status != http.StatusBadGateway || !strings.Contains(body, "command rejected") {
		t.Fatalf("error event did not produce a fixed rejection: %d %s", status, body)
	}
	if strings.Contains(body, "secret-account diagnostic text") {
		t.Fatalf("raw Soloist error leaked through HTTP: %s", body)
	}
}

func TestSoloistBackendTimeoutRetiresConnectionBeforeLateMatchingAck(t *testing.T) {
	var lateAttempted atomic.Bool
	acceptedConnection := make(chan int, 1)
	backend, base, _ := newSoloistBackendTestHarness(t, func(id int, socket *websocket.Conn) {
		if sendSoloistFixture(socket, `{"type":"auth_state","logged_in":true,"is_active":true}`) != nil {
			return
		}
		for {
			frame, err := receiveSoloistFixture(socket)
			if err != nil {
				return
			}
			var message struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal([]byte(frame), &message) != nil {
				return
			}
			if message.Type == "command" && message.Command == "get_state" {
				_ = sendSoloistFixture(socket, soloistPlaybackFixture("paused", false, 10))
				continue
			}
			if message.Type == "command" && message.Command == "pause" {
				if id == 1 {
					time.Sleep(180 * time.Millisecond)
					_ = sendSoloistFixture(socket, `{"type":"command_result","command":"pause"}`)
					lateAttempted.Store(true)
					return
				}
				acceptedConnection <- id
				_ = sendSoloistFixture(socket, `{"type":"command_result","command":"pause"}`)
			}
		}
	})
	backend.ackTimeout = 80 * time.Millisecond
	waitForSoloistBackend(t, backend, func() bool { return backend.state.Snapshot().Authenticated })
	status, body := doSoloistBackendRequest(t, base, http.MethodPost, "/player/pause", soloistBackendTestToken, "")
	if status != http.StatusGatewayTimeout || !strings.Contains(body, "acknowledgment timeout") {
		t.Fatalf("missing acknowledgment did not time out: %d %s", status, body)
	}
	waitForSoloistBackend(t, backend, func() bool {
		current := backend.currentConnection()
		return current != nil && backend.state.Snapshot().Authenticated
	})
	status, body = doSoloistBackendRequest(t, base, http.MethodPost, "/player/pause", soloistBackendTestToken, "")
	if status != http.StatusNoContent {
		t.Fatalf("late first acknowledgment affected reconnect command: %d %s", status, body)
	}
	select {
	case id := <-acceptedConnection:
		if id < 2 {
			t.Fatalf("second command reused retired connection %d", id)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnected command was not accepted on its new connection")
	}
	deadline := time.Now().Add(time.Second)
	for !lateAttempted.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !lateAttempted.Load() {
		t.Fatal("fake old connection did not attempt its late acknowledgment")
	}
}

func TestSoloistBackendReconnectFencesOldControllerAndCancellationClosesSocket(t *testing.T) {
	connections := make(chan int, 4)
	backend, _, cancel := newSoloistBackendTestHarness(t, func(id int, socket *websocket.Conn) {
		connections <- id
		soloistTestAuthAndQuery(id, socket, "paused")
	})
	waitForSoloistBackend(t, backend, func() bool {
		current := backend.currentConnection()
		return current != nil && backend.connectionReady(current) && current.controller != nil
	})
	old := backend.currentConnection()
	if err := old.websocket.Close(); err != nil {
		t.Fatalf("close first fake connection: %v", err)
	}
	waitForSoloistBackend(t, backend, func() bool {
		current := backend.currentConnection()
		return current != nil && current != old && backend.connectionReady(current) && current.controller != nil
	})
	if err := old.controller.Play(context.Background()); !errors.Is(err, ErrSoloistStaleSession) {
		t.Fatalf("stale controller crossed reconnect: %v", err)
	}
	cancel()
	select {
	case <-backend.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not stop the backend")
	}
	if snapshot := backend.state.Snapshot(); snapshot.Connected || snapshot.Authenticated {
		t.Fatalf("cancellation retained connection state: %+v", snapshot)
	}
}

func TestSoloistBackendRejectsInvalidEndpointAndTokenWithoutLeakingValues(t *testing.T) {
	private := "ws://user:private-secret@192.0.2.1:9090?api=private-secret"
	backend, err := NewSoloistBackend(context.Background(), private, soloistBackendTestToken)
	if backend != nil || !errors.Is(err, ErrSoloistBackendConfig) || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("invalid endpoint was not rejected with a fixed error: backend=%v err=%v", backend, err)
	}
}

type stubSpotifyAudioQualifier struct {
	err       error
	callCount int
}

func (q *stubSpotifyAudioQualifier) QualifyPlayableAudio(ctx context.Context) error {
	q.callCount++
	return q.err
}

func TestSpotifyBackendSelectorPrefersGoLibrespotAndPinsSelection(t *testing.T) {
	goLibrespot := &stubSpotifyAudioQualifier{}
	soloist := &stubSpotifyAudioQualifier{}
	selector := NewSpotifyBackendSelector(goLibrespot, soloist)

	kind, err := selector.Select(context.Background())
	if err != nil || kind != SpotifyBackendGoLibrespot {
		t.Fatalf("unexpected selection: kind=%q err=%v", kind, err)
	}
	if goLibrespot.callCount != 1 || soloist.callCount != 0 {
		t.Fatalf("selector did not prefer go-librespot: go=%d soloist=%d", goLibrespot.callCount, soloist.callCount)
	}
	if pinned, ok := selector.Pinned(); !ok || pinned != SpotifyBackendGoLibrespot {
		t.Fatalf("selection was not pinned: pinned=%q ok=%v", pinned, ok)
	}

	kind, err = selector.Select(context.Background())
	if err != nil || kind != SpotifyBackendGoLibrespot {
		t.Fatalf("pinned selector re-qualified and changed backend: kind=%q err=%v", kind, err)
	}
	if goLibrespot.callCount != 1 || soloist.callCount != 0 {
		t.Fatalf("pinned selector re-ran qualification: go=%d soloist=%d", goLibrespot.callCount, soloist.callCount)
	}
}

func TestSpotifyBackendSelectorFallsBackToSoloistOnceOnGoLibrespotFailure(t *testing.T) {
	goLibrespot := &stubSpotifyAudioQualifier{err: errors.New("audio key refused")}
	soloist := &stubSpotifyAudioQualifier{}
	selector := NewSpotifyBackendSelector(goLibrespot, soloist)

	kind, err := selector.Select(context.Background())
	if err != nil || kind != SpotifyBackendSoloist {
		t.Fatalf("unexpected fallback selection: kind=%q err=%v", kind, err)
	}
	if goLibrespot.callCount != 1 || soloist.callCount != 1 {
		t.Fatalf("fallback did not qualify exactly once per backend: go=%d soloist=%d", goLibrespot.callCount, soloist.callCount)
	}

	kind, err = selector.Select(context.Background())
	if err != nil || kind != SpotifyBackendSoloist {
		t.Fatalf("pinned fallback changed after qualification: kind=%q err=%v", kind, err)
	}
	if goLibrespot.callCount != 1 || soloist.callCount != 1 {
		t.Fatalf("pinned fallback re-ran qualification: go=%d soloist=%d", goLibrespot.callCount, soloist.callCount)
	}
}

func TestSpotifyBackendSelectorReturnsQualificationFailureWithoutSafeSwitches(t *testing.T) {
	goLibrespot := &stubSpotifyAudioQualifier{err: errors.New("oauth only")}
	soloist := &stubSpotifyAudioQualifier{err: errors.New("audio key failed")}
	selector := NewSpotifyBackendSelector(goLibrespot, soloist)

	kind, err := selector.Select(context.Background())
	if !errors.Is(err, ErrSpotifyBackendQualificationFailed) || kind != "" {
		t.Fatalf("unexpected failed selection: kind=%q err=%v", kind, err)
	}
	if goLibrespot.callCount != 1 || soloist.callCount != 1 {
		t.Fatalf("failed qualification did not assess both backends once: go=%d soloist=%d", goLibrespot.callCount, soloist.callCount)
	}
	if pinned, ok := selector.Pinned(); ok || pinned != "" {
		t.Fatalf("failed selection incorrectly pinned a backend: pinned=%q ok=%v", pinned, ok)
	}
}

func TestSpotifyBackendSelectionStateRetriesPendingUntilReadyAndPins(t *testing.T) {
	goLibrespot := &stubSpotifyAudioQualifier{err: ErrSpotifyBackendQualificationPending}
	state := newSpotifyBackendSelectionState(goLibrespot, nil)

	kind, err := state.Resolve(context.Background())
	if !errors.Is(err, ErrSpotifyBackendQualificationPending) || kind != "" {
		t.Fatalf("pending cold start should stay open: kind=%q err=%v", kind, err)
	}
	if goLibrespot.callCount != 1 {
		t.Fatalf("pending selection did not run the initial qualifier: go=%d", goLibrespot.callCount)
	}

	goLibrespot.err = nil
	kind, err = state.Resolve(context.Background())
	if err != nil || kind != SpotifyBackendGoLibrespot {
		t.Fatalf("pending startup did not recover after output appeared: kind=%q err=%v", kind, err)
	}
	if goLibrespot.callCount != 2 {
		t.Fatalf("successful retry did not re-run the qualifier once: go=%d", goLibrespot.callCount)
	}
	if pinned, ok := state.selector.Pinned(); !ok || pinned != SpotifyBackendGoLibrespot {
		t.Fatalf("resolved backend was not pinned: pinned=%q ok=%v", pinned, ok)
	}

	kind, err = state.Resolve(context.Background())
	if err != nil || kind != SpotifyBackendGoLibrespot {
		t.Fatalf("pinned backend re-qualified and changed: kind=%q err=%v", kind, err)
	}
	if goLibrespot.callCount != 2 {
		t.Fatalf("pinned backend re-ran qualification: go=%d", goLibrespot.callCount)
	}
}

func TestSpotifyBackendSelectorKeepsPendingColdStartOpenWithoutSwitching(t *testing.T) {
	goLibrespot := &stubSpotifyAudioQualifier{err: ErrSpotifyBackendQualificationPending}
	soloist := &stubSpotifyAudioQualifier{}
	selector := NewSpotifyBackendSelector(goLibrespot, soloist)

	kind, err := selector.Select(context.Background())
	if !errors.Is(err, ErrSpotifyBackendQualificationPending) || kind != "" {
		t.Fatalf("pending startup should stop before pinning or switching: kind=%q err=%v", kind, err)
	}
	if goLibrespot.callCount != 1 || soloist.callCount != 0 {
		t.Fatalf("pending qualification should not trigger Soloist fallback: go=%d soloist=%d", goLibrespot.callCount, soloist.callCount)
	}
	if pinned, ok := selector.Pinned(); ok || pinned != "" {
		t.Fatalf("pending qualification incorrectly pinned a backend: pinned=%q ok=%v", pinned, ok)
	}
}

func TestSpotifyHandlerAudioPendingThenReadyPinsGoLibrespot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/code":
			w.WriteHeader(http.StatusNoContent)
		case "/status":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"username":"fixture-user","stopped":false,"paused":false,"buffering":false,"track":null}`))
		case "/player/resume":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := os.Setenv("ZOMBIE_SPOTIFY_DAEMON_URL", server.URL); err != nil {
		t.Fatalf("set daemon URL: %v", err)
	}
	defer func() { _ = os.Unsetenv("ZOMBIE_SPOTIFY_DAEMON_URL") }()

	oldFactory := newSpotifyBridgeFactory
	var bridge *spotifyBridge
	newSpotifyBridgeFactory = func(ctx context.Context, fifoPath string) *spotifyBridge {
		bridge = &spotifyBridge{ctx: ctx, fifoPath: fifoPath, subscribers: make(map[*subscriber]struct{}), maxBuffer: 128 * 1024}
		return bridge
	}
	defer func() { newSpotifyBridgeFactory = oldFactory }()

	config := Config{Mode: "spotify", Listen: "127.0.0.1:0", Token: "0123456789012345678901234567890123456789", StateDir: t.TempDir()}
	handler := handlerWithAirPlayDACPAndFullDiagnostics(context.Background(), config, nil, nil, nil, nil, nil, nil, nil)
	authorized := func(method, path string) *http.Request {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+config.Token)
		return req
	}

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, authorized(http.MethodGet, "/audio"))
	if res.Code != http.StatusServiceUnavailable || !strings.Contains(res.Body.String(), "audio unavailable") {
		t.Fatalf("pending /audio did not fail boundedly: status=%d body=%q", res.Code, res.Body.String())
	}

	resume := httptest.NewRecorder()
	handler.ServeHTTP(resume, authorized(http.MethodPost, "/player/resume"))
	if resume.Code != http.StatusOK {
		t.Fatalf("player/resume was blocked while pending: status=%d body=%q", resume.Code, resume.Body.String())
	}

	bridge.mu.Lock()
	bridge.buffer = [][]byte{[]byte("known-audio")}
	bridge.bufferBytes = len("known-audio")
	bridge.lastEncoded = time.Now()
	bridge.encodedBytes = uint64(len("known-audio"))
	bridge.mu.Unlock()

	readyReq := authorized(http.MethodGet, "/audio")
	readyCtx, readyCancel := context.WithCancel(readyReq.Context())
	readyReq = readyReq.WithContext(readyCtx)
	ready := httptest.NewRecorder()
	go func() {
		time.AfterFunc(25*time.Millisecond, readyCancel)
	}()
	handler.ServeHTTP(ready, readyReq)
	if ready.Code != http.StatusOK || ready.Body.String() != "known-audio" {
		t.Fatalf("ready /audio did not serve pinned audio: status=%d body=%q", ready.Code, ready.Body.String())
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, authorized(http.MethodGet, "/health"))
	if health.Code != http.StatusOK {
		t.Fatalf("health returned %d: %s", health.Code, health.Body.String())
	}
	var body struct {
		Backend    string `json:"backend"`
		AudioReady bool   `json:"audioReady"`
	}
	if err := json.Unmarshal(health.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v: %s", err, health.Body.String())
	}
	if body.Backend != string(SpotifyBackendGoLibrespot) || !body.AudioReady {
		t.Fatalf("health did not pin go-librespot after output became ready: %+v", body)
	}

	bridge.mu.Lock()
	bridge.lastEncoded = time.Time{}
	bridge.encodedBytes = 0
	bridge.buffer = nil
	bridge.bufferBytes = 0
	bridge.mu.Unlock()

	stable := httptest.NewRecorder()
	handler.ServeHTTP(stable, authorized(http.MethodGet, "/health"))
	if stable.Code != http.StatusOK {
		t.Fatalf("post-publication health returned %d: %s", stable.Code, stable.Body.String())
	}
	if err := json.Unmarshal(stable.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode stable health: %v: %s", err, stable.Body.String())
	}
	if body.Backend != string(SpotifyBackendGoLibrespot) {
		t.Fatalf("pinned backend switched after publication: %+v", body)
	}
}

func TestSpotifyHandlerAudioTerminalFailureKeepsGoLibrespotPinnedWithoutSoloistFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/code":
			w.WriteHeader(http.StatusNoContent)
		case "/status":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"username":"","stopped":true,"paused":false,"buffering":false,"track":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := os.Setenv("ZOMBIE_SPOTIFY_DAEMON_URL", server.URL); err != nil {
		t.Fatalf("set daemon URL: %v", err)
	}
	defer func() { _ = os.Unsetenv("ZOMBIE_SPOTIFY_DAEMON_URL") }()

	oldFactory := newSpotifyBridgeFactory
	newSpotifyBridgeFactory = func(ctx context.Context, fifoPath string) *spotifyBridge {
		return &spotifyBridge{ctx: ctx, fifoPath: fifoPath, subscribers: make(map[*subscriber]struct{}), maxBuffer: 128 * 1024}
	}
	defer func() { newSpotifyBridgeFactory = oldFactory }()

	config := Config{Mode: "spotify", Listen: "127.0.0.1:0", Token: "0123456789012345678901234567890123456789", StateDir: t.TempDir()}
	handler := handlerWithAirPlayDACPAndFullDiagnostics(context.Background(), config, nil, nil, nil, nil, nil, nil, nil)
	authorized := func(method, path string) *http.Request {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+config.Token)
		return req
	}

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, authorized(http.MethodGet, "/audio"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("terminal qualification did not return bounded failure: %d %s", res.Code, res.Body.String())
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, authorized(http.MethodGet, "/health"))
	if health.Code != http.StatusServiceUnavailable {
		t.Fatalf("terminal health returned %d: %s", health.Code, health.Body.String())
	}
}
