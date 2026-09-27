package main

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/worker"
)

const (
	airplayAudioHLSTuningEnv     = "ZOMBIE_AIRPLAY_AUDIO_HLS_QA_PROFILE"
	airplayAudioRTPDiagnosticEnv = "ZOMBIE_AIRPLAY_AUDIO_RTP_DIAGNOSTIC"
	airplayAudioRTPPort          = 35016
	airplayAudioSequenceWindow   = 128
	airplayAudioMaxSeqAdvance    = 4096
	airplayAudioLongGap          = 250 * time.Millisecond
	airplayAudioMaxManifest      = 64 << 10
	airplayAudioMaxSegments      = 16
	airplayAudioMaxSegmentSec    = 60.0
	airplayAudioMaxAge           = 24 * time.Hour
	airplayAudioMaxRequest       = 30 * time.Second
)

var airplayAudioSegmentName = regexp.MustCompile(`^audio[0-9]+\.ts$`)

type airplayAudioHLSTuning struct {
	segmentDuration string
	listSize        string
}

func airplayAudioHLSTuningForProfile(profile string) (airplayAudioHLSTuning, error) {
	switch profile {
	case "", "default":
		return airplayAudioHLSTuning{segmentDuration: "0.6", listSize: "4"}, nil
	case "1s-4":
		return airplayAudioHLSTuning{segmentDuration: "1", listSize: "4"}, nil
	case "1s-10":
		return airplayAudioHLSTuning{segmentDuration: "1", listSize: "10"}, nil
	default:
		return airplayAudioHLSTuning{}, errors.New("invalid AirPlay audio HLS QA profile")
	}
}

func airplayAudioHLSTuningFromEnv() (airplayAudioHLSTuning, error) {
	return airplayAudioHLSTuningForProfile(os.Getenv(airplayAudioHLSTuningEnv))
}

func airplayAudioRTPDiagnosticEnabled(value string) (bool, error) {
	switch value {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New("invalid AirPlay audio RTP diagnostic flag")
	}
}

func airplayAudioRTPDiagnosticEnabledFromEnv() (bool, error) {
	return airplayAudioRTPDiagnosticEnabled(os.Getenv(airplayAudioRTPDiagnosticEnv))
}

func airplayAudioRTPTargets(includeDiagnosticListener bool) string {
	targets := "127.0.0.1:35012,127.0.0.1:35014"
	if includeDiagnosticListener {
		targets += ",127.0.0.1:" + strconv.Itoa(airplayAudioRTPPort)
	}
	return targets
}

type airplayAudioDiagnostics struct {
	mu sync.Mutex

	stateDir string
	rtp      airplayAudioRTPState

	listener        *net.UDPConn
	listenerDone    chan struct{}
	listenerActive  bool
	playlistRequest airplayAudioRequestState
	segmentRequest  airplayAudioRequestState
}

// Sequence counters are process-scoped. No SSRC or sender identity is kept;
// jumps over 4096 packets either way reset the bounded sequence window, so a
// reset count is a discontinuity heuristic rather than proof of reconnect.
// Reorder recovery is limited to the most recent 128 sequence positions.
type airplayAudioRTPState struct {
	packetCount          uint64
	malformedPacketCount uint64
	sequenceGapCount     uint64
	recoveredGapCount    uint64
	outstandingGapCount  uint64
	expiredGapCount      uint64
	duplicateCount       uint64
	reorderedCount       uint64
	oldPacketCount       uint64
	sequenceResetCount   uint64
	arrivalGapCount      uint64
	longArrivalGapCount  uint64
	lastArrivalGap       time.Duration
	maxArrivalGap        time.Duration
	lastArrival          time.Time
	lastPacket           time.Time
	hasSequence          bool
	highestSequence      uint16
	seen                 [airplayAudioSequenceWindow]bool
	missing              [airplayAudioSequenceWindow]bool
}

type airplayAudioRequestState struct {
	requestCount  uint64
	successCount  uint64
	missCount     uint64
	errorCount    uint64
	lastStatus    int
	lastDuration  time.Duration
	lastRequestAt time.Time
}

func newAirplayAudioDiagnostics(stateDir string) *airplayAudioDiagnostics {
	return &airplayAudioDiagnostics{stateDir: stateDir}
}

// StartRTPListener binds only to loopback and discards each received packet
// after parsing its RTP header. The listener does not retain payloads, SSRCs,
// addresses, or any sender metadata.
func (d *airplayAudioDiagnostics) StartRTPListener(ctx context.Context, address string) error {
	if ctx == nil {
		return errors.New("AirPlay RTP listener requires a context")
	}
	udpAddress, err := net.ResolveUDPAddr("udp4", address)
	if err != nil || udpAddress.IP == nil || udpAddress.IP.To4() == nil || !udpAddress.IP.IsLoopback() {
		return errors.New("AirPlay RTP diagnostics must bind to loopback")
	}
	conn, err := net.ListenUDP("udp4", udpAddress)
	if err != nil {
		return err
	}
	_ = conn.SetReadBuffer(1 << 20)

	d.mu.Lock()
	if d.listener != nil {
		d.mu.Unlock()
		_ = conn.Close()
		return errors.New("AirPlay RTP diagnostics listener already started")
	}
	d.listener = conn
	d.listenerDone = make(chan struct{})
	d.listenerActive = true
	done := d.listenerDone
	d.mu.Unlock()

	stopContextClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	go func() {
		defer close(done)
		defer stopContextClose()
		defer func() {
			d.mu.Lock()
			d.listenerActive = false
			d.mu.Unlock()
		}()

		buffer := make([]byte, 64<<10)
		for {
			count, _, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			d.ObserveRTPPacket(buffer[:count], time.Now())
		}
	}()
	return nil
}

func (d *airplayAudioDiagnostics) Close() error {
	d.mu.Lock()
	conn := d.listener
	done := d.listenerDone
	d.mu.Unlock()
	if conn == nil {
		return nil
	}
	err := conn.Close()
	if done != nil {
		<-done
	}
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// ObserveRTPPacket retains only the validated 16-bit RTP sequence number and
// bounded timing/sequence counters. Invalid headers increment one counter.
func (d *airplayAudioDiagnostics) ObserveRTPPacket(packet []byte, receivedAt time.Time) {
	sequence, valid := airplayRTPSequenceNumber(packet)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !valid {
		d.rtp.malformedPacketCount = saturatingAdd(d.rtp.malformedPacketCount, 1)
		return
	}

	if !receivedAt.IsZero() {
		if !d.rtp.lastArrival.IsZero() {
			gap := receivedAt.Sub(d.rtp.lastArrival)
			if gap < 0 {
				gap = 0
			}
			d.rtp.arrivalGapCount = saturatingAdd(d.rtp.arrivalGapCount, 1)
			d.rtp.lastArrivalGap = gap
			if gap > d.rtp.maxArrivalGap {
				d.rtp.maxArrivalGap = gap
			}
			// 250 ms is a diagnostic stall threshold; all arrival intervals are
			// also counted and the last and maximum intervals are retained.
			if gap >= airplayAudioLongGap {
				d.rtp.longArrivalGapCount = saturatingAdd(d.rtp.longArrivalGapCount, 1)
			}
		}
		d.rtp.lastArrival = receivedAt
		d.rtp.lastPacket = receivedAt
	}
	d.rtp.packetCount = saturatingAdd(d.rtp.packetCount, 1)
	d.observeSequenceLocked(sequence)
}

func (d *airplayAudioDiagnostics) observeSequenceLocked(sequence uint16) {
	state := &d.rtp
	if !state.hasSequence {
		state.hasSequence = true
		state.highestSequence = sequence
		state.seen[0] = true
		return
	}

	delta := int16(sequence - state.highestSequence)
	if delta == 0 {
		state.duplicateCount = saturatingAdd(state.duplicateCount, 1)
		return
	}
	if delta > 0 {
		advance := int(delta)
		if advance > airplayAudioMaxSeqAdvance {
			d.resetSequenceLocked(sequence)
			return
		}

		shift := advance
		if shift > airplayAudioSequenceWindow {
			shift = airplayAudioSequenceWindow
		}
		for age := airplayAudioSequenceWindow - shift; age < airplayAudioSequenceWindow; age++ {
			if state.missing[age] {
				decrementIfPositive(&state.outstandingGapCount)
				state.expiredGapCount = saturatingAdd(state.expiredGapCount, 1)
			}
		}

		var nextSeen, nextMissing [airplayAudioSequenceWindow]bool
		if advance < airplayAudioSequenceWindow {
			for age := advance; age < airplayAudioSequenceWindow; age++ {
				nextSeen[age] = state.seen[age-advance]
				nextMissing[age] = state.missing[age-advance]
			}
		}
		state.seen = nextSeen
		state.missing = nextMissing

		newGapCount := advance - 1
		state.sequenceGapCount = saturatingAdd(state.sequenceGapCount, uint64(newGapCount))
		trackedGaps := newGapCount
		if trackedGaps >= airplayAudioSequenceWindow {
			trackedGaps = airplayAudioSequenceWindow - 1
		}
		for age := 1; age <= trackedGaps; age++ {
			state.missing[age] = true
		}
		state.outstandingGapCount = saturatingAdd(state.outstandingGapCount, uint64(trackedGaps))
		state.expiredGapCount = saturatingAdd(state.expiredGapCount, uint64(newGapCount-trackedGaps))
		state.highestSequence = sequence
		state.seen[0] = true
		return
	}

	age := -int(delta)
	if age > airplayAudioMaxSeqAdvance {
		// RTP sequence numbers alone cannot disambiguate the exact half-range
		// jump or a larger backward discontinuity. Reset the bounded tracker
		// instead of treating that packet as permanently old.
		d.resetSequenceLocked(sequence)
		return
	}
	if age >= airplayAudioSequenceWindow {
		state.oldPacketCount = saturatingAdd(state.oldPacketCount, 1)
		return
	}
	if state.seen[age] {
		state.duplicateCount = saturatingAdd(state.duplicateCount, 1)
		return
	}
	state.seen[age] = true
	state.reorderedCount = saturatingAdd(state.reorderedCount, 1)
	if state.missing[age] {
		state.missing[age] = false
		decrementIfPositive(&state.outstandingGapCount)
		state.recoveredGapCount = saturatingAdd(state.recoveredGapCount, 1)
	}
}

func (d *airplayAudioDiagnostics) resetSequenceLocked(sequence uint16) {
	state := &d.rtp
	state.sequenceResetCount = saturatingAdd(state.sequenceResetCount, 1)
	state.expiredGapCount = saturatingAdd(state.expiredGapCount, state.outstandingGapCount)
	state.outstandingGapCount = 0
	state.seen = [airplayAudioSequenceWindow]bool{}
	state.missing = [airplayAudioSequenceWindow]bool{}
	state.highestSequence = sequence
	state.hasSequence = true
	state.seen[0] = true
}

func airplayRTPSequenceNumber(packet []byte) (uint16, bool) {
	if len(packet) < 12 || packet[0]>>6 != 2 {
		return 0, false
	}
	headerLength := 12 + int(packet[0]&0x0f)*4
	if headerLength > len(packet) {
		return 0, false
	}
	if packet[0]&0x10 != 0 {
		if len(packet)-headerLength < 4 {
			return 0, false
		}
		extensionWords := int(packet[headerLength+2])<<8 | int(packet[headerLength+3])
		headerLength += 4 + extensionWords*4
		if headerLength > len(packet) {
			return 0, false
		}
	}
	if packet[0]&0x20 != 0 {
		padding := int(packet[len(packet)-1])
		if padding == 0 || padding > len(packet)-headerLength {
			return 0, false
		}
	}
	return uint16(packet[2])<<8 | uint16(packet[3]), true
}

func (d *airplayAudioDiagnostics) RecordAirPlayHLSRequest(kind worker.AirPlayAudioRequestKind, statusCode int, duration time.Duration, observedAt time.Time) {
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if statusCode < 100 || statusCode > 599 {
		statusCode = 0
	}
	duration = clampDuration(duration, airplayAudioMaxRequest)
	d.mu.Lock()
	defer d.mu.Unlock()

	var state *airplayAudioRequestState
	switch kind {
	case worker.AirPlayAudioPlaylistRequest:
		state = &d.playlistRequest
	case worker.AirPlayAudioSegmentRequest:
		state = &d.segmentRequest
	default:
		return
	}
	state.requestCount = saturatingAdd(state.requestCount, 1)
	if statusCode >= 200 && statusCode < 300 {
		state.successCount = saturatingAdd(state.successCount, 1)
	} else if statusCode == httpStatusNotFound || statusCode == httpStatusGone {
		state.missCount = saturatingAdd(state.missCount, 1)
	} else {
		state.errorCount = saturatingAdd(state.errorCount, 1)
	}
	state.lastStatus = statusCode
	state.lastDuration = duration
	state.lastRequestAt = observedAt
}

const (
	httpStatusNotFound = 404
	httpStatusGone     = 410
)

func (d *airplayAudioDiagnostics) AirPlayAudioSnapshot(now time.Time) worker.AirPlayAudioStatus {
	d.mu.Lock()
	rtp := d.rtp
	listenerAvailable := d.listenerActive
	playlistRequest := d.playlistRequest
	segmentRequest := d.segmentRequest
	d.mu.Unlock()

	status := worker.AirPlayAudioStatus{
		RTPListenerAvailable:             listenerAvailable,
		RTPPacketCount:                   rtp.packetCount,
		RTPMalformedPacketCount:          rtp.malformedPacketCount,
		RTPSequenceGapCount:              rtp.sequenceGapCount,
		RTPRecoveredSequenceGapCount:     rtp.recoveredGapCount,
		RTPOutstandingSequenceGapCount:   rtp.outstandingGapCount,
		RTPExpiredSequenceGapCount:       rtp.expiredGapCount,
		RTPDuplicatePacketCount:          rtp.duplicateCount,
		RTPReorderedPacketCount:          rtp.reorderedCount,
		RTPOldPacketCount:                rtp.oldPacketCount,
		RTPSequenceResetCount:            rtp.sequenceResetCount,
		RTPArrivalGapCount:               rtp.arrivalGapCount,
		RTPLongArrivalGapCount:           rtp.longArrivalGapCount,
		RTPLastArrivalGapMS:              boundedMilliseconds(rtp.lastArrivalGap),
		RTPMaxArrivalGapMS:               boundedMilliseconds(rtp.maxArrivalGap),
		HLSPlaylistRequestCount:          playlistRequest.requestCount,
		HLSPlaylistSuccessCount:          playlistRequest.successCount,
		HLSPlaylistMissCount:             playlistRequest.missCount,
		HLSPlaylistOtherErrorCount:       playlistRequest.errorCount,
		HLSPlaylistLastStatusCode:        playlistRequest.lastStatus,
		HLSPlaylistLastRequestDurationMS: boundedMilliseconds(playlistRequest.lastDuration),
		HLSSegmentRequestCount:           segmentRequest.requestCount,
		HLSSegmentSuccessCount:           segmentRequest.successCount,
		HLSSegmentMissCount:              segmentRequest.missCount,
		HLSSegmentOtherErrorCount:        segmentRequest.errorCount,
		HLSSegmentLastStatusCode:         segmentRequest.lastStatus,
		HLSSegmentLastRequestDurationMS:  boundedMilliseconds(segmentRequest.lastDuration),
	}
	if !rtp.lastPacket.IsZero() {
		status.RTPLastPacketAgeMS = boundedMilliseconds(clampDuration(now.Sub(rtp.lastPacket), airplayAudioMaxAge))
	}
	if rtp.packetCount > 0 {
		status.RTPMaxArrivalGapMS = boundedMilliseconds(rtp.maxArrivalGap)
	}
	if !playlistRequest.lastRequestAt.IsZero() {
		status.HLSPlaylistLastRequestAgeMS = boundedMilliseconds(clampDuration(now.Sub(playlistRequest.lastRequestAt), airplayAudioMaxAge))
	}
	if !segmentRequest.lastRequestAt.IsZero() {
		status.HLSSegmentLastRequestAgeMS = boundedMilliseconds(clampDuration(now.Sub(segmentRequest.lastRequestAt), airplayAudioMaxAge))
	}
	mergeAirplayAudioHLSStatus(&status, inspectAirplayAudioHLS(d.stateDir, now))
	return status
}

type airplayAudioHLSState struct {
	present             bool
	valid               bool
	age                 time.Duration
	segmentCount        uint16
	missingSegmentCount uint16
	span                time.Duration
	newestSegmentAge    time.Duration
	hasNewestSegment    bool
}

func inspectAirplayAudioHLS(stateDir string, now time.Time) airplayAudioHLSState {
	state := airplayAudioHLSState{}
	manifestPath := filepath.Join(stateDir, "hls", "audio.m3u8")
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return state
	}
	state.present = true
	state.age = clampDuration(now.Sub(info.ModTime()), airplayAudioMaxAge)
	if !info.Mode().IsRegular() || info.Size() > airplayAudioMaxManifest {
		return state
	}
	file, err := os.Open(manifestPath)
	if err != nil {
		return state
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() > airplayAudioMaxManifest {
		return state
	}
	content, err := io.ReadAll(io.LimitReader(file, airplayAudioMaxManifest+1))
	if err != nil || len(content) == 0 || len(content) > airplayAudioMaxManifest {
		return state
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) == 0 || strings.TrimSpace(strings.TrimSuffix(lines[0], "\r")) != "#EXTM3U" {
		return state
	}

	valid := true
	hasPendingDuration := false
	pendingDuration := 0.0
	segmentCount := 0
	var span time.Duration
	for _, rawLine := range lines[1:] {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if hasPendingDuration {
				valid = false
			}
			durationText := strings.TrimPrefix(line, "#EXTINF:")
			if comma := strings.IndexByte(durationText, ','); comma >= 0 {
				durationText = durationText[:comma]
			}
			durationSeconds, parseErr := strconv.ParseFloat(strings.TrimSpace(durationText), 64)
			if parseErr != nil || math.IsNaN(durationSeconds) || math.IsInf(durationSeconds, 0) || durationSeconds < 0 || durationSeconds > airplayAudioMaxSegmentSec {
				valid = false
				hasPendingDuration = false
				continue
			}
			pendingDuration = durationSeconds
			hasPendingDuration = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !hasPendingDuration || !airplayAudioSegmentName.MatchString(line) || segmentCount >= airplayAudioMaxSegments {
			valid = false
			hasPendingDuration = false
			continue
		}
		hasPendingDuration = false
		segmentCount++
		span += time.Duration(pendingDuration * float64(time.Second))
		segmentPath := filepath.Join(stateDir, "hls", line)
		segmentInfo, statErr := os.Lstat(segmentPath)
		if statErr != nil || !segmentInfo.Mode().IsRegular() || segmentInfo.Size() == 0 {
			state.missingSegmentCount++
			continue
		}
		segmentAge := clampDuration(now.Sub(segmentInfo.ModTime()), airplayAudioMaxAge)
		if !state.hasNewestSegment || segmentAge < state.newestSegmentAge {
			state.newestSegmentAge = segmentAge
			state.hasNewestSegment = true
		}
	}
	if hasPendingDuration || segmentCount == 0 {
		valid = false
	}
	state.segmentCount = uint16(segmentCount)
	state.span = span
	state.valid = valid && state.missingSegmentCount == 0
	return state
}

func mergeAirplayAudioHLSStatus(status *worker.AirPlayAudioStatus, hls airplayAudioHLSState) {
	status.HLSPlaylistPresent = hls.present
	status.HLSPlaylistValid = hls.valid
	status.HLSPlaylistAgeMS = boundedMilliseconds(hls.age)
	status.HLSPlaylistSegmentCount = hls.segmentCount
	status.HLSPlaylistMissingSegmentCount = hls.missingSegmentCount
	status.HLSPlaylistSpanMS = boundedMilliseconds(hls.span)
	if hls.hasNewestSegment {
		status.HLSLatestSegmentAgeMS = boundedMilliseconds(hls.newestSegmentAge)
	}
}

func saturatingAdd(value, increment uint64) uint64 {
	if math.MaxUint64-value < increment {
		return math.MaxUint64
	}
	return value + increment
}

func decrementIfPositive(value *uint64) {
	if *value > 0 {
		*value--
	}
}

func clampDuration(value, maximum time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	if value > maximum {
		return maximum
	}
	return value
}

func boundedMilliseconds(value time.Duration) int64 {
	value = clampDuration(value, airplayAudioMaxAge)
	return value.Milliseconds()
}
