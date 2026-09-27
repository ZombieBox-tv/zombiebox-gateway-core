package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/worker"
)

// UxPlay sends RTP only to these loopback listeners. The bridge forwards it to
// the one active FFmpeg process, so a process can be replaced without asking
// the AirPlay sender to reconnect or binding two encoders to the same ports.
type mirrorPorts struct {
	videoIn, audioIn, videoOut, audioOut int
}

var airplayMirrorPorts = mirrorPorts{35010, 35012, 35020, 35022}

type mirrorMode uint8

const (
	noMirror mirrorMode = iota
	videoMirror
	audioVideoMirror
)

const (
	mirrorPacketLimit = 4096
	mirrorProbeDelay  = 500 * time.Millisecond
	mirrorAudioTTL    = 3 * time.Second
	mirrorVideoTTL    = 12 * time.Second
	mirrorSummaryTTL  = 30 * time.Second
)

const (
	mirrorFFmpegStartNotAttempted = "not_attempted"
	mirrorFFmpegStartStarted      = "started"
	mirrorFFmpegStartFailed       = "failed"
	mirrorFFmpegExitNotObserved   = "not_observed"
	mirrorFFmpegExitStopped       = "stopped"
	mirrorFFmpegExitClean         = "clean_exit"
	mirrorFFmpegExitError         = "error_exit"
	mirrorFFmpegExitTimeout       = "stop_timeout"
	mirrorModeNotSelected         = "not_observed"
	mirrorFailureNone             = "none"
)

type mirrorTransitionSummary struct {
	status          worker.AirPlayMirrorSessionSummary
	sessionStarted  time.Time
	sessionEnded    time.Time
	firstVideoRTPAt time.Time
	lastVideoRTPAt  time.Time
	firstManifestAt time.Time
	firstSegmentAt  time.Time
}

type mirrorIngressResult struct {
	generation uint64
	accepted   bool
}

type mirrorRTPPacket struct {
	payload   []byte
	sequence  uint16
	timestamp uint32
	marker    bool
}

type mirrorFFmpegReport struct {
	reportedFrames  uint64
	progressRecords uint64
	errorLines      uint64
}

// mirrorFFmpegDiagnostics parses FFmpeg's progress and stderr as a byte
// stream. It never stores a raw line or forwards process output to logs.
type mirrorFFmpegDiagnostics struct {
	mu sync.Mutex

	frameKeyMatch    int
	progressKeyMatch int
	errorMatch       int
	parsingFrame     bool
	number           uint64
	numberStarted    bool
	lineHasProgress  bool
	lineHasFrame     bool
	lineError        bool
	frameValue       uint64
	hasFrameValue    bool
	report           mirrorFFmpegReport
}

type mirrorBridge struct {
	ports         mirrorPorts
	hlsDir        string
	videoSDP      string
	audioVideoSDP string
	ffmpegPath    string
	videoInput    *net.UDPConn
	audioInput    *net.UDPConn
	videoOutput   *net.UDPConn
	audioOutput   *net.UDPConn

	mu                sync.Mutex
	session           uint64
	activeSession     uint64
	videoSSRC         uint32
	videoSequence     uint16
	videoTimestamp    uint32
	hasVideoClock     bool
	audioSSRC         uint32
	priorAudioSSRC    uint32
	firstVideo        time.Time
	lastVideo         time.Time
	lastAudio         time.Time
	runningSession    uint64
	runningMode       mirrorMode
	process           *exec.Cmd
	processExited     chan error
	lastSegmentNumber int64
	videoPacketCount  uint64
	bridgeStage       string
	failureStage      string
	encoderRunning    bool
	uxplayLogs        *airPlayLogDiagnostics
	sessionSummary    mirrorTransitionSummary
	videoDiagSequence uint16
	hasVideoDiagSeq   bool
	videoFUOpen       bool
	videoFUTimestamp  uint32
	videoFUType       byte
	ffmpegDiagnostics *mirrorFFmpegDiagnostics
	ffmpegDiagSession uint64
}

func newMirrorBridge(hlsDir, stateDir, ffmpegPath string, ports mirrorPorts) (*mirrorBridge, error) {
	listen := func(port int) (*net.UDPConn, error) {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	}
	dial := func(port int) (*net.UDPConn, error) {
		return net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	}
	b := &mirrorBridge{ports: ports, hlsDir: hlsDir, ffmpegPath: ffmpegPath,
		videoSDP:      filepath.Join(stateDir, "mirror-video.sdp"),
		audioVideoSDP: filepath.Join(stateDir, "mirror-av.sdp"),
		bridgeStage:   "waiting_for_video_rtp",
		uxplayLogs:    newAirPlayLogDiagnostics()}
	var err error
	defer func() {
		if err != nil {
			b.closeSockets()
		}
	}()
	if b.videoInput, err = listen(ports.videoIn); err != nil {
		return nil, fmt.Errorf("listen for AirPlay video RTP: %w", err)
	}
	if b.audioInput, err = listen(ports.audioIn); err != nil {
		return nil, fmt.Errorf("listen for AirPlay audio RTP: %w", err)
	}
	if b.videoOutput, err = dial(ports.videoOut); err != nil {
		return nil, fmt.Errorf("forward AirPlay video RTP: %w", err)
	}
	if b.audioOutput, err = dial(ports.audioOut); err != nil {
		return nil, fmt.Errorf("forward AirPlay audio RTP: %w", err)
	}
	videoSDP := fmt.Sprintf("v=0\no=- 0 0 IN IP4 127.0.0.1\ns=Zombie AirPlay Mirror Video\nc=IN IP4 127.0.0.1\nt=0 0\nm=video %d RTP/AVP 96\na=rtpmap:96 H264/90000\na=fmtp:96 packetization-mode=1\n", ports.videoOut)
	audioVideoSDP := videoSDP + fmt.Sprintf("m=audio %d RTP/AVP 97\na=rtpmap:97 L16/44100/2\n", ports.audioOut)
	if err = os.WriteFile(b.videoSDP, []byte(videoSDP), 0600); err != nil {
		return nil, err
	}
	if err = os.WriteFile(b.audioVideoSDP, []byte(audioVideoSDP), 0600); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *mirrorBridge) closeSockets() {
	for _, conn := range []*net.UDPConn{b.videoInput, b.audioInput, b.videoOutput, b.audioOutput} {
		if conn != nil {
			_ = conn.Close()
		}
	}
}

func validMirrorRTP(packet []byte, payloadType byte) bool {
	_, ok := parseMirrorRTP(packet, payloadType)
	return ok
}

// parseMirrorRTP reads only the bounded RTP envelope needed for forwarding and
// H.264 packetization evidence; it does not decode or retain packet contents.
func parseMirrorRTP(packet []byte, payloadType byte) (mirrorRTPPacket, bool) {
	if len(packet) < 12 || len(packet) >= mirrorPacketLimit || packet[0]>>6 != 2 || packet[1]&0x7f != payloadType {
		return mirrorRTPPacket{}, false
	}
	headerLength := 12 + 4*int(packet[0]&0x0f)
	if headerLength > len(packet) {
		return mirrorRTPPacket{}, false
	}
	if packet[0]&0x10 != 0 {
		if len(packet)-headerLength < 4 {
			return mirrorRTPPacket{}, false
		}
		extensionLength := 4 + 4*int(binary.BigEndian.Uint16(packet[headerLength+2:headerLength+4]))
		if extensionLength > len(packet)-headerLength {
			return mirrorRTPPacket{}, false
		}
		headerLength += extensionLength
	}
	payloadEnd := len(packet)
	if packet[0]&0x20 != 0 {
		paddingLength := int(packet[len(packet)-1])
		if paddingLength == 0 || paddingLength > payloadEnd-headerLength {
			return mirrorRTPPacket{}, false
		}
		payloadEnd -= paddingLength
	}
	if payloadEnd <= headerLength {
		return mirrorRTPPacket{}, false
	}
	return mirrorRTPPacket{
		payload:   packet[headerLength:payloadEnd],
		sequence:  binary.BigEndian.Uint16(packet[2:4]),
		timestamp: binary.BigEndian.Uint32(packet[4:8]),
		marker:    packet[1]&0x80 != 0,
	}, true
}

func emptyMirrorSessionSummary() worker.AirPlayMirrorSessionSummary {
	return worker.AirPlayMirrorSessionSummary{
		Version:          1,
		SelectedMode:     mirrorModeNotSelected,
		FFmpegStartClass: mirrorFFmpegStartNotAttempted,
		FFmpegExitClass:  mirrorFFmpegExitNotObserved,
		FailureClass:     mirrorFailureNone,
	}
}

func relativeAgeMS(now, eventAt time.Time) int64 {
	if eventAt.IsZero() {
		return 0
	}
	age := now.Sub(eventAt)
	if age <= 0 {
		return 0
	}
	return age.Milliseconds()
}

func incrementMirrorCounter(value *uint64) {
	if *value < ^uint64(0) {
		*value = *value + 1
	}
}

func advanceMirrorMatch(pattern string, matched int, value byte) (int, bool) {
	if matched > 0 && value == pattern[matched] {
		matched++
		if matched == len(pattern) {
			return 0, true
		}
		return matched, false
	}
	if value == pattern[0] {
		return 1, false
	}
	return 0, false
}

func (d *mirrorFFmpegDiagnostics) Write(data []byte) (int, error) {
	if d == nil {
		return len(data), nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, value := range data {
		if value == '\n' || value == '\r' {
			d.finishLineLocked()
			continue
		}
		if d.parsingFrame {
			if value >= '0' && value <= '9' {
				d.numberStarted = true
				digit := uint64(value - '0')
				if d.number > (^uint64(0)-digit)/10 {
					d.number = ^uint64(0)
				} else {
					d.number = d.number*10 + digit
				}
			} else if !d.numberStarted && (value == ' ' || value == '\t') {
			} else {
				d.finishFrameNumberLocked()
			}
		}
		var matched bool
		d.frameKeyMatch, matched = advanceMirrorMatch("frame=", d.frameKeyMatch, value)
		if matched {
			d.parsingFrame = true
			d.number = 0
			d.numberStarted = false
			d.lineHasFrame = true
		}
		d.progressKeyMatch, matched = advanceMirrorMatch("progress=", d.progressKeyMatch, value)
		if matched {
			d.lineHasProgress = true
		}
		lower := value
		if lower >= 'A' && lower <= 'Z' {
			lower += 'a' - 'A'
		}
		var found bool
		d.errorMatch, found = advanceMirrorMatch("error", d.errorMatch, lower)
		d.lineError = d.lineError || found
	}
	return len(data), nil
}

func (d *mirrorFFmpegDiagnostics) finishFrameNumberLocked() {
	if !d.numberStarted {
		d.parsingFrame = false
		return
	}
	d.frameValue = d.number
	d.hasFrameValue = true
	d.parsingFrame = false
	d.number = 0
	d.numberStarted = false
}

func (d *mirrorFFmpegDiagnostics) finishLineLocked() {
	if d.parsingFrame {
		d.finishFrameNumberLocked()
	}
	if d.lineHasFrame && d.hasFrameValue {
		d.report.reportedFrames = d.frameValue
	}
	if d.lineHasProgress {
		incrementMirrorCounter(&d.report.progressRecords)
	} else if !d.lineHasFrame && d.lineError {
		incrementMirrorCounter(&d.report.errorLines)
	}
	d.frameKeyMatch = 0
	d.progressKeyMatch = 0
	d.errorMatch = 0
	d.parsingFrame = false
	d.number = 0
	d.numberStarted = false
	d.lineHasProgress = false
	d.lineHasFrame = false
	d.lineError = false
	d.hasFrameValue = false
}

func (d *mirrorFFmpegDiagnostics) snapshot() mirrorFFmpegReport {
	if d == nil {
		return mirrorFFmpegReport{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.report
}

func (d *mirrorFFmpegDiagnostics) finish() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.finishLineLocked()
}

func (b *mirrorBridge) beginMirrorSessionLocked(generation uint64, now time.Time) {
	status := emptyMirrorSessionSummary()
	status.Available = true
	status.Generation = generation
	status.FirstVideoRTPObserved = true
	status.LastVideoRTPObserved = true
	b.sessionSummary = mirrorTransitionSummary{
		status:          status,
		sessionStarted:  now,
		firstVideoRTPAt: now,
		lastVideoRTPAt:  now,
	}
	b.videoDiagSequence = 0
	b.hasVideoDiagSeq = false
	b.videoFUOpen = false
	b.videoFUTimestamp = 0
	b.videoFUType = 0
	b.ffmpegDiagnostics = &mirrorFFmpegDiagnostics{}
	b.ffmpegDiagSession = generation
}

func (b *mirrorBridge) sessionSummarySnapshot(now time.Time) worker.AirPlayMirrorSessionSummary {
	b.mu.Lock()
	defer b.mu.Unlock()
	summary := b.sessionSummary
	if !summary.status.Available {
		return emptyMirrorSessionSummary()
	}
	if b.mirrorSummaryExpiredLocked(now) {
		b.sessionSummary = mirrorTransitionSummary{}
		return emptyMirrorSessionSummary()
	}

	status := summary.status
	status.VideoRTPFUAOpen = b.videoFUOpen && b.session == status.Generation
	status.SessionAgeMS = relativeAgeMS(now, summary.sessionStarted)
	if status.FirstVideoRTPObserved {
		status.FirstVideoRTPAgeMS = relativeAgeMS(now, summary.firstVideoRTPAt)
	}
	if status.LastVideoRTPObserved {
		status.LastVideoRTPAgeMS = relativeAgeMS(now, summary.lastVideoRTPAt)
	}
	if status.FirstHLSManifestObserved {
		status.FirstHLSManifestAgeMS = relativeAgeMS(now, summary.firstManifestAt)
	}
	if status.FirstHLSSegmentObserved {
		status.FirstHLSSegmentAgeMS = relativeAgeMS(now, summary.firstSegmentAt)
	}
	return status
}

func (b *mirrorBridge) expireMirrorSessionSummary(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mirrorSummaryExpiredLocked(now) {
		b.sessionSummary = mirrorTransitionSummary{}
	}
}

func (b *mirrorBridge) mirrorSummaryExpiredLocked(now time.Time) bool {
	if !b.sessionSummary.status.Available || b.sessionSummary.sessionEnded.IsZero() {
		return false
	}
	retained := now.Sub(b.sessionSummary.sessionEnded)
	return retained < 0 || retained > mirrorSummaryTTL
}

func (b *mirrorBridge) markMirrorSessionEnded(generation uint64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessionSummary.status.Available &&
		b.sessionSummary.status.Generation == generation &&
		b.sessionSummary.sessionEnded.IsZero() {
		if b.videoFUOpen && b.session == generation {
			incrementMirrorCounter(&b.sessionSummary.status.VideoRTPIncompleteFUs)
			b.videoFUOpen = false
		}
		b.sessionSummary.status.VideoRTPFUAOpen = false
		b.sessionSummary.sessionEnded = now
	}
}

func (b *mirrorBridge) recordMirrorFFmpegReport(generation uint64, report mirrorFFmpegReport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.sessionSummary.status.Available || b.sessionSummary.status.Generation != generation {
		return
	}
	status := &b.sessionSummary.status
	status.FFmpegReportedFrames = report.reportedFrames
	status.FFmpegProgressRecords = report.progressRecords
	status.FFmpegErrorLines = report.errorLines
}

func (b *mirrorBridge) mirrorSessionExpired(generation uint64, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return generation != b.session || b.lastVideo.IsZero() || now.Sub(b.lastVideo) > mirrorVideoTTL
}

func (b *mirrorBridge) recordMirrorMode(generation uint64, mode mirrorMode) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessionSummary.status.Available && b.sessionSummary.status.Generation == generation {
		b.sessionSummary.status.SelectedMode = mirrorModeName(mode)
	}
}

func (b *mirrorBridge) recordMirrorFFmpegStart(generation uint64, class string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessionSummary.status.Available && b.sessionSummary.status.Generation == generation {
		b.sessionSummary.status.FFmpegStartClass = mirrorFFmpegStartName(class)
		b.sessionSummary.status.FFmpegExitClass = mirrorFFmpegExitNotObserved
	}
}

func (b *mirrorBridge) recordMirrorFFmpegExit(generation uint64, class string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessionSummary.status.Available &&
		b.sessionSummary.status.Generation == generation &&
		b.sessionSummary.status.FFmpegExitClass == mirrorFFmpegExitNotObserved {
		b.sessionSummary.status.FFmpegExitClass = mirrorFFmpegExitName(class)
	}
}

func (b *mirrorBridge) recordMirrorHLSReadiness(now time.Time, manifestReady, segmentReady bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	summary := &b.sessionSummary
	if !summary.status.Available ||
		!summary.sessionEnded.IsZero() ||
		!b.encoderRunning ||
		b.runningSession != summary.status.Generation {
		return
	}
	if manifestReady && !summary.status.FirstHLSManifestObserved {
		summary.status.FirstHLSManifestObserved = true
		summary.firstManifestAt = now
	}
	if segmentReady && !summary.status.FirstHLSSegmentObserved {
		summary.status.FirstHLSSegmentObserved = true
		summary.firstSegmentAt = now
	}
}

func mirrorFailureClass(stage string) string {
	switch stage {
	case "rtp_listener", "ffmpeg_start", "ffmpeg_exit", "ffmpeg_stop", "hls_cleanup":
		return stage
	case "":
		return mirrorFailureNone
	default:
		return "other"
	}
}

func mirrorFFmpegStartName(class string) string {
	switch class {
	case mirrorFFmpegStartNotAttempted, mirrorFFmpegStartStarted, mirrorFFmpegStartFailed:
		return class
	default:
		return "other"
	}
}

func mirrorFFmpegExitName(class string) string {
	switch class {
	case mirrorFFmpegExitNotObserved, mirrorFFmpegExitStopped, mirrorFFmpegExitClean, mirrorFFmpegExitError, mirrorFFmpegExitTimeout:
		return class
	default:
		return "other"
	}
}

func videoRTPRestart(lastSequence, sequence uint16, lastTimestamp, timestamp uint32, elapsed time.Duration) (restart, late bool) {
	sequenceDelta := uint16(sequence - lastSequence)
	if sequenceDelta == 0 || sequenceDelta >= 65536-32 {
		return false, true
	}
	if sequenceDelta > 1024 {
		return true, false
	}
	timestampDelta := uint32(timestamp - lastTimestamp)
	if timestampDelta >= 1<<31 {
		// Ignore a reordered packet within one second; a larger backward
		// move means the sender restarted its RTP clock.
		if uint32(lastTimestamp-timestamp) <= 90000 {
			return false, true
		}
		return true, false
	}
	if elapsed < 2*time.Second && timestampDelta > 90_000*30 {
		return true, false
	}
	return false, false
}

func (b *mirrorBridge) observeVideo(now time.Time, ssrc uint32, sequence uint16, timestamp uint32) bool {
	result := b.observeVideoIngress(now, ssrc, sequence, timestamp, nil, false)
	return result.accepted && b.mirrorSessionActive(result.generation)
}

func (b *mirrorBridge) observeVideoIngress(now time.Time, ssrc uint32, sequence uint16, timestamp uint32, payload []byte, marker bool) mirrorIngressResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	restart := b.session == 0 || ssrc != b.videoSSRC || now.Sub(b.lastVideo) > mirrorVideoTTL
	if !restart && b.hasVideoClock {
		var late bool
		restart, late = videoRTPRestart(b.videoSequence, sequence, b.videoTimestamp, timestamp, now.Sub(b.lastVideo))
		if late {
			return mirrorIngressResult{generation: b.session}
		}
	}
	if restart {
		b.session++
		b.activeSession = 0
		b.videoSSRC = ssrc
		b.priorAudioSSRC = b.audioSSRC
		b.audioSSRC = 0
		b.firstVideo = now
		b.lastAudio = time.Time{}
		b.beginMirrorSessionLocked(b.session, now)
	}
	if b.videoPacketCount < ^uint64(0) {
		b.videoPacketCount++
	}
	if b.sessionSummary.status.Available && b.sessionSummary.status.Generation == b.session {
		incrementMirrorCounter(&b.sessionSummary.status.VideoRTPAcceptedPackets)
		if b.sessionSummary.status.VideoRTPPacketCount < ^uint64(0) {
			b.sessionSummary.status.VideoRTPPacketCount++
		}
		b.sessionSummary.status.LastVideoRTPObserved = true
		b.sessionSummary.lastVideoRTPAt = now
		b.recordVideoH264PacketLocked(payload, sequence, timestamp, marker)
	}
	b.videoSequence = sequence
	b.videoTimestamp = timestamp
	b.hasVideoClock = true
	b.lastVideo = now
	if b.bridgeStage == "" || b.bridgeStage == "waiting_for_video_rtp" {
		b.bridgeStage = "probing_video_input"
	}
	return mirrorIngressResult{generation: b.session, accepted: true}
}

func (b *mirrorBridge) mirrorSessionActive(generation uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return generation != 0 && generation == b.session && generation == b.activeSession
}

func (b *mirrorBridge) incrementVideoIncompleteFULocked() bool {
	if b.videoFUOpen {
		incrementMirrorCounter(&b.sessionSummary.status.VideoRTPIncompleteFUs)
		b.videoFUOpen = false
		b.sessionSummary.status.VideoRTPFUAOpen = false
		return true
	}
	return false
}

func (b *mirrorBridge) recordVideoNALHeaderLocked(nalType byte) {
	status := &b.sessionSummary.status
	switch nalType {
	case 7:
		status.VideoRTPSPSObserved = true
	case 8:
		status.VideoRTPPPSObserved = true
	case 5:
		status.VideoRTPIDRObserved = true
	}
}

func (b *mirrorBridge) recordVideoH264PacketLocked(payload []byte, sequence uint16, timestamp uint32, marker bool) {
	status := &b.sessionSummary.status
	incompleteFromGap := false
	if marker {
		incrementMirrorCounter(&status.VideoRTPMarkerPackets)
	}
	if b.hasVideoDiagSeq {
		delta := uint16(sequence - b.videoDiagSequence)
		if delta > 1 && delta < 1<<15 {
			incrementMirrorCounter(&status.VideoRTPSequenceGaps)
			incompleteFromGap = b.incrementVideoIncompleteFULocked()
		}
	}
	b.videoDiagSequence, b.hasVideoDiagSeq = sequence, true
	if len(payload) == 0 {
		return
	}
	if payload[0]&0x80 != 0 {
		incrementMirrorCounter(&status.VideoRTPUnclassifiedPackets)
		b.incrementVideoIncompleteFULocked()
		return
	}
	packetType := payload[0] & 0x1f
	switch {
	case packetType >= 1 && packetType <= 23:
		if b.videoFUOpen {
			b.incrementVideoIncompleteFULocked()
		}
		incrementMirrorCounter(&status.VideoRTPSingleNALPackets)
		b.recordVideoNALHeaderLocked(packetType)
	case packetType == 24:
		if b.videoFUOpen {
			b.incrementVideoIncompleteFULocked()
		}
		incrementMirrorCounter(&status.VideoRTPSTAPAPackets)
		position, units := 1, 0
		for position < len(payload) {
			if len(payload)-position < 2 {
				incrementMirrorCounter(&status.VideoRTPUnclassifiedPackets)
				break
			}
			size := int(binary.BigEndian.Uint16(payload[position : position+2]))
			position += 2
			if size == 0 || size > len(payload)-position {
				incrementMirrorCounter(&status.VideoRTPUnclassifiedPackets)
				break
			}
			nal := payload[position]
			typ := nal & 0x1f
			if nal&0x80 != 0 || typ == 0 || typ > 23 {
				incrementMirrorCounter(&status.VideoRTPUnclassifiedPackets)
			} else {
				b.recordVideoNALHeaderLocked(typ)
				units++
			}
			position += size
		}
		if units == 0 {
			incrementMirrorCounter(&status.VideoRTPUnclassifiedPackets)
		}
	case packetType == 28:
		incrementMirrorCounter(&status.VideoRTPFUAPackets)
		if len(payload) < 2 || payload[0]&0x80 != 0 {
			incrementMirrorCounter(&status.VideoRTPUnclassifiedPackets)
			b.incrementVideoIncompleteFULocked()
			return
		}
		fu := payload[1]
		start, end, typ := fu&0x80 != 0, fu&0x40 != 0, fu&0x1f
		if fu&0x20 != 0 || typ == 0 || typ > 23 || (start && end) {
			incrementMirrorCounter(&status.VideoRTPUnclassifiedPackets)
			b.incrementVideoIncompleteFULocked()
			return
		}
		if start {
			if b.videoFUOpen {
				b.incrementVideoIncompleteFULocked()
			}
			b.videoFUOpen, b.videoFUTimestamp, b.videoFUType = true, timestamp, typ
			status.VideoRTPFUAOpen = true
		} else if !b.videoFUOpen || b.videoFUType != typ || b.videoFUTimestamp != timestamp {
			if !incompleteFromGap {
				incrementMirrorCounter(&status.VideoRTPIncompleteFUs)
			}
			b.videoFUOpen = false
			status.VideoRTPFUAOpen = false
		} else if end {
			b.recordVideoNALHeaderLocked(typ)
			b.videoFUOpen = false
			status.VideoRTPFUAOpen = false
		}
	default:
		if b.videoFUOpen {
			b.incrementVideoIncompleteFULocked()
		}
		incrementMirrorCounter(&status.VideoRTPUnclassifiedPackets)
	}
	if marker && b.videoFUOpen {
		b.incrementVideoIncompleteFULocked()
	}
}

func (b *mirrorBridge) forwardMirrorPacket(generation uint64, video bool, output *net.UDPConn, packet []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.sessionSummary.status.Available || b.sessionSummary.status.Generation != generation {
		return
	}
	status := &b.sessionSummary.status
	var inactiveDrops, forwarded, failures *uint64
	if video {
		inactiveDrops = &status.VideoRTPInactiveDrops
		forwarded = &status.VideoRTPForwardedPackets
		failures = &status.VideoRTPForwardFailures
	} else {
		inactiveDrops = &status.AudioRTPInactiveDrops
		forwarded = &status.AudioRTPForwardedPackets
		failures = &status.AudioRTPForwardFailures
	}
	if generation != b.session || generation != b.activeSession || generation != b.runningSession || !b.encoderRunning || b.runningMode == noMirror {
		incrementMirrorCounter(inactiveDrops)
		return
	}
	if output == nil {
		incrementMirrorCounter(failures)
		return
	}
	written, err := output.Write(packet)
	if err != nil || written != len(packet) {
		incrementMirrorCounter(failures)
		return
	}
	incrementMirrorCounter(forwarded)
}

// AirPlayMirrorSnapshot exposes only fixed state and counters. It reads a
// bounded local manifest and never returns its contents or any RTP/log data.
func (b *mirrorBridge) AirPlayMirrorSnapshot(now time.Time) worker.AirPlayMirrorStatus {
	b.mu.Lock()
	lastVideo := b.lastVideo
	packetCount := b.videoPacketCount
	mode := b.runningMode
	stage := b.bridgeStage
	failureStage := b.failureStage
	encoderRunning := b.encoderRunning
	logs := b.uxplayLogs
	ffmpegDiagnostics := b.ffmpegDiagnostics
	ffmpegDiagSession := b.ffmpegDiagSession
	b.mu.Unlock()
	if ffmpegDiagnostics != nil {
		b.recordMirrorFFmpegReport(ffmpegDiagSession, ffmpegDiagnostics.snapshot())
	}

	status := worker.AirPlayMirrorStatus{
		Protocol:                     "airplay_mirroring_rtp_h264",
		Mode:                         mirrorModeName(mode),
		VideoRTPPacketCount:          packetCount,
		BridgeStage:                  stage,
		BridgeFailureStage:           failureStage,
		PhotoAppAttributionAvailable: false,
	}
	if !lastVideo.IsZero() {
		age := now.Sub(lastVideo)
		if age >= 0 {
			status.VideoRTPLastPacketAgeMS = age.Milliseconds()
			status.VideoRTPAdvancedRecently = age <= time.Second
		}
	}
	status.HLSManifestReady, status.HLSSegmentReady, status.HLSSegmentAgeMS = mirrorHLSReadiness(b.hlsDir, now)
	b.recordMirrorHLSReadiness(now, status.HLSManifestReady, status.HLSSegmentReady)
	status.SessionSummary = b.sessionSummarySnapshot(now)
	if encoderRunning && status.HLSManifestReady && status.HLSSegmentReady {
		status.BridgeStage = "hls_ready"
	} else if status.BridgeStage == "" {
		status.BridgeStage = "waiting_for_video_rtp"
	}
	if logs != nil {
		status.DirectVideoRequestCount = logs.DirectVideoRequestCount()
	}
	return status
}

func mirrorModeName(mode mirrorMode) string {
	switch mode {
	case videoMirror:
		return "video"
	case audioVideoMirror:
		return "audio_video"
	default:
		return "idle"
	}
}

func mirrorHLSReadiness(hlsDir string, now time.Time) (manifestReady, segmentReady bool, segmentAgeMS int64) {
	if hlsDir == "" {
		return false, false, 0
	}
	manifestPath := filepath.Join(hlsDir, "index.m3u8")
	info, err := os.Stat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 64<<10 || !mirrorFileFresh(info.ModTime(), now) {
		return false, false, 0
	}
	file, err := os.Open(manifestPath)
	if err != nil {
		return false, false, 0
	}
	data, readErr := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	_ = file.Close()
	if readErr != nil || len(data) == 0 || len(data) > 64<<10 {
		return false, false, 0
	}
	content := string(data)
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return false, false, 0
	}
	targetDurationValid := false
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			continue
		}
		target, parseErr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")))
		targetDurationValid = parseErr == nil && target > 0
		break
	}
	if !targetDurationValid {
		return false, false, 0
	}
	manifestReady = true
	var pendingDuration float64
	hasPendingDuration := false
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#EXTINF:") {
			duration := strings.TrimPrefix(line, "#EXTINF:")
			if comma := strings.IndexByte(duration, ','); comma >= 0 {
				duration = duration[:comma]
			}
			pendingDuration, err = strconv.ParseFloat(strings.TrimSpace(duration), 64)
			hasPendingDuration = err == nil && pendingDuration >= 0.5
			continue
		}
		if strings.HasPrefix(line, "#") || !hasPendingDuration {
			continue
		}
		name := line
		hasPendingDuration = false
		if !mirrorSegmentName(name) {
			continue
		}
		segmentInfo, statErr := os.Stat(filepath.Join(hlsDir, name))
		if statErr != nil || !segmentInfo.Mode().IsRegular() || segmentInfo.Size() < 4096 || !mirrorFileFresh(segmentInfo.ModTime(), now) {
			continue
		}
		segmentReady = true
		age := now.Sub(segmentInfo.ModTime())
		if age >= 0 {
			segmentAgeMS = age.Milliseconds()
		}
		break
	}
	return manifestReady, segmentReady, segmentAgeMS
}

func mirrorFileFresh(modTime time.Time, now time.Time) bool {
	age := now.Sub(modTime)
	return age >= 0 && age <= 15*time.Second
}

const directAirPlayVideoRequestPhrase = "ignoring AirPlay video streaming request (use option -hls to activate HLS support)"

// airPlayLogDiagnostics counts one fixed, non-mirroring video-route message
// from UxPlay. It retains only a matcher offset and a saturating counter; app
// identity cannot be inferred from this message.
type airPlayLogDiagnostics struct {
	mu          sync.Mutex
	matched     int
	discardLine bool
	count       uint8
}

func newAirPlayLogDiagnostics() *airPlayLogDiagnostics {
	return &airPlayLogDiagnostics{}
}

// Write implements io.Writer for UxPlay stdout. It recognizes only a complete
// fixed route-warning line. No line, URL, address, pairing value or payload is kept.
func (d *airPlayLogDiagnostics) Write(data []byte) (int, error) {
	if d == nil {
		return len(data), nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, value := range data {
		if value == '\n' || value == '\r' {
			if d.matched == len(directAirPlayVideoRequestPhrase) && d.count < ^uint8(0) {
				d.count++
			}
			d.matched = 0
			d.discardLine = false
			continue
		}
		if d.discardLine {
			continue
		}
		if d.matched < len(directAirPlayVideoRequestPhrase) && value == directAirPlayVideoRequestPhrase[d.matched] {
			d.matched++
		} else {
			d.matched = 0
			d.discardLine = true
		}
	}
	return len(data), nil
}

func (d *airPlayLogDiagnostics) DirectVideoRequestCount() uint8 {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

func (b *mirrorBridge) observeAudio(now time.Time, ssrc uint32) bool {
	result := b.observeAudioIngress(now, ssrc)
	return result.accepted && b.mirrorSessionActive(result.generation)
}

func (b *mirrorBridge) observeAudioIngress(now time.Time, ssrc uint32) mirrorIngressResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == 0 || now.Sub(b.lastVideo) > mirrorVideoTTL {
		return mirrorIngressResult{generation: b.session}
	}
	// A late packet from the preceding sender cannot select the AV encoder for
	// the new video session. A continuous sender reusing its SSRC is accepted
	// after the short initial probe window.
	if ssrc == b.priorAudioSSRC && now.Sub(b.firstVideo) < mirrorProbeDelay {
		return mirrorIngressResult{generation: b.session}
	}
	b.audioSSRC = ssrc
	b.lastAudio = now
	if b.sessionSummary.status.Available && b.sessionSummary.status.Generation == b.session {
		incrementMirrorCounter(&b.sessionSummary.status.AudioRTPAcceptedPackets)
	}
	return mirrorIngressResult{generation: b.session, accepted: true}
}

func (b *mirrorBridge) desired(now time.Time) (mirrorMode, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == 0 || now.Sub(b.lastVideo) > mirrorVideoTTL {
		return noMirror, b.session
	}
	if now.Sub(b.firstVideo) < mirrorProbeDelay {
		return noMirror, b.session
	}
	if !b.lastAudio.IsZero() && now.Sub(b.lastAudio) <= mirrorAudioTTL {
		return audioVideoMirror, b.session
	}
	return videoMirror, b.session
}

func (b *mirrorBridge) forward(ctx context.Context, input, output *net.UDPConn, payloadType byte, video bool, failures chan<- error) {
	packet := make([]byte, mirrorPacketLimit)
	for ctx.Err() == nil {
		_ = input.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, sender, err := input.ReadFromUDP(packet)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if ctx.Err() == nil {
				select {
				case failures <- fmt.Errorf("AirPlay RTP listener failed: %w", err):
				default:
				}
			}
			return
		}
		if !sender.IP.IsLoopback() {
			continue
		}
		rtp, ok := parseMirrorRTP(packet[:n], payloadType)
		if !ok {
			continue
		}
		ssrc := binary.BigEndian.Uint32(packet[8:12])
		now := time.Now()
		var accepted mirrorIngressResult
		if video {
			accepted = b.observeVideoIngress(now, ssrc, rtp.sequence, rtp.timestamp, rtp.payload, rtp.marker)
		} else {
			accepted = b.observeAudioIngress(now, ssrc)
		}
		if accepted.accepted {
			b.forwardMirrorPacket(accepted.generation, video, output, packet[:n])
		}
	}
}

func (b *mirrorBridge) run(ctx context.Context) error {
	defer b.closeSockets()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := b.clearPlaylist(true); err != nil {
		b.setBridgeStage("hls_cleanup_failed", "hls_cleanup")
		return err
	}
	b.setBridgeStage("waiting_for_video_rtp", "")
	failures := make(chan error, 2)
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		b.forward(ctx, b.videoInput, b.videoOutput, 96, true, failures)
	}()
	go func() {
		defer readers.Done()
		b.forward(ctx, b.audioInput, b.audioOutput, 97, false, failures)
	}()
	defer func() {
		cancel()
		readers.Wait()
	}()
	defer func() {
		_ = b.stopProcess()
		b.markMirrorSessionEnded(b.runningSession, time.Now())
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	cleanup := time.NewTicker(10 * time.Second)
	defer cleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			b.setBridgeStage("rtp_listener_failed", "rtp_listener")
			return err
		case err := <-b.processExited:
			exitClass := mirrorFFmpegExitClean
			if err != nil {
				exitClass = mirrorFFmpegExitError
			}
			b.recordMirrorFFmpegExit(b.runningSession, exitClass)
			b.setBridgeStage("ffmpeg_exited", "ffmpeg_exit")
			return fmt.Errorf("AirPlay mirror FFmpeg exited: %v", err)
		case <-ticker.C:
			now := time.Now()
			mode, session := b.desired(now)
			if mode != b.runningMode || session != b.runningSession {
				if err := b.transition(ctx, mode, session); err != nil {
					return err
				}
			}
			manifestReady, segmentReady, _ := mirrorHLSReadiness(b.hlsDir, now)
			b.recordMirrorHLSReadiness(now, manifestReady, segmentReady)
		case <-cleanup.C:
			if err := b.clearOldSegments(); err != nil {
				return err
			}
			b.expireMirrorSessionSummary(time.Now())
		}
	}
}

func (b *mirrorBridge) transition(ctx context.Context, mode mirrorMode, session uint64) error {
	previousSession := b.runningSession
	b.mu.Lock()
	b.activeSession = 0
	b.mu.Unlock()
	if err := b.stopProcess(); err != nil {
		b.setBridgeStage("ffmpeg_stop_failed", "ffmpeg_stop")
		return err
	}
	if err := b.clearPlaylist(previousSession != session || mode == noMirror); err != nil {
		b.setBridgeStage("hls_cleanup_failed", "hls_cleanup")
		return err
	}
	b.mu.Lock()
	b.runningSession = session
	b.runningMode = noMirror
	b.mu.Unlock()
	if mode == noMirror {
		b.setBridgeStage("waiting_for_video_rtp", "")
		now := time.Now()
		if b.mirrorSessionExpired(session, now) {
			b.markMirrorSessionEnded(session, now)
		}
		return nil
	}
	b.setBridgeStage("starting_ffmpeg", "")
	b.recordMirrorMode(session, mode)
	cmd := exec.CommandContext(ctx, b.ffmpegPath, b.commandArgs(mode)...)
	cmd.WaitDelay = time.Second
	b.mu.Lock()
	var diagnostics *mirrorFFmpegDiagnostics
	if b.ffmpegDiagSession == session {
		diagnostics = b.ffmpegDiagnostics
		cmd.Stderr = diagnostics
	}
	b.mu.Unlock()
	// AirPlay metadata, private paths and RTP never enter process logs.
	if err := cmd.Start(); err != nil {
		b.recordMirrorFFmpegStart(session, mirrorFFmpegStartFailed)
		b.setBridgeStage("ffmpeg_start_failed", "ffmpeg_start")
		return fmt.Errorf("start AirPlay mirror encoder: %w", err)
	}
	b.process = cmd
	b.processExited = make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if diagnostics != nil {
			diagnostics.finish()
			b.recordMirrorFFmpegReport(session, diagnostics.snapshot())
		}
		b.processExited <- err
	}()
	b.mu.Lock()
	b.runningMode = mode
	b.encoderRunning = true
	b.bridgeStage = "awaiting_hls"
	if b.sessionSummary.status.Available && b.sessionSummary.status.Generation == session {
		b.sessionSummary.status.FFmpegStartClass = mirrorFFmpegStartStarted
		b.sessionSummary.status.FFmpegExitClass = mirrorFFmpegExitNotObserved
	}
	if b.session == session {
		b.activeSession = session
	}
	b.mu.Unlock()
	return nil
}

func (b *mirrorBridge) stopProcess() error {
	if b.process == nil {
		b.mu.Lock()
		b.encoderRunning = false
		b.mu.Unlock()
		return nil
	}
	generation := b.runningSession
	_ = b.process.Process.Kill()
	select {
	case <-b.processExited:
		b.recordMirrorFFmpegExit(generation, mirrorFFmpegExitStopped)
	case <-time.After(3 * time.Second):
		b.recordMirrorFFmpegExit(generation, mirrorFFmpegExitTimeout)
		b.setBridgeStage("ffmpeg_stop_failed", "ffmpeg_stop")
		return fmt.Errorf("AirPlay mirror encoder did not stop")
	}
	b.process = nil
	b.processExited = nil
	b.mu.Lock()
	b.runningMode = noMirror
	b.encoderRunning = false
	b.mu.Unlock()
	return nil
}

func (b *mirrorBridge) setBridgeStage(stage, failure string) {
	b.mu.Lock()
	b.bridgeStage = stage
	b.failureStage = failure
	if b.sessionSummary.status.Available && b.sessionSummary.status.FailureClass == mirrorFailureNone && failure != "" {
		b.sessionSummary.status.FailureClass = mirrorFailureClass(failure)
	}
	b.mu.Unlock()
}

func (b *mirrorBridge) commandArgs(mode mirrorMode) []string {
	sdp := b.videoSDP
	if mode == audioVideoMirror {
		sdp = b.audioVideoSDP
	}
	// The old encoder has exited before this command is built. Its remaining
	// segments determine a short, monotonic media sequence that legacy HLS
	// clients can parse without an epoch-sized integer.
	number := b.lastSegmentNumber + 1
	if entries, err := os.ReadDir(b.hlsDir); err == nil {
		for _, entry := range entries {
			if mirrorSegmentName(entry.Name()) {
				n, parseErr := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "segment"), ".ts"), 10, 64)
				if parseErr == nil && n >= number {
					number = n + 1
				}
			}
		}
	}
	b.lastSegmentNumber = number
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-progress", "pipe:2", "-protocol_whitelist", "file,udp,rtp", "-localaddr", "127.0.0.1", "-listen_timeout", "-1", "-threads", "1", "-i", sdp, "-map", "0:v:0"}
	if mode == audioVideoMirror {
		args = append(args, "-map", "0:a:0")
	}
	args = append(args, "-c:v", "libx264", "-threads", "2", "-preset", "ultrafast", "-tune", "zerolatency", "-profile:v", "baseline", "-level:v", "3.0", "-vf", "scale=640:360:force_original_aspect_ratio=decrease,pad=640:360:(ow-iw)/2:(oh-ih)/2,format=yuv420p", "-r", "30", "-g", "30", "-b:v", "1000k")
	if mode == audioVideoMirror {
		args = append(args, "-c:a", "aac", "-b:a", "128k")
	}
	return append(args, "-f", "hls", "-hls_time", "1", "-hls_list_size", "4", "-hls_flags", "delete_segments+omit_endlist+temp_file+discont_start", "-start_number", strconv.FormatInt(number, 10), "-hls_segment_filename", filepath.Join(b.hlsDir, "segment%d.ts"), filepath.Join(b.hlsDir, "index.m3u8"))
}

func mirrorSegmentName(name string) bool {
	if !strings.HasPrefix(name, "segment") || !strings.HasSuffix(name, ".ts") {
		return false
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(name, "segment"), ".ts")
	if digits == "" {
		return false
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func (b *mirrorBridge) clearPlaylist(removeSegments bool) error {
	if err := os.Remove(filepath.Join(b.hlsDir, "index.m3u8")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(filepath.Join(b.hlsDir, "index.m3u8.tmp")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if !removeSegments {
		return nil
	}
	entries, err := os.ReadDir(b.hlsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if mirrorSegmentName(entry.Name()) {
			if err := os.Remove(filepath.Join(b.hlsDir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (b *mirrorBridge) clearOldSegments() error {
	entries, err := os.ReadDir(b.hlsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !mirrorSegmentName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if time.Since(info.ModTime()) > 30*time.Second {
			if err := os.Remove(filepath.Join(b.hlsDir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
