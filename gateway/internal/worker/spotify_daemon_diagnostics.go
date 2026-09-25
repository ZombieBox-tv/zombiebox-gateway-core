package worker

import (
	"net/http"
	"sync"
	"time"
)

const (
	spotifyStallThreshold         = 15 * time.Second
	spotifyMaxConsecutiveRefusals = 3
	spotifyCircuitCooldown        = 30 * time.Second
	spotifyMinStopInterval        = 1 * time.Second
)

var spotifyFailureNames = [...]string{
	"audioKeyRefused",
	"unsupportedMedia",
	"networkOrCdn",
	"decoder",
	"audioPipe",
	"trackLoad",
}

type spotifyFailurePattern struct {
	category int
	phrase   string
	prefix   []int
	matched  int
}

func newSpotifyFailurePattern(category int, phrase string) spotifyFailurePattern {
	p := spotifyFailurePattern{category: category, phrase: phrase, prefix: make([]int, len(phrase))}
	for i, matched := 1, 0; i < len(phrase); i++ {
		for matched > 0 && phrase[i] != phrase[matched] {
			matched = p.prefix[matched-1]
		}
		if phrase[i] == phrase[matched] {
			matched++
		}
		p.prefix[i] = matched
	}
	return p
}

// SpotifyDaemonDiagnostics is the daemon's stderr sink. It holds only fixed
// pattern progress and counters: no raw log line, track URI, account or token
// can be retrieved from it. Unknown output is discarded.
type SpotifyDaemonDiagnostics struct {
	mu                  sync.Mutex
	patterns            []spotifyFailurePattern
	lineCategories      [len(spotifyFailureNames)]bool
	counts              [len(spotifyFailureNames)]uint64
	lineBytes           int
	bufferingSince      time.Time
	consecutiveRefusals int
	refusalLimited      bool
	circuitOpenedAt     time.Time
	lastStopAttemptAt   time.Time
	stopAttempts        int
	lastStopResult      string
	lastStopHTTPStatus  int
	onRefusalLimit      func()
	clock               func() time.Time
}

func NewSpotifyDaemonDiagnostics() *SpotifyDaemonDiagnostics {
	return newSpotifyDaemonDiagnostics(time.Now)
}

func newSpotifyDaemonDiagnostics(clock func() time.Time) *SpotifyDaemonDiagnostics {
	d := &SpotifyDaemonDiagnostics{clock: clock}
	for _, entry := range []struct {
		category int
		phrase   string
	}{
		{0, "refused the audio key"},
		{0, "failed retrieving aes key with code"},
		{0, "failed retrieving audio key"},
		{0, "aeskeyerror"},
		{0, "failed requesting playplay license"},
		{0, "failed deobfuscating playplay key"},
		{1, "no supported formats"},
		{1, "media restricted"},
		{2, "failed resolving track storage"},
		{2, "connection refused"},
		{2, "i/o timeout"},
		{2, "network is unreachable"},
		{3, "decoder"},
		{3, "vorbis"},
		{3, "flac"},
		{4, "fifo"},
		{4, "broken pipe"},
		{4, "audio output"},
		{5, "failed loading current track"},
	} {
		d.patterns = append(d.patterns, newSpotifyFailurePattern(entry.category, entry.phrase))
	}
	return d
}

func (d *SpotifyDaemonDiagnostics) SetOnRefusalLimit(fn func()) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onRefusalLimit = fn
}

func (d *SpotifyDaemonDiagnostics) ResetRefusals() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.consecutiveRefusals = 0
	d.refusalLimited = false
	d.circuitOpenedAt = time.Time{}
}

func (d *SpotifyDaemonDiagnostics) RecordStopResult(statusCode int, errStr string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastStopHTTPStatus = statusCode
	if errStr != "" {
		d.lastStopResult = errStr
	} else {
		switch statusCode {
		case http.StatusOK:
			d.lastStopResult = "ok"
		case http.StatusNoContent:
			d.lastStopResult = "no_session"
		default:
			d.lastStopResult = "http_error"
		}
	}
}

func (d *SpotifyDaemonDiagnostics) Write(input []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, value := range input {
		if value == '\n' {
			d.finishLine()
			continue
		}
		// Bound CPU for hostile or malformed lines while still draining stderr.
		if d.lineBytes >= 4096 {
			continue
		}
		d.lineBytes++
		if value >= 'A' && value <= 'Z' {
			value += 'a' - 'A'
		}
		for i := range d.patterns {
			p := &d.patterns[i]
			for p.matched > 0 && value != p.phrase[p.matched] {
				p.matched = p.prefix[p.matched-1]
			}
			if value == p.phrase[p.matched] {
				p.matched++
			}
			if p.matched == len(p.phrase) {
				d.lineCategories[p.category] = true
				p.matched = p.prefix[p.matched-1]
			}
		}
	}
	return len(input), nil
}

func (d *SpotifyDaemonDiagnostics) finishLine() {
	var onLimit func()
	keyRefused := d.lineCategories[0]
	for i, matched := range d.lineCategories {
		if matched && d.counts[i] < ^uint64(0) {
			d.counts[i]++
		}
		d.lineCategories[i] = false
	}
	if keyRefused {
		now := d.clock()
		d.consecutiveRefusals++
		if d.refusalLimited {
			// Circuit breaker is already open. If key refusals continue to arrive
			// (from overlapping in-flight loads or an auto-reconnect cascade),
			// re-assert stop if paced interval has elapsed to prevent an unbounded cascade.
			if d.onRefusalLimit != nil && (d.lastStopAttemptAt.IsZero() || now.Sub(d.lastStopAttemptAt) >= spotifyMinStopInterval) {
				d.lastStopAttemptAt = now
				d.stopAttempts++
				onLimit = d.onRefusalLimit
			}
		} else if d.consecutiveRefusals >= spotifyMaxConsecutiveRefusals {
			d.refusalLimited = true
			d.circuitOpenedAt = now
			d.lastStopAttemptAt = now
			d.stopAttempts++
			if d.onRefusalLimit != nil {
				onLimit = d.onRefusalLimit
			}
		}
	}
	for i := range d.patterns {
		d.patterns[i].matched = 0
	}
	d.lineBytes = 0
	if onLimit != nil {
		go onLimit()
	}
}

type spotifyDaemonHealth struct {
	FailureCounts       map[string]uint64 `json:"failureCounts"`
	StalledBuffering    bool              `json:"stalledBuffering"`
	RefusalLimited      bool              `json:"refusalLimited,omitempty"`
	ConsecutiveRefusals int               `json:"consecutiveRefusals,omitempty"`
	StopAttempts        int               `json:"stopAttempts,omitempty"`
	LastStopResult      string            `json:"lastStopResult,omitempty"`
	LastStopStatus      int               `json:"lastStopStatus,omitempty"`
}

// An active attempt is evaluated against the refusal circuit. Reconnects during
// cooldown keep the circuit open; later or explicitly requested attempts may retry.
func (d *SpotifyDaemonDiagnostics) observePlayback(stopped, bufferingWithoutTrack, audioActive bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock()
	if !stopped && d.refusalLimited {
		// Playback transitioned from stopped to active while refusal limited.
		// Only re-arm if the circuit cooldown has elapsed, indicating an intentional
		// later retry rather than an immediate automatic reconnect cascade.
		if !d.circuitOpenedAt.IsZero() && now.Sub(d.circuitOpenedAt) >= spotifyCircuitCooldown {
			d.consecutiveRefusals = 0
			d.refusalLimited = false
			d.circuitOpenedAt = time.Time{}
		}
	}
	d.observeLocked(bufferingWithoutTrack, audioActive)
}

func (d *SpotifyDaemonDiagnostics) observeLocked(bufferingWithoutTrack, audioActive bool) {
	now := d.clock()
	if !bufferingWithoutTrack || audioActive {
		d.bufferingSince = time.Time{}
		if audioActive {
			d.consecutiveRefusals = 0
			d.refusalLimited = false
			d.circuitOpenedAt = time.Time{}
			// Real recovery: clear cumulative failure counters so stale errors do not linger.
			for i := range d.counts {
				d.counts[i] = 0
			}
		}
	} else if d.bufferingSince.IsZero() {
		d.bufferingSince = now
	}
}

func (d *SpotifyDaemonDiagnostics) snapshot(bufferingWithoutTrack, audioActive bool) *spotifyDaemonHealth {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observeLocked(bufferingWithoutTrack, audioActive)
	now := d.clock()
	out := &spotifyDaemonHealth{
		FailureCounts:       make(map[string]uint64, len(spotifyFailureNames)),
		StalledBuffering:    !d.bufferingSince.IsZero() && now.Sub(d.bufferingSince) >= spotifyStallThreshold,
		RefusalLimited:      d.refusalLimited,
		ConsecutiveRefusals: d.consecutiveRefusals,
		StopAttempts:        d.stopAttempts,
		LastStopResult:      d.lastStopResult,
		LastStopStatus:      d.lastStopHTTPStatus,
	}
	for i, name := range spotifyFailureNames {
		out.FailureCounts[name] = d.counts[i]
	}
	return out
}
