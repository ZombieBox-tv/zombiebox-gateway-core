package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	maxAirPlayProgressLineBytes = 512
	maxAirPlayTrackDuration     = 24 * time.Hour
	maxAirPlayProgressAge       = 5 * time.Second
)

var airPlayProgressPattern = regexp.MustCompile(`^audio progress \(min:sec\):[ \t]*([0-9]{1,4}):([0-5][0-9]);[ \t]*remaining:[ \t]*([0-9]{1,4}):([0-5][0-9]);[ \t]*track length[ \t]+([0-9]{1,4}):([0-5][0-9])$`)

// AirPlayProgress accepts UxPlay stdout but retains only parsed position data.
// All other upstream output is discarded and is never logged or exposed.
type AirPlayProgress struct {
	mu sync.Mutex

	trackRevision string
	lineRevision  string
	line          []byte
	discardLine   bool
	sample        airPlayProgressSample
}

type airPlayProgressSample struct {
	trackRevision string
	positionMS    int64
	durationMS    int64
	reportedAt    time.Time
}

// AirPlayProgressSnapshot is the bounded, credential-free sender clock sample.
type AirPlayProgressSnapshot struct {
	Known      bool
	PositionMS int64
	DurationMS int64
	AgeMS      int64
}

func NewAirPlayProgress() *AirPlayProgress {
	return &AirPlayProgress{}
}

// Write implements io.Writer for UxPlay stdout. It recognizes carriage-return
// and newline-delimited progress records while keeping memory use bounded.
func (p *AirPlayProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, value := range data {
		if value == '\r' || value == '\n' {
			p.finishLine(time.Now())
			p.clearLine()
			p.lineRevision = ""
			p.discardLine = false
			continue
		}
		if p.discardLine {
			continue
		}
		if len(p.line) == 0 {
			p.lineRevision = p.trackRevision
		}
		if len(p.line) >= maxAirPlayProgressLineBytes {
			p.clearLine()
			p.lineRevision = ""
			p.discardLine = true
			continue
		}
		p.line = append(p.line, value)
	}
	return len(data), nil
}

// ObserveTrack fences progress at each confirmed connection or metadata
// revision. A delayed sample from the previous revision cannot cross the fence.
func (p *AirPlayProgress) ObserveTrack(revision string) {
	if len(revision) > 64 {
		revision = ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if revision == p.trackRevision {
		return
	}
	hadPartialLine := len(p.line) > 0 || p.discardLine
	p.clearLine()
	p.lineRevision = ""
	p.discardLine = hadPartialLine
	p.trackRevision = revision
	p.sample = airPlayProgressSample{}
}

func (p *AirPlayProgress) Snapshot(now time.Time) AirPlayProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	sample := p.sample
	if p.trackRevision == "" || sample.trackRevision != p.trackRevision || sample.reportedAt.IsZero() {
		return AirPlayProgressSnapshot{}
	}
	age := now.Sub(sample.reportedAt)
	if age < 0 || age > maxAirPlayProgressAge {
		return AirPlayProgressSnapshot{}
	}
	return AirPlayProgressSnapshot{
		Known:      true,
		PositionMS: sample.positionMS,
		DurationMS: sample.durationMS,
		AgeMS:      age.Milliseconds(),
	}
}

func (p *AirPlayProgress) finishLine(now time.Time) {
	if p.lineRevision == "" || p.lineRevision != p.trackRevision {
		return
	}
	positionMS, durationMS, ok := parseAirPlayProgress(p.line)
	if !ok {
		return
	}
	p.sample = airPlayProgressSample{
		trackRevision: p.lineRevision,
		positionMS:    positionMS,
		durationMS:    durationMS,
		reportedAt:    now,
	}
}

func (p *AirPlayProgress) clearLine() {
	for index := range p.line {
		p.line[index] = 0
	}
	p.line = p.line[:0]
}

func parseAirPlayProgress(line []byte) (int64, int64, bool) {
	fields := airPlayProgressPattern.FindSubmatch(line)
	if len(fields) != 7 {
		return 0, 0, false
	}
	values := [6]int64{}
	for index, field := range fields[1:] {
		value, err := strconv.ParseInt(string(field), 10, 32)
		if err != nil {
			return 0, 0, false
		}
		values[index] = value
	}
	positionSeconds := values[0]*60 + values[1]
	remainingSeconds := values[2]*60 + values[3]
	durationSeconds := values[4]*60 + values[5]
	if durationSeconds <= 0 || durationSeconds > int64(maxAirPlayTrackDuration/time.Second) || positionSeconds > durationSeconds || positionSeconds+remainingSeconds != durationSeconds {
		return 0, 0, false
	}
	return positionSeconds * 1000, durationSeconds * 1000, true
}

// airPlayProgressTrackRevision hashes only validated private identity material.
// Rewriting metadata for the same item must not invalidate its sender clock.
func airPlayProgressTrackRevision(metadata map[string]string, connectionRevision string) string {
	if metadata["title"] == "" || len(connectionRevision) != 16 {
		return ""
	}
	if _, err := hex.DecodeString(connectionRevision); err != nil {
		return ""
	}
	identity := metadata["title"] + "\x00" + metadata["artist"] + "\x00" + metadata["album"] + "\x00" + connectionRevision
	revision := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(revision[:])
}
