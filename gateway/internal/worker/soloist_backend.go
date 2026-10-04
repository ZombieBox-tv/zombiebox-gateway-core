package worker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	minimumSoloistBackendTokenBytes = 32
	maxSoloistBackendRequestBytes   = 1024
	soloistBackendReconnectInitial  = 250 * time.Millisecond
	soloistBackendReconnectMaximum  = 5 * time.Second
	soloistBackendAckTimeout        = 4 * time.Second
	soloistBackendQueryTimeout      = 3 * time.Second
)

var (
	ErrSoloistBackendConfig      = errors.New("invalid Soloist backend configuration")
	ErrSoloistBackendAckTimeout  = errors.New("Soloist command acknowledgment timed out")
	ErrSoloistBackendUnavailable = errors.New("Soloist backend unavailable")
)

// SoloistBackend supervises a private, loopback WebSocket connection and
// exposes only the bounded Spotify worker HTTP contract. It does not launch
// Soloist or transport audio.
type SoloistBackend struct {
	ctx      context.Context
	cancel   context.CancelFunc
	endpoint string
	token    [sha256.Size]byte
	state    *SoloistState

	mu      sync.RWMutex
	current *soloistBackendConnection
	done    chan struct{}
	output  *SoloistPCMHub
	audio   *SoloistPCMRoute

	ackTimeout   time.Duration
	queryTimeout time.Duration
}

type soloistBackendConnection struct {
	websocket    *SoloistWebSocket
	session      SoloistSession
	controller   *SoloistController
	commandGate  chan struct{}
	ready        chan struct{}
	readyOnce    sync.Once
	authRevision uint64
}

type soloistRevisionBoundTransport struct {
	websocket    *SoloistWebSocket
	authRevision uint64
}

func (t soloistRevisionBoundTransport) SendCommand(ctx context.Context, session SoloistSession, frame []byte) error {
	if t.websocket == nil || t.authRevision == 0 {
		return ErrSoloistWSUnavailable
	}
	return t.websocket.sendCommandForAuthRevision(ctx, session, frame, t.authRevision)
}

// NewSoloistBackend validates its private endpoint and bearer token, creates
// its reducer, and starts the bounded reconnect loop. The returned backend's
// Handler can be mounted by a later runtime.
func NewSoloistBackend(ctx context.Context, endpoint, token string) (*SoloistBackend, error) {
	if ctx == nil || len(token) < minimumSoloistBackendTokenBytes {
		return nil, ErrSoloistBackendConfig
	}
	location, err := validateSoloistWSEndpoint(endpoint)
	if err != nil {
		return nil, ErrSoloistBackendConfig
	}
	lifetime, cancel := context.WithCancel(ctx)
	backend := &SoloistBackend{
		ctx:          lifetime,
		cancel:       cancel,
		endpoint:     location.String(),
		token:        sha256.Sum256([]byte(token)),
		state:        NewSoloistState(nil),
		done:         make(chan struct{}),
		ackTimeout:   soloistBackendAckTimeout,
		queryTimeout: soloistBackendQueryTimeout,
	}
	go backend.reconnectLoop()
	return backend, nil
}

// AttachOutput connects the private sink monitor at the composition root.
func (b *SoloistBackend) AttachOutput(output *SoloistPCMHub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.output = output
	b.audio = NewSoloistPCMRoute(nil, output.Stream)
}

func (b *SoloistBackend) audioAvailable() bool {
	b.mu.RLock()
	output := b.output
	b.mu.RUnlock()
	state := b.state.Snapshot()
	return output != nil && output.Active() && state.Connected && state.Authenticated && state.Active && state.Status == SoloistPlaying && b.connectionReady(b.currentConnection())
}

// Handler returns the authenticated private worker HTTP boundary.
func (b *SoloistBackend) Handler() http.Handler {
	return b
}

// ServeHTTP implements the existing Spotify worker's narrow status/control
// shape without forwarding Soloist's raw HTTP or WebSocket data.
func (b *SoloistBackend) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if b == nil || request == nil {
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if !b.authorized(request) {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch request.URL.Path {
	case "/health":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		b.serveHealth(writer)
	case "/status":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		b.serveStatus(writer)
	case "/audio":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		b.mu.RLock()
		audio := b.audio
		b.mu.RUnlock()
		if audio == nil || !b.audioAvailable() {
			writeSoloistJSONError(writer, http.StatusServiceUnavailable, "audio unavailable")
			return
		}
		audio.ServeHTTP(writer, request)
	case "/auth/code":
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		writeSoloistJSONError(writer, http.StatusServiceUnavailable, "authorization unavailable")
	default:
		if strings.HasPrefix(request.URL.Path, "/player/") {
			b.serveControl(writer, request)
			return
		}
		http.NotFound(writer, request)
	}
}

// Close stops reconnection and closes the current transport, if any.
func (b *SoloistBackend) Close() error {
	if b == nil {
		return nil
	}
	b.cancel()
	b.mu.RLock()
	current := b.current
	b.mu.RUnlock()
	if current != nil {
		_ = current.websocket.Close()
	}
	return nil
}

// Done closes after cancellation has stopped the reconnect loop.
func (b *SoloistBackend) Done() <-chan struct{} {
	if b == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return b.done
}

func (b *SoloistBackend) authorized(request *http.Request) bool {
	if b == nil || request == nil {
		return false
	}
	const prefix = "Bearer "
	values := request.Header.Values("Authorization")
	authorization := ""
	if len(values) == 1 {
		authorization = values[0]
	}
	candidate := ""
	wellFormed := len(values) == 1 && strings.HasPrefix(authorization, prefix)
	if wellFormed {
		candidate = authorization[len(prefix):]
	}
	provided := sha256.Sum256([]byte(candidate))
	matched := subtle.ConstantTimeCompare(provided[:], b.token[:])
	return wellFormed && matched == 1
}

func (b *SoloistBackend) serveHealth(writer http.ResponseWriter) {
	diagnostics := b.state.Diagnostics()
	current := b.currentConnection()
	response := struct {
		Ready                 bool                  `json:"ready"`
		AccountReady          bool                  `json:"accountReady"`
		Connected             bool                  `json:"connected"`
		AuthenticationKnown   bool                  `json:"authentication_known"`
		Authenticated         bool                  `json:"authenticated"`
		ActivityKnown         bool                  `json:"activity_known"`
		Active                bool                  `json:"active"`
		Status                SoloistPlaybackStatus `json:"status"`
		AudioAvailable        bool                  `json:"audio_available"`
		AudioReady            bool                  `json:"audioReady"`
		Backend               string                `json:"backend"`
		AuthMode              string                `json:"authMode"`
		AuthorizationRequired bool                  `json:"authorizationRequired"`
	}{
		Ready:               b.audioAvailable(),
		AccountReady:        diagnostics.Connected && diagnostics.Authenticated && b.connectionReady(current),
		Connected:           diagnostics.Connected,
		AuthenticationKnown: diagnostics.AuthenticationKnown,
		Authenticated:       diagnostics.Authenticated,
		ActivityKnown:       diagnostics.ActivityKnown,
		Active:              diagnostics.Active,
		Status:              diagnostics.Status,
		AudioAvailable:      b.audioAvailable(),
		AudioReady:          b.audioAvailable(),
		Backend:             "soloist", AuthMode: "zeroconf",
		AuthorizationRequired: !diagnostics.Authenticated,
	}
	writeSoloistJSON(writer, http.StatusOK, response)
}

func (b *SoloistBackend) serveStatus(writer http.ResponseWriter) {
	current := b.currentConnection()
	snapshot := SoloistSnapshot{Status: SoloistIdle, PositionMS: -1, LocalOutputAgeMS: -1}
	var candidate *soloistArtworkCandidate
	if b.connectionReady(current) {
		snapshot, candidate = b.state.playbackSnapshotForAuthRevision(current.session, current.authRevision)
	}
	status := struct {
		Stopped        bool                `json:"stopped"`
		Paused         bool                `json:"paused"`
		Buffering      bool                `json:"buffering"`
		Volume         *int                `json:"volume,omitempty"`
		VolumeSteps    *int                `json:"volume_steps,omitempty"`
		AudioAvailable bool                `json:"audio_available"`
		Backend        string              `json:"backend"`
		PCMFormat      string              `json:"pcm_format"`
		Track          *soloistWorkerTrack `json:"track"`
	}{
		Stopped:        !snapshot.Connected || !snapshot.Authenticated || !snapshot.ActivityKnown || !snapshot.Active || snapshot.Status == SoloistIdle,
		Paused:         snapshot.ActivityKnown && snapshot.Active && snapshot.Status == SoloistPaused,
		Buffering:      snapshot.ActivityKnown && snapshot.Active && snapshot.Status == SoloistBuffering,
		AudioAvailable: b.audioAvailable(),
		Backend:        "soloist", PCMFormat: soloistPCMFormat,
	}
	if snapshot.VolumeKnown {
		volume := snapshot.Volume
		steps := 100
		status.Volume = &volume
		status.VolumeSteps = &steps
	}
	if snapshot.Track != nil && snapshot.Connected && snapshot.Authenticated {
		position := int64(-1)
		if snapshot.PositionKnown {
			position = snapshot.PositionMS
		}
		var cover *string
		if candidate != nil && validSoloistCoverURL(candidate.url) {
			value := candidate.url
			cover = &value
		}
		artists := append([]string(nil), snapshot.Track.Artists...)
		if artists == nil {
			artists = []string{}
		}
		status.Track = &soloistWorkerTrack{
			Name:     snapshot.Track.Title,
			Cover:    cover,
			Artists:  artists,
			Duration: snapshot.Track.DurationMS,
			Position: position,
		}
	}
	writeSoloistJSON(writer, http.StatusOK, status)
}

type soloistWorkerTrack struct {
	Name     string   `json:"name"`
	Cover    *string  `json:"album_cover_url"`
	Artists  []string `json:"artist_names"`
	Duration int64    `json:"duration"`
	Position int64    `json:"position"`
}

func (b *SoloistBackend) serveControl(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	action := strings.TrimPrefix(request.URL.Path, "/player/")
	if strings.Contains(action, "/") || !validSoloistBackendAction(action) {
		http.NotFound(writer, request)
		return
	}
	position, volume, err := decodeSoloistBackendControl(request.Body, action)
	if err != nil {
		writeSoloistJSONError(writer, http.StatusBadRequest, "invalid command")
		return
	}
	current := b.currentConnection()
	if current == nil {
		writeSoloistJSONError(writer, http.StatusServiceUnavailable, "worker unavailable")
		return
	}
	if err := b.runControl(request.Context(), current, action, position, volume); err != nil {
		status, message := soloistBackendControlError(err)
		writeSoloistJSONError(writer, status, message)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (b *SoloistBackend) runControl(ctx context.Context, current *soloistBackendConnection, action string, position int64, volume int) error {
	if ctx == nil || current == nil {
		return ErrSoloistBackendUnavailable
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.ctx.Done():
		return ErrSoloistBackendUnavailable
	case <-current.websocket.Done():
		return ErrSoloistDisconnected
	case <-current.ready:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.ctx.Done():
		return ErrSoloistBackendUnavailable
	case <-current.commandGate:
	}
	defer func() { current.commandGate <- struct{}{} }()
	if b.ctx.Err() != nil {
		return ErrSoloistBackendUnavailable
	}
	if b.currentConnection() != current {
		return ErrSoloistStaleSession
	}

	command := soloistBackendCommandName(action)
	wait, err := b.state.beginCommandAckWait(current.session, command, current.authRevision)
	if err != nil {
		return err
	}
	if err := sendSoloistBackendCommand(ctx, current.controller, action, position, volume); err != nil {
		b.state.cancelCommandAckWait(wait, err)
		if shouldRetireSoloistConnection(err) {
			b.retire(current)
		}
		return err
	}

	timer := time.NewTimer(b.ackTimeout)
	defer timer.Stop()
	select {
	case result := <-wait.result:
		if result != nil {
			b.retire(current)
		}
		return result
	case <-current.websocket.Done():
		b.state.cancelCommandAckWait(wait, ErrSoloistDisconnected)
		return ErrSoloistDisconnected
	case <-ctx.Done():
		b.state.cancelCommandAckWait(wait, ctx.Err())
		b.retire(current)
		return ctx.Err()
	case <-b.ctx.Done():
		b.state.cancelCommandAckWait(wait, ErrSoloistBackendUnavailable)
		b.retire(current)
		return ErrSoloistBackendUnavailable
	case <-timer.C:
		b.state.cancelCommandAckWait(wait, ErrSoloistBackendAckTimeout)
		b.retire(current)
		return ErrSoloistBackendAckTimeout
	}
}

func (b *SoloistBackend) reconnectLoop() {
	defer close(b.done)
	delay := soloistBackendReconnectInitial
	for b.ctx.Err() == nil {
		websocket, err := ConnectSoloistWebSocket(b.ctx, b.endpoint, b.state)
		if err != nil {
			if !b.waitReconnect(delay) {
				return
			}
			delay = nextSoloistReconnectDelay(delay)
			continue
		}
		connection := &soloistBackendConnection{
			websocket:   websocket,
			session:     websocket.Session(),
			commandGate: make(chan struct{}, 1),
			ready:       make(chan struct{}),
		}
		connection.commandGate <- struct{}{}
		b.setCurrent(connection)
		b.observeConnection(connection)
		b.clearCurrent(connection)
		_ = websocket.Close()
		if !b.waitReconnect(delay) {
			return
		}
		delay = nextSoloistReconnectDelay(delay)
	}
}

func (b *SoloistBackend) observeConnection(current *soloistBackendConnection) {
	if err := b.state.waitUntilAuthenticated(b.ctx, current.session); err != nil {
		return
	}
	authRevision, playbackRevision, errorRevision, err := b.state.queryRevisions(current.session)
	if err != nil {
		return
	}
	if err := current.websocket.RequestPlaybackState(b.ctx, current.session); err != nil {
		_ = current.websocket.Close()
		return
	}
	queryContext, cancel := context.WithTimeout(b.ctx, b.queryTimeout)
	err = b.state.waitForPlaybackStateAfter(queryContext, current.session, authRevision, playbackRevision, errorRevision)
	cancel()
	if err != nil {
		_ = current.websocket.Close()
		return
	}
	current.authRevision = authRevision
	current.controller = NewSoloistController(b.state, current.session, soloistRevisionBoundTransport{
		websocket:    current.websocket,
		authRevision: authRevision,
	})
	current.readyOnce.Do(func() { close(current.ready) })
	_ = b.state.waitForAuthRevisionChange(b.ctx, current.session, authRevision)
	_ = current.websocket.Close()
}

func (b *SoloistBackend) waitReconnect(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-b.ctx.Done():
		return false
	case <-timer.C:
		return b.ctx.Err() == nil
	}
}

func (b *SoloistBackend) currentConnection() *soloistBackendConnection {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.current
}

func (b *SoloistBackend) connectionReady(current *soloistBackendConnection) bool {
	if b == nil || current == nil || b.currentConnection() != current {
		return false
	}
	select {
	case <-current.ready:
	default:
		return false
	}
	authRevision, _, _, err := b.state.queryRevisions(current.session)
	return err == nil && authRevision == current.authRevision
}

func (b *SoloistBackend) setCurrent(current *soloistBackendConnection) {
	b.mu.Lock()
	if b.output != nil {
		b.output.Invalidate()
	}
	b.current = current
	b.mu.Unlock()
}

func (b *SoloistBackend) clearCurrent(current *soloistBackendConnection) {
	b.mu.Lock()
	if b.current == current {
		b.current = nil
		if b.output != nil {
			b.output.Invalidate()
		}
	}
	b.mu.Unlock()
}

func (b *SoloistBackend) retire(current *soloistBackendConnection) {
	b.mu.Lock()
	if b.current == current {
		b.current = nil
		if b.output != nil {
			b.output.Invalidate()
		}
	}
	b.mu.Unlock()
	_ = current.websocket.Close()
}

func nextSoloistReconnectDelay(current time.Duration) time.Duration {
	if current >= soloistBackendReconnectMaximum/2 {
		return soloistBackendReconnectMaximum
	}
	return current * 2
}

func validSoloistBackendAction(action string) bool {
	switch action {
	case "pause", "resume", "next", "prev", "seek", "volume", "stop":
		return true
	default:
		return false
	}
}

func soloistBackendCommandName(action string) string {
	switch action {
	case "pause", "stop":
		// stop intentionally maps to pause: Soloist exposes no finite stop
		// control in this worker slice, so observed playback remains event-owned.
		return "pause"
	case "resume":
		return "play"
	case "next":
		return "skip_next"
	case "prev":
		return "skip_prev"
	case "seek":
		return "seek"
	case "volume":
		return "set_volume"
	default:
		return ""
	}
}

func sendSoloistBackendCommand(ctx context.Context, controller *SoloistController, action string, position int64, volume int) error {
	if controller == nil {
		return ErrSoloistBackendUnavailable
	}
	switch action {
	case "pause", "stop":
		return controller.Pause(ctx)
	case "resume":
		return controller.Play(ctx)
	case "next":
		return controller.SkipNext(ctx)
	case "prev":
		return controller.SkipPrevious(ctx)
	case "seek":
		return controller.Seek(ctx, position)
	case "volume":
		return controller.SetVolume(ctx, volume)
	default:
		return ErrSoloistInvalidCommand
	}
}

func shouldRetireSoloistConnection(err error) bool {
	return errors.Is(err, ErrSoloistTransport) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func decodeSoloistBackendControl(body io.Reader, action string) (int64, int, error) {
	if body == nil {
		body = http.NoBody
	}
	data, err := io.ReadAll(io.LimitReader(body, maxSoloistBackendRequestBytes+1))
	if err != nil || len(data) > maxSoloistBackendRequestBytes {
		return 0, 0, ErrSoloistInvalidCommand
	}
	if len(data) == 0 {
		if action == "pause" || action == "resume" || action == "next" || action == "prev" || action == "stop" {
			return 0, 0, nil
		}
		return 0, 0, ErrSoloistInvalidCommand
	}
	if !json.Valid(data) || !soloistJSONHasUniqueKeys(data) {
		return 0, 0, ErrSoloistInvalidCommand
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return 0, 0, ErrSoloistInvalidCommand
	}
	switch action {
	case "pause", "resume", "next", "prev", "stop":
		if len(fields) != 0 {
			return 0, 0, ErrSoloistInvalidCommand
		}
	case "seek":
		raw, ok := fields["position"]
		if !ok || len(fields) != 1 || isSoloistNull(raw) {
			return 0, 0, ErrSoloistInvalidCommand
		}
		var position int64
		if json.Unmarshal(raw, &position) != nil || position < 0 || position > maxSoloistPositionMS {
			return 0, 0, ErrSoloistInvalidCommand
		}
		return position, 0, nil
	case "volume":
		raw, ok := fields["volume"]
		if !ok || len(fields) != 1 || isSoloistNull(raw) {
			return 0, 0, ErrSoloistInvalidCommand
		}
		var volume int
		if json.Unmarshal(raw, &volume) != nil || volume < 0 || volume > 100 {
			return 0, 0, ErrSoloistInvalidCommand
		}
		return 0, volume, nil
	default:
		return 0, 0, ErrSoloistInvalidCommand
	}
	return 0, 0, nil
}

func soloistBackendControlError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrSoloistInvalidCommand):
		return http.StatusBadRequest, "invalid command"
	case errors.Is(err, ErrSoloistBackendAckTimeout), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "command acknowledgment timeout"
	case errors.Is(err, ErrSoloistCommandRejected):
		return http.StatusBadGateway, "command rejected"
	case errors.Is(err, ErrSoloistStaleSession), errors.Is(err, ErrSoloistDisconnected), errors.Is(err, ErrSoloistUnauthenticated), errors.Is(err, ErrSoloistBackendUnavailable), errors.Is(err, ErrSoloistTransport), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, "worker unavailable"
	default:
		return http.StatusServiceUnavailable, "worker unavailable"
	}
}

func methodNotAllowed(writer http.ResponseWriter, method string) {
	writer.Header().Set("Allow", method)
	http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
}

func writeSoloistJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeSoloistJSONError(writer http.ResponseWriter, status int, message string) {
	writeSoloistJSON(writer, status, struct {
		Error string `json:"error"`
	}{Error: message})
}

// SpotifyBackendKind identifies which Spotify audio backend produced playable
// audio for the current worker process.
type SpotifyBackendKind string

const (
	SpotifyBackendGoLibrespot SpotifyBackendKind = "go-librespot"
	SpotifyBackendSoloist     SpotifyBackendKind = "soloist"
)

// ErrSpotifyBackendQualificationFailed is returned when neither backend
// reached playable audio during initial qualification.
var ErrSpotifyBackendQualificationFailed = errors.New("no Spotify backend produced playable audio")

// ErrSpotifyBackendQualificationPending means the first backend has passed
// session/auth checks but has not yet demonstrated playable output. It is a
// cold-start state, not a terminal backend failure, and the worker must keep
// the session command path open without publishing audio until output evidence
// appears.
var ErrSpotifyBackendQualificationPending = errors.New("Spotify backend awaiting playable output")

// SpotifyAudioQualifier proves a single backend can produce actual playable
// audio for the configured account before session audio is published.
// Success must mean observed/decrypted audio output, not merely OAuth,
// pairing, or account readiness: Spotify librespot issue #1649 shows an
// account can authenticate and still fail to obtain an audio key and decode
// the track.
type SpotifyAudioQualifier interface {
	QualifyPlayableAudio(ctx context.Context) error
}

// spotifyDaemonAudioQualifier uses the existing daemon health/status evidence and
// the live bridge diagnostic to distinguish auth/session readiness from actual
// playable-audio output. It never infers audio success from OAuth alone.
type spotifyDaemonAudioQualifier struct {
	client    *http.Client
	daemonURL string
	stateDir  string
	bridge    *spotifyBridge
}

func (q *spotifyDaemonAudioQualifier) QualifyPlayableAudio(ctx context.Context) error {
	if q == nil || q.client == nil || q.daemonURL == "" || q.stateDir == "" {
		return errors.New("go-librespot qualifier not configured")
	}
	health, err := spotifyHealth(ctx, q.client, q.daemonURL, q.stateDir)
	if err != nil {
		return err
	}
	if !health.Ready || health.AuthorizationRequired {
		return errors.New("Spotify session unavailable")
	}
	if q.bridge == nil {
		return errors.New("go-librespot audio bridge unavailable")
	}
	if diag := q.bridge.diagnostic(); !diag.Active || diag.EncodedBytes == 0 {
		return ErrSpotifyBackendQualificationPending
	}
	return nil
}

// SpotifyBackendQualificationError reports why each backend failed
// qualification so callers can distinguish auth-only failure from
// audio-key/decode/output failure instead of conflating them with readiness.
type SpotifyBackendQualificationError struct {
	GoLibrespotError error
	SoloistError     error
}

func (e *SpotifyBackendQualificationError) Error() string {
	return ErrSpotifyBackendQualificationFailed.Error()
}

func (e *SpotifyBackendQualificationError) Unwrap() error {
	return ErrSpotifyBackendQualificationFailed
}

// SpotifyBackendSelector chooses and pins exactly one Spotify backend for the
// lifetime of a single worker process. It always tries go-librespot first on
// a cold start. Soloist is attempted at most once, only during initial
// qualification before any session audio is published, and only if
// go-librespot fails to qualify. Once a backend is pinned, Select never
// re-qualifies or switches; the next cold start (a new selector instance)
// retries go-librespot first.
type SpotifyBackendSelector struct {
	goLibrespot SpotifyAudioQualifier
	soloist     SpotifyAudioQualifier

	mu         sync.Mutex
	pinned     SpotifyBackendKind
	pinnedOnce bool
}

// NewSpotifyBackendSelector builds a selector for a single worker process
// execution. Either qualifier may be nil, which is treated as an immediate
// qualification failure for that backend.
func NewSpotifyBackendSelector(goLibrespot, soloist SpotifyAudioQualifier) *SpotifyBackendSelector {
	return &SpotifyBackendSelector{goLibrespot: goLibrespot, soloist: soloist}
}

// Select qualifies go-librespot first; only on its failure does it attempt
// Soloist once. The first backend to reach playable audio is pinned for the
// remainder of this process. Once pinned, later calls return the pinned
// backend without re-qualifying either backend or switching away from an
// established session.
func (s *SpotifyBackendSelector) Select(ctx context.Context) (SpotifyBackendKind, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinnedOnce {
		return s.pinned, nil
	}

	goErr := errors.New("go-librespot qualifier not configured")
	if s.goLibrespot != nil {
		goErr = s.goLibrespot.QualifyPlayableAudio(ctx)
		if goErr == nil {
			s.pin(SpotifyBackendGoLibrespot)
			return s.pinned, nil
		}
		if errors.Is(goErr, ErrSpotifyBackendQualificationPending) {
			return "", ErrSpotifyBackendQualificationPending
		}
	}

	soloistErr := errors.New("Soloist qualifier not configured")
	if s.soloist != nil {
		soloistErr = s.soloist.QualifyPlayableAudio(ctx)
		if soloistErr == nil {
			s.pin(SpotifyBackendSoloist)
			return s.pinned, nil
		}
		if errors.Is(soloistErr, ErrSpotifyBackendQualificationPending) {
			return "", ErrSpotifyBackendQualificationPending
		}
	}

	return "", &SpotifyBackendQualificationError{GoLibrespotError: goErr, SoloistError: soloistErr}
}

// Pinned reports the backend pinned for this process lifetime, if any.
func (s *SpotifyBackendSelector) Pinned() (SpotifyBackendKind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinned, s.pinnedOnce
}

func (s *SpotifyBackendSelector) pin(kind SpotifyBackendKind) {
	s.pinned = kind
	s.pinnedOnce = true
}

// spotifyBackendSelectionState keeps the cold-start evaluation recoverable while
// the bridge remains idle and the current worker has not yet published audio.
// It does not allow a mid-session switch once a backend is pinned.
type spotifyBackendSelectionState struct {
	selector *SpotifyBackendSelector
	mu       sync.Mutex
	kind     SpotifyBackendKind
	err      error
}

func newSpotifyBackendSelectionState(goLibrespot, soloist SpotifyAudioQualifier) *spotifyBackendSelectionState {
	return &spotifyBackendSelectionState{selector: NewSpotifyBackendSelector(goLibrespot, soloist)}
}

func (s *spotifyBackendSelectionState) Resolve(ctx context.Context) (SpotifyBackendKind, error) {
	if s == nil || s.selector == nil {
		return "", ErrSpotifyBackendQualificationFailed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kind, err := s.selector.Select(ctx)
	s.kind, s.err = kind, err
	return kind, err
}
