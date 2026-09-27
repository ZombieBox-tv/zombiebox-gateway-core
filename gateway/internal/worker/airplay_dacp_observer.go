package worker

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

const (
	airplayDACPObserverPollInterval = 100 * time.Millisecond
	airplayDACPProbeInterval        = 4 * time.Second
	airplayDACPProbeWindow          = 20 * time.Second
	airplayDACPProbeLimit           = 5
	airplayDACPDiagnosticAgeLimit   = 24 * time.Hour
)

// AirPlayDACPDiagnosticStatus contains only bounded counters and fixed stages.
// It never includes DACP credentials, addresses, response data, or metadata.
type AirPlayDACPDiagnosticStatus struct {
	Enabled               bool   `json:"enabled"`
	Stage                 string `json:"stage"`
	Generation            uint64 `json:"generation,omitempty"`
	RequestCount          uint8  `json:"requestCount"`
	SuccessCount          uint8  `json:"successCount"`
	FailureCount          uint8  `json:"failureCount"`
	StaleResponseCount    uint8  `json:"staleResponseCount"`
	SessionChangeCount    uint8  `json:"sessionChangeCount"`
	LastOutcome           string `json:"lastOutcome,omitempty"`
	LastResponseAgeMS     int64  `json:"lastResponseAgeMs,omitempty"`
	LastRequestDurationMS int64  `json:"lastRequestDurationMs,omitempty"`
}

// AirPlayDACPDiagnostics supplies a sanitized snapshot for the authenticated
// private worker status route.
type AirPlayDACPDiagnostics interface {
	AirPlayDACPDiagnosticSnapshot(time.Time) AirPlayDACPDiagnosticStatus
}

type dacpStatusProbe func(context.Context) (time.Time, error)

// AirPlayDACPObserver performs a small, diagnostic-only number of read-only
// status observations for each receiver.dacp credential-file generation.
type AirPlayDACPObserver struct {
	controller *DACPController
	probe      dacpStatusProbe

	mu             sync.Mutex
	status         AirPlayDACPDiagnosticStatus
	lastResponseAt time.Time
}

// NewAirPlayDACPObserver constructs the opt-in diagnostic observer. Call Run
// only when the process-level diagnostic opt-in is enabled.
func NewAirPlayDACPObserver(controller *DACPController) *AirPlayDACPObserver {
	observer := &AirPlayDACPObserver{controller: controller}
	if controller != nil {
		observer.probe = controller.ObserveStatus
	}
	observer.status = AirPlayDACPDiagnosticStatus{Enabled: true, Stage: "waiting_for_session"}
	return observer
}

func newAirPlayDACPObserverWithProbe(probe dacpStatusProbe) *AirPlayDACPObserver {
	observer := &AirPlayDACPObserver{
		probe:  probe,
		status: AirPlayDACPDiagnosticStatus{Enabled: true, Stage: "waiting_for_session"},
	}
	return observer
}

// Run watches the private DACP credential file for receiver-session changes.
// It starts at most five requests per generation in a 20-second window, with
// at least four seconds between completed requests. Generation changes cancel
// an in-flight request; the next generation is not probed until it has ended.
func (o *AirPlayDACPObserver) Run(ctx context.Context) {
	if o == nil || o.controller == nil || o.probe == nil {
		return
	}
	tracker := newDACPStatusSessionTracker(o.controller.credentialPath)
	o.run(ctx, tracker.observe, airplayDACPObserverPollInterval, airplayDACPProbeInterval, airplayDACPProbeWindow)
}

type dacpStatusSessionTracker struct {
	path        string
	active      bool
	fingerprint [32]byte
	generation  uint64
}

func newDACPStatusSessionTracker(path string) *dacpStatusSessionTracker {
	return &dacpStatusSessionTracker{path: path}
}

func (t *dacpStatusSessionTracker) observe() uint64 {
	snapshot, err := readDACPCredentialSnapshot(t.path)
	if err != nil {
		t.active = false
		return 0
	}
	if !t.active || snapshot.fingerprint != t.fingerprint {
		if t.generation < math.MaxUint64 {
			t.generation++
		}
		t.fingerprint = snapshot.fingerprint
		t.active = true
	}
	return t.generation
}

type dacpStatusProbeResult struct {
	generation uint64
	startedAt  time.Time
	receivedAt time.Time
	err        error
}

func (o *AirPlayDACPObserver) run(
	ctx context.Context,
	sessionGeneration func() uint64,
	pollInterval time.Duration,
	probeInterval time.Duration,
	probeWindow time.Duration,
) {
	if o == nil || o.probe == nil || sessionGeneration == nil || pollInterval <= 0 || probeInterval <= 0 || probeWindow <= 0 {
		return
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	// There is at most one active request; the buffered slot lets it finish
	// without blocking if Run exits on process cancellation first.
	results := make(chan dacpStatusProbeResult, 1)

	var generation uint64
	var sessionStarted time.Time
	var nextProbeAt time.Time
	var probesInGeneration uint8
	var inFlight bool
	var inFlightGeneration uint64
	var cancelInFlight context.CancelFunc

	setGeneration := func(next uint64, now time.Time) {
		if next == generation {
			return
		}
		if cancelInFlight != nil {
			cancelInFlight()
		}
		previous := generation
		generation = next
		sessionStarted = now
		nextProbeAt = now
		probesInGeneration = 0
		o.mu.Lock()
		if previous != 0 || next != 0 {
			o.status.SessionChangeCount = incrementUint8(o.status.SessionChangeCount)
		}
		o.status.Generation = next
		o.status.Stage = "waiting_for_session"
		if next != 0 {
			o.status.Stage = "session_ready"
		}
		o.status.LastOutcome = ""
		o.status.LastRequestDurationMS = 0
		o.status.LastResponseAgeMS = 0
		o.lastResponseAt = time.Time{}
		o.mu.Unlock()
	}

	for {
		if err := ctx.Err(); err != nil {
			if cancelInFlight != nil {
				cancelInFlight()
			}
			o.setStopped(generation)
			return
		}
		now := time.Now()
		setGeneration(sessionGeneration(), now)

		if generation != 0 && !inFlight && probesInGeneration < airplayDACPProbeLimit {
			if now.Sub(sessionStarted) >= probeWindow {
				o.setStage(generation, "window_expired")
			} else if !now.Before(nextProbeAt) {
				requestCtx, cancel := context.WithTimeout(ctx, dacpRequestTimeout)
				inFlight = true
				inFlightGeneration = generation
				cancelInFlight = cancel
				probesInGeneration++
				o.recordRequest(generation)
				startedAt := now
				go func(requestGeneration uint64) {
					receivedAt, err := o.probe(requestCtx)
					results <- dacpStatusProbeResult{
						generation: requestGeneration,
						startedAt:  startedAt,
						receivedAt: receivedAt,
						err:        err,
					}
				}(generation)
				o.setStage(generation, "requesting")
			}
		} else if generation != 0 && !inFlight && probesInGeneration >= airplayDACPProbeLimit {
			o.setStage(generation, "window_complete")
		}

		select {
		case <-ctx.Done():
			if cancelInFlight != nil {
				cancelInFlight()
			}
			o.setStopped(generation)
			return
		case result := <-results:
			currentGeneration := sessionGeneration()
			setGeneration(currentGeneration, time.Now())
			if inFlight && result.generation == inFlightGeneration {
				inFlight = false
				inFlightGeneration = 0
				if cancelInFlight != nil {
					cancelInFlight()
					cancelInFlight = nil
				}
			}
			if result.generation != currentGeneration {
				if !result.receivedAt.IsZero() {
					o.recordStaleResponse()
				}
				continue
			}
			finishedAt := time.Now()
			nextProbeAt = finishedAt.Add(probeInterval)
			o.recordResult(result, finishedAt)
		case <-ticker.C:
		}
	}
}

func (o *AirPlayDACPObserver) recordRequest(generation uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.status.Generation != generation {
		return
	}
	o.status.RequestCount = incrementUint8(o.status.RequestCount)
}

func (o *AirPlayDACPObserver) recordStaleResponse() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.status.StaleResponseCount = incrementUint8(o.status.StaleResponseCount)
	if o.status.Generation != 0 {
		o.status.Stage = "stale_response_discarded"
		o.status.LastOutcome = "session_changed"
	}
}

func (o *AirPlayDACPObserver) recordResult(result dacpStatusProbeResult, finishedAt time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.status.Generation != result.generation {
		return
	}
	o.status.LastRequestDurationMS = boundedDiagnosticAgeMS(finishedAt.Sub(result.startedAt))
	if result.err != nil {
		if errors.Is(result.err, context.Canceled) && result.receivedAt.IsZero() {
			o.status.Stage = "request_cancelled"
			o.status.LastOutcome = "cancelled"
			return
		}
		o.status.FailureCount = incrementUint8(o.status.FailureCount)
		o.status.Stage = "probe_failed"
		o.status.LastOutcome = dacpDiagnosticErrorClass(result.err)
		return
	}
	if result.receivedAt.IsZero() {
		o.status.FailureCount = incrementUint8(o.status.FailureCount)
		o.status.Stage = "probe_failed"
		o.status.LastOutcome = "invalid_completion"
		return
	}
	o.status.SuccessCount = incrementUint8(o.status.SuccessCount)
	o.status.Stage = "response_received"
	o.status.LastOutcome = "received"
	o.lastResponseAt = result.receivedAt
	o.status.LastResponseAgeMS = 0
}

func (o *AirPlayDACPObserver) setStage(generation uint64, stage string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.status.Generation == generation {
		o.status.Stage = stage
	}
}

func (o *AirPlayDACPObserver) setStopped(generation uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.status.Generation == generation {
		o.status.Stage = "stopped"
	}
}

func (o *AirPlayDACPObserver) AirPlayDACPDiagnosticSnapshot(now time.Time) AirPlayDACPDiagnosticStatus {
	if o == nil {
		return AirPlayDACPDiagnosticStatus{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	status := o.status
	if !o.lastResponseAt.IsZero() {
		status.LastResponseAgeMS = boundedDiagnosticAgeMS(now.Sub(o.lastResponseAt))
	}
	return status
}

func dacpDiagnosticErrorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, errInvalidDACPFile), errors.Is(err, errDACPUnavailable):
		return "receiver_unavailable"
	case errors.Is(err, errDACPChanged):
		return "session_changed"
	case errors.Is(err, errDACPResponse):
		return "response_rejected"
	default:
		return "request_failed"
	}
}

func incrementUint8(value uint8) uint8 {
	if value < math.MaxUint8 {
		return value + 1
	}
	return value
}

func boundedDiagnosticAgeMS(age time.Duration) int64 {
	if age < 0 {
		return 0
	}
	if age > airplayDACPDiagnosticAgeLimit {
		return airplayDACPDiagnosticAgeLimit.Milliseconds()
	}
	return age.Milliseconds()
}
