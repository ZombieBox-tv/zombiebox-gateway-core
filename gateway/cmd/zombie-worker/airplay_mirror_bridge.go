package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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
		audioVideoSDP: filepath.Join(stateDir, "mirror-av.sdp")}
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
	b.videoSequence = sequence
	b.videoTimestamp = timestamp
	b.hasVideoClock = true
	b.lastVideo = now
	return b.activeSession == b.session
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
		return err
	}
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
			return err
		case err := <-b.processExited:
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
		return err
	}
	if err := b.clearPlaylist(previousSession != session || mode == noMirror); err != nil {
		return err
	}
	b.runningSession = session
	b.runningMode = noMirror
	if mode == noMirror {
		return nil
	}
	cmd := exec.CommandContext(ctx, b.ffmpegPath, b.commandArgs(mode)...)
	cmd.WaitDelay = time.Second
	// AirPlay metadata, private paths and RTP never enter process logs.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start AirPlay mirror encoder: %w", err)
	}
	b.process = cmd
	b.processExited = make(chan error, 1)
	go func() { b.processExited <- cmd.Wait() }()
	b.runningMode = mode
	b.mu.Lock()
	if b.session == session {
		b.activeSession = session
	}
	b.mu.Unlock()
	return nil
}

func (b *mirrorBridge) stopProcess() error {
	if b.process == nil {
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
	b.runningMode = noMirror
	return nil
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
