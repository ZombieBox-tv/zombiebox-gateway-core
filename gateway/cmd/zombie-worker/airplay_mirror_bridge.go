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
)

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
	if len(packet) < 12 || len(packet) >= mirrorPacketLimit || packet[0]>>6 != 2 || packet[1]&0x7f != payloadType {
		return false
	}
	return len(packet) >= 12+4*int(packet[0]&0x0f)
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
	b.mu.Lock()
	defer b.mu.Unlock()
	restart := b.session == 0 || ssrc != b.videoSSRC || now.Sub(b.lastVideo) > mirrorVideoTTL
	if !restart && b.hasVideoClock {
		var late bool
		restart, late = videoRTPRestart(b.videoSequence, sequence, b.videoTimestamp, timestamp, now.Sub(b.lastVideo))
		if late {
			return false
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
	}
	if b.videoPacketCount < ^uint64(0) {
		b.videoPacketCount++
	}
	b.videoSequence = sequence
	b.videoTimestamp = timestamp
	b.hasVideoClock = true
	b.lastVideo = now
	if b.bridgeStage == "" || b.bridgeStage == "waiting_for_video_rtp" {
		b.bridgeStage = "probing_video_input"
	}
	return b.activeSession == b.session
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
	b.mu.Unlock()

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
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == 0 || now.Sub(b.lastVideo) > mirrorVideoTTL {
		return false
	}
	// A late packet from the preceding sender cannot select the AV encoder for
	// the new video session. A continuous sender reusing its SSRC is accepted
	// after the short initial probe window.
	if ssrc == b.priorAudioSSRC && now.Sub(b.firstVideo) < mirrorProbeDelay {
		return false
	}
	b.audioSSRC = ssrc
	b.lastAudio = now
	return b.activeSession == b.session
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
		if !sender.IP.IsLoopback() || !validMirrorRTP(packet[:n], payloadType) {
			continue
		}
		ssrc := binary.BigEndian.Uint32(packet[8:12])
		now := time.Now()
		var active bool
		if video {
			active = b.observeVideo(now, ssrc, binary.BigEndian.Uint16(packet[2:4]), binary.BigEndian.Uint32(packet[4:8]))
		} else {
			active = b.observeAudio(now, ssrc)
		}
		if active {
			_, _ = output.Write(packet[:n])
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
	defer func() { _ = b.stopProcess() }()
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
			b.setBridgeStage("ffmpeg_exited", "ffmpeg_exit")
			return fmt.Errorf("AirPlay mirror FFmpeg exited: %v", err)
		case <-ticker.C:
			mode, session := b.desired(time.Now())
			if mode != b.runningMode || session != b.runningSession {
				if err := b.transition(ctx, mode, session); err != nil {
					return err
				}
			}
		case <-cleanup.C:
			if err := b.clearOldSegments(); err != nil {
				return err
			}
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
	b.runningSession = session
	b.mu.Lock()
	b.runningMode = noMirror
	b.mu.Unlock()
	if mode == noMirror {
		b.setBridgeStage("waiting_for_video_rtp", "")
		return nil
	}
	b.setBridgeStage("starting_ffmpeg", "")
	cmd := exec.CommandContext(ctx, b.ffmpegPath, b.commandArgs(mode)...)
	cmd.WaitDelay = time.Second
	// AirPlay metadata, private paths and RTP never enter process logs.
	if err := cmd.Start(); err != nil {
		b.setBridgeStage("ffmpeg_start_failed", "ffmpeg_start")
		return fmt.Errorf("start AirPlay mirror encoder: %w", err)
	}
	b.process = cmd
	b.processExited = make(chan error, 1)
	go func() { b.processExited <- cmd.Wait() }()
	b.mu.Lock()
	b.runningMode = mode
	b.encoderRunning = true
	b.bridgeStage = "awaiting_hls"
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
	_ = b.process.Process.Kill()
	select {
	case <-b.processExited:
	case <-time.After(3 * time.Second):
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
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,udp,rtp", "-localaddr", "127.0.0.1", "-listen_timeout", "-1", "-threads", "1", "-i", sdp, "-map", "0:v:0"}
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
