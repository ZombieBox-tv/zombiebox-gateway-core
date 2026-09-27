package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestSoloistWebSocketObservesStateSendsControlAndCloses(t *testing.T) {
	commands := make(chan string, 1)
	endpoint := newSoloistWSTestServer(t, func(conn *websocket.Conn) {
		for _, frame := range []string{
			`{"type":"auth_state","logged_in":true,"is_active":true}`,
			`{"type":"playback_state","status":"paused","item":{"uri":"spotify:track:local-test","entity_type":"track","decorations":{"identity":{"name":"Local Test"},"playback":{"duration_ms":60000}}},"position":{"position_ms":1000,"timestamp_ms":1780000000000,"speed":0.0}}`,
			`{"type":"position_sync","position":{"position_ms":2500,"timestamp_ms":1780000001000,"speed":0.0}}`,
		} {
			if err := websocket.Message.Send(conn, frame); err != nil {
				return
			}
		}
		var command string
		if err := websocket.Message.Receive(conn, &command); err == nil {
			commands <- command
		}
	})
	state := NewSoloistState(nil)
	observer, err := ConnectSoloistWebSocket(context.Background(), endpoint, state)
	if err != nil {
		t.Fatalf("connect local Soloist socket: %v", err)
	}

	waitForSoloistWS(t, observer.Done(), func() bool {
		snapshot := state.Snapshot()
		return snapshot.Authenticated && snapshot.Active && snapshot.Track != nil && snapshot.PositionKnown && snapshot.PositionMS == 2500
	})
	frame := []byte(`{"type":"command","command":"pause"}`)
	if err := observer.SendCommand(context.Background(), observer.Session(), frame); err != nil {
		t.Fatalf("send pause command: %v", err)
	}
	select {
	case command := <-commands:
		if command != string(frame) {
			t.Fatalf("server received wrong control: %q", command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local server did not receive control")
	}

	if err := observer.Close(); err != nil {
		t.Fatalf("explicit close: %v", err)
	}
	select {
	case <-observer.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish observer")
	}
	if !errors.Is(observer.Err(), ErrSoloistWSClosed) {
		t.Fatalf("close returned unexpected fixed status: %v", observer.Err())
	}
	if snapshot := state.Snapshot(); snapshot.Connected || snapshot.AuthenticationKnown || snapshot.Track != nil {
		t.Fatalf("close left session state active: %+v", snapshot)
	}
	if err := observer.Close(); err != nil {
		t.Fatalf("repeated close was not idempotent: %v", err)
	}
}

func TestSoloistWebSocketFencesCommandsAfterReconnect(t *testing.T) {
	type receivedCommand struct {
		connection int32
		text       string
	}
	commands := make(chan receivedCommand, 2)
	var connections atomic.Int32
	endpoint := newSoloistWSTestServer(t, func(conn *websocket.Conn) {
		connection := connections.Add(1)
		if err := websocket.Message.Send(conn, `{"type":"auth_state","logged_in":true,"is_active":false}`); err != nil {
			return
		}
		var command string
		if err := websocket.Message.Receive(conn, &command); err == nil {
			commands <- receivedCommand{connection: connection, text: command}
		}
	})
	state := NewSoloistState(nil)
	first, err := ConnectSoloistWebSocket(context.Background(), endpoint, state)
	if err != nil {
		t.Fatalf("first local connection: %v", err)
	}
	waitForSoloistWS(t, first.Done(), func() bool { return state.Snapshot().Authenticated })
	second, err := ConnectSoloistWebSocket(context.Background(), endpoint, state)
	if err != nil {
		_ = first.Close()
		t.Fatalf("reconnected local connection: %v", err)
	}
	defer first.Close()
	defer second.Close()
	if first.Session() == second.Session() {
		t.Fatal("reconnect reused the prior session identity")
	}
	waitForSoloistWS(t, second.Done(), func() bool { return state.Snapshot().Authenticated })

	frame := []byte(`{"type":"command","command":"pause"}`)
	if err := first.SendCommand(context.Background(), first.Session(), frame); !errors.Is(err, ErrSoloistStaleSession) {
		t.Fatalf("stale socket sent a control: %v", err)
	}
	select {
	case command := <-commands:
		t.Fatalf("stale command reached local connection %d: %s", command.connection, command.text)
	case <-time.After(75 * time.Millisecond):
	}
	if err := second.SendCommand(context.Background(), second.Session(), frame); err != nil {
		t.Fatalf("current socket control failed: %v", err)
	}
	select {
	case command := <-commands:
		if command.connection != 2 || command.text != string(frame) {
			t.Fatalf("control went to the wrong connection: %+v", command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("current local connection did not receive control")
	}
}

func TestSoloistWebSocketRejectsHostileEndpoints(t *testing.T) {
	invalid := []string{
		"http://127.0.0.1:9090",
		"wss://127.0.0.1:9090",
		"ws://localhost:9090",
		"ws://192.0.2.1:9090",
		"ws://127.0.0.1",
		"ws://127.0.0.1:+9090",
		"ws://127.0.0.1:0",
		"ws://127.0.0.1:70000",
		"ws://127.0.0.1:9090/path",
		"ws://127.0.0.1:9090?key=private-key",
		"ws://user:private-key@127.0.0.1:9090",
		"ws://127.0.0.1:9090#private-key",
	}
	for _, endpoint := range invalid {
		t.Run(endpoint, func(t *testing.T) {
			_, err := ConnectSoloistWebSocket(context.Background(), endpoint, NewSoloistState(nil))
			if !errors.Is(err, ErrSoloistWSEndpoint) {
				t.Fatalf("endpoint was not rejected before dialing: %v", err)
			}
			if strings.Contains(err.Error(), "private-key") {
				t.Fatalf("endpoint secret leaked through error: %v", err)
			}
		})
	}
	for _, endpoint := range []string{"ws://127.0.0.1:9090", "ws://[::1]:9090"} {
		if _, err := validateSoloistWSEndpoint(endpoint); err != nil {
			t.Fatalf("numeric loopback endpoint was rejected: %s: %v", endpoint, err)
		}
	}
}

func TestSoloistWebSocketBoundsHandshakeTimeoutWithoutLeakingEndpoint(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	defer server.Close()
	defer close(release)
	endpoint := localWSEndpoint(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := ConnectSoloistWebSocket(ctx, endpoint, NewSoloistState(nil))
	if !errors.Is(err, ErrSoloistWSTimeout) {
		t.Fatalf("handshake deadline was not returned as a fixed timeout: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("handshake exceeded its bounded deadline: %s", elapsed)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("test server did not receive handshake")
	}
	if strings.Contains(err.Error(), endpoint) {
		t.Fatalf("endpoint leaked through timeout error: %v", err)
	}
}

func TestSoloistWebSocketDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	_, err := ConnectSoloistWebSocket(context.Background(), localWSEndpoint(redirect.URL), NewSoloistState(nil))
	if !errors.Is(err, ErrSoloistWSDial) {
		t.Fatalf("redirecting handshake returned unexpected error: %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("WebSocket dial followed a redirect to another endpoint (%d requests)", redirected.Load())
	}
}

func TestSoloistWebSocketRejectsOversizedBinaryAndMalformedFrames(t *testing.T) {
	tests := []struct {
		name     string
		send     func(*websocket.Conn) error
		want     error
		contains string
	}{
		{
			name: "oversized",
			send: func(conn *websocket.Conn) error {
				return websocket.Message.Send(conn, strings.Repeat("x", maxSoloistEventBytes+1))
			},
			want: ErrSoloistWSFrameTooLarge,
		},
		{
			name: "binary",
			send: func(conn *websocket.Conn) error {
				return websocket.Message.Send(conn, []byte("binary-private-payload"))
			},
			want:     ErrSoloistWSBinaryFrame,
			contains: "binary-private-payload",
		},
		{
			name: "malformed",
			send: func(conn *websocket.Conn) error {
				return websocket.Message.Send(conn, `{"type":"auth_state","logged_in":"secret-account","is_active":`)
			},
			want:     ErrSoloistWSMalformedFrame,
			contains: "secret-account",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint := newSoloistWSTestServer(t, func(conn *websocket.Conn) {
				_ = test.send(conn)
			})
			observer, err := ConnectSoloistWebSocket(context.Background(), endpoint, NewSoloistState(nil))
			if err != nil {
				t.Fatalf("connect local test server: %v", err)
			}
			defer observer.Close()
			select {
			case <-observer.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("invalid frame did not terminate the observer")
			}
			if !errors.Is(observer.Err(), test.want) {
				t.Fatalf("unexpected fixed frame error: %v", observer.Err())
			}
			if test.contains != "" && strings.Contains(observer.Err().Error(), test.contains) {
				t.Fatalf("frame payload leaked through error: %v", observer.Err())
			}
		})
	}
}

func TestSoloistWebSocketContextCancellationReleasesState(t *testing.T) {
	endpoint := newSoloistWSTestServer(t, func(conn *websocket.Conn) {
		var frame string
		_ = websocket.Message.Receive(conn, &frame)
	})
	state := NewSoloistState(nil)
	ctx, cancel := context.WithCancel(context.Background())
	observer, err := ConnectSoloistWebSocket(ctx, endpoint, state)
	if err != nil {
		cancel()
		t.Fatalf("connect local server: %v", err)
	}
	cancel()
	select {
	case <-observer.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not close observer")
	}
	if !errors.Is(observer.Err(), ErrSoloistWSCanceled) {
		t.Fatalf("unexpected cancellation status: %v", observer.Err())
	}
	if snapshot := state.Snapshot(); snapshot.Connected || snapshot.AuthenticationKnown {
		t.Fatalf("cancellation left session state active: %+v", snapshot)
	}
}

func TestSoloistWebSocketContextDeadlineClosesIdleRead(t *testing.T) {
	endpoint := newSoloistWSTestServer(t, func(conn *websocket.Conn) {
		var frame string
		_ = websocket.Message.Receive(conn, &frame)
	})
	state := NewSoloistState(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	observer, err := ConnectSoloistWebSocket(ctx, endpoint, state)
	if err != nil {
		t.Fatalf("connect local server: %v", err)
	}
	select {
	case <-observer.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context deadline did not close the idle observer")
	}
	if !errors.Is(observer.Err(), ErrSoloistWSTimeout) {
		t.Fatalf("unexpected deadline status: %v", observer.Err())
	}
	if snapshot := state.Snapshot(); snapshot.Connected || snapshot.AuthenticationKnown {
		t.Fatalf("deadline left session state active: %+v", snapshot)
	}
}

func newSoloistWSTestServer(t *testing.T, handler func(*websocket.Conn)) string {
	t.Helper()
	server := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		handler(conn)
	}))
	t.Cleanup(server.Close)
	return localWSEndpoint(server.URL)
}

func localWSEndpoint(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func waitForSoloistWS(t *testing.T, done <-chan struct{}, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-done:
			t.Fatalf("observer ended before state arrived")
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("timed out waiting for Soloist state")
		}
	}
}
