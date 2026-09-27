package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const (
	soloistWSDialTimeout   = 3 * time.Second
	soloistWSWriteTimeout  = 2 * time.Second
	maxSoloistCommandBytes = 1024
)

var (
	ErrSoloistWSEndpoint       = errors.New("invalid Soloist WebSocket endpoint")
	ErrSoloistWSDial           = errors.New("Soloist WebSocket connection failed")
	ErrSoloistWSTimeout        = errors.New("Soloist WebSocket operation timed out")
	ErrSoloistWSCanceled       = errors.New("Soloist WebSocket connection canceled")
	ErrSoloistWSClosed         = errors.New("Soloist WebSocket connection closed")
	ErrSoloistWSRead           = errors.New("Soloist WebSocket read failed")
	ErrSoloistWSWrite          = errors.New("Soloist WebSocket write failed")
	ErrSoloistWSFrameTooLarge  = errors.New("Soloist WebSocket frame exceeds size limit")
	ErrSoloistWSBinaryFrame    = errors.New("Soloist WebSocket requires text frames")
	ErrSoloistWSMalformedFrame = errors.New("invalid Soloist WebSocket event")
	ErrSoloistWSInvalidCommand = errors.New("invalid Soloist WebSocket command")
	ErrSoloistWSUnavailable    = errors.New("Soloist WebSocket transport unavailable")

	errSoloistTextCodecTarget = errors.New("unsupported Soloist text codec target")
	errSoloistBinaryFrame     = errors.New("unsupported Soloist binary frame")
)

var soloistTextCodec = websocket.Codec{
	Marshal: func(value any) ([]byte, byte, error) {
		message, ok := value.(string)
		if !ok {
			return nil, websocket.TextFrame, errSoloistTextCodecTarget
		}
		return []byte(message), websocket.TextFrame, nil
	},
	Unmarshal: func(data []byte, payloadType byte, target any) error {
		if payloadType != websocket.TextFrame {
			return errSoloistBinaryFrame
		}
		message, ok := target.(*string)
		if !ok {
			return errSoloistTextCodecTarget
		}
		*message = string(data)
		return nil
	},
}

// SoloistWebSocket observes one explicitly requested loopback connection.
// It does not reconnect, launch Soloist, or route audio.
type SoloistWebSocket struct {
	mu         sync.Mutex
	finishOnce sync.Once
	conn       *websocket.Conn
	state      *SoloistState
	session    SoloistSession
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	closed     bool
	err        error
}

// ConnectSoloistWebSocket dials a numeric loopback Soloist endpoint exactly
// once. The supplied context owns the observer lifetime after connection.
func ConnectSoloistWebSocket(ctx context.Context, endpoint string, state *SoloistState) (*SoloistWebSocket, error) {
	if ctx == nil || state == nil {
		return nil, ErrSoloistWSUnavailable
	}
	location, err := validateSoloistWSEndpoint(endpoint)
	if err != nil {
		return nil, ErrSoloistWSEndpoint
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrSoloistWSTimeout
		}
		return nil, ErrSoloistWSCanceled
	}

	origin := &url.URL{Scheme: "http", Host: location.Host}
	config, err := websocket.NewConfig(location.String(), origin.String())
	if err != nil {
		return nil, ErrSoloistWSEndpoint
	}
	config.Dialer = &net.Dialer{Timeout: soloistWSDialTimeout, KeepAlive: 30 * time.Second}
	dialCtx, cancelDial := context.WithTimeout(ctx, soloistWSDialTimeout)
	conn, err := config.DialContext(dialCtx)
	dialContextErr := dialCtx.Err()
	cancelDial()
	if err != nil {
		if errors.Is(dialContextErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrSoloistWSTimeout
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, ErrSoloistWSCanceled
		}
		return nil, ErrSoloistWSDial
	}
	if ctx.Err() != nil {
		_ = conn.Close()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrSoloistWSTimeout
		}
		return nil, ErrSoloistWSCanceled
	}

	conn.MaxPayloadBytes = maxSoloistEventBytes
	session := state.BeginSession()
	lifetime, cancel := context.WithCancel(ctx)
	observer := &SoloistWebSocket{
		conn:    conn,
		state:   state,
		session: session,
		ctx:     lifetime,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go observer.readLoop()
	go observer.watchCancellation()
	return observer, nil
}

// Session returns the opaque identity used to fence commands for this socket.
func (w *SoloistWebSocket) Session() SoloistSession {
	if w == nil {
		return SoloistSession{}
	}
	return w.session
}

// Done is closed after explicit close, context cancellation, or connection
// failure has released the session state.
func (w *SoloistWebSocket) Done() <-chan struct{} {
	if w == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return w.done
}

// Err returns a fixed, sanitized termination reason after Done closes.
func (w *SoloistWebSocket) Err() error {
	if w == nil {
		return ErrSoloistWSUnavailable
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Close explicitly releases this socket and its matching Soloist state.
func (w *SoloistWebSocket) Close() error {
	if w == nil {
		return nil
	}
	w.finish(ErrSoloistWSClosed)
	return nil
}

// SendCommand writes one bounded official control command. The state session
// lock remains held through the bounded write, so BeginSession cannot overtake
// validation and then allow an old command onto the new session.
func (w *SoloistWebSocket) SendCommand(ctx context.Context, session SoloistSession, frame []byte) error {
	if w == nil || w.state == nil || w.conn == nil || ctx == nil {
		return ErrSoloistWSUnavailable
	}
	if ctx.Err() != nil {
		return soloistWSContextError(ctx)
	}
	if len(frame) > maxSoloistCommandBytes || !validSoloistCommandFrame(frame) {
		return ErrSoloistWSInvalidCommand
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrSoloistWSClosed
	}
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	if ctx.Err() != nil {
		return soloistWSContextError(ctx)
	}
	if session != w.session || !w.state.matchesSession(session) {
		return ErrSoloistStaleSession
	}
	if !w.state.connected {
		return ErrSoloistDisconnected
	}
	if !w.state.authenticationKnown || !w.state.authenticated {
		return ErrSoloistUnauthenticated
	}
	if err := validateSoloistCommandState(frame, w.state); err != nil {
		return err
	}

	deadline := time.Now().Add(soloistWSWriteTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := w.conn.SetWriteDeadline(deadline); err != nil {
		return ErrSoloistWSWrite
	}
	writeDone := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = w.conn.SetWriteDeadline(time.Now())
		case <-writeDone:
		}
	}()
	err := soloistTextCodec.Send(w.conn, string(frame))
	close(writeDone)
	<-watchDone
	_ = w.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		if ctx.Err() != nil {
			return soloistWSContextError(ctx)
		}
		return ErrSoloistWSWrite
	}
	return nil
}

func (w *SoloistWebSocket) readLoop() {
	result := ErrSoloistWSClosed
	for {
		if w.ctx.Err() != nil {
			result = soloistWSContextError(w.ctx)
			break
		}
		var frame string
		if err := soloistTextCodec.Receive(w.conn, &frame); err != nil {
			switch {
			case errors.Is(err, websocket.ErrFrameTooLarge):
				result = ErrSoloistWSFrameTooLarge
			case errors.Is(err, errSoloistBinaryFrame):
				result = ErrSoloistWSBinaryFrame
			case w.ctx.Err() != nil:
				result = soloistWSContextError(w.ctx)
			default:
				result = ErrSoloistWSRead
			}
			break
		}
		if len(frame) > maxSoloistEventBytes {
			result = ErrSoloistWSFrameTooLarge
			break
		}
		err := w.state.Apply(w.session, []byte(frame))
		switch {
		case err == nil, errors.Is(err, ErrSoloistUnsupported), errors.Is(err, ErrSoloistStaleEvent):
			continue
		case errors.Is(err, ErrSoloistStaleSession):
			result = ErrSoloistStaleSession
		case errors.Is(err, ErrSoloistEventTooLarge):
			result = ErrSoloistWSFrameTooLarge
		default:
			result = ErrSoloistWSMalformedFrame
		}
		break
	}
	w.finish(result)
}

func (w *SoloistWebSocket) watchCancellation() {
	select {
	case <-w.ctx.Done():
		w.finish(soloistWSContextError(w.ctx))
	case <-w.done:
	}
}

func soloistWSContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrSoloistWSTimeout
	}
	return ErrSoloistWSCanceled
}

func (w *SoloistWebSocket) finish(reason error) {
	w.finishOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.err = reason
		_ = w.conn.SetWriteDeadline(time.Now().Add(soloistWSWriteTimeout))
		_ = w.conn.Close()
		w.mu.Unlock()

		w.cancel()
		_ = w.state.EndSession(w.session)
		close(w.done)
	})
}

func validateSoloistWSEndpoint(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 2048 || strings.Contains(raw, "#") {
		return nil, ErrSoloistWSEndpoint
	}
	location, err := url.Parse(raw)
	if err != nil || location.Scheme != "ws" || location.User != nil || location.Opaque != "" || location.Path != "" || location.RawPath != "" || location.RawQuery != "" || location.ForceQuery || location.Fragment != "" || location.RawFragment != "" {
		return nil, ErrSoloistWSEndpoint
	}
	host, portText, err := net.SplitHostPort(location.Host)
	if err != nil || host == "" || portText == "" {
		return nil, ErrSoloistWSEndpoint
	}
	for _, digit := range portText {
		if digit < '0' || digit > '9' {
			return nil, ErrSoloistWSEndpoint
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, ErrSoloistWSEndpoint
	}
	address := net.ParseIP(location.Hostname())
	if address == nil || !address.IsLoopback() || strings.Contains(location.Hostname(), "%") {
		return nil, ErrSoloistWSEndpoint
	}
	if host != location.Hostname() {
		return nil, ErrSoloistWSEndpoint
	}
	return location, nil
}

func validSoloistCommandFrame(frame []byte) bool {
	if len(frame) == 0 || !json.Valid(frame) || !soloistJSONHasUniqueKeys(frame) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(frame, &fields) != nil || fields == nil {
		return false
	}
	_, hasPosition := fields["position_ms"]
	_, hasVolume := fields["volume"]
	for key := range fields {
		switch key {
		case "type", "command", "position_ms", "volume":
		default:
			return false
		}
	}
	var command struct {
		Type       string `json:"type"`
		Command    string `json:"command"`
		PositionMS *int64 `json:"position_ms"`
		Volume     *int   `json:"volume"`
	}
	if json.Unmarshal(frame, &command) != nil || command.Type != "command" {
		return false
	}
	switch command.Command {
	case "play", "pause", "skip_next", "skip_prev":
		return !hasPosition && !hasVolume
	case "seek":
		return hasPosition && command.PositionMS != nil && !hasVolume && *command.PositionMS >= 0 && *command.PositionMS <= maxSoloistPositionMS
	case "set_volume":
		return hasVolume && command.Volume != nil && !hasPosition && *command.Volume >= 0 && *command.Volume <= 100
	default:
		return false
	}
}

func validateSoloistCommandState(frame []byte, state *SoloistState) error {
	var command struct {
		Command    string `json:"command"`
		PositionMS *int64 `json:"position_ms"`
	}
	if json.Unmarshal(frame, &command) != nil {
		return ErrSoloistWSInvalidCommand
	}
	if command.Command == "seek" && state.track != nil && state.track.DurationMS > 0 && *command.PositionMS > state.track.DurationMS {
		return ErrSoloistInvalidCommand
	}
	return nil
}

var _ SoloistCommandTransport = (*SoloistWebSocket)(nil)
