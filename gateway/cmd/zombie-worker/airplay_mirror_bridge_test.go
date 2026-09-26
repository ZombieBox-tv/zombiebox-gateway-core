package main

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMirrorSessionScopesAudioAndStopsOldForwarding(t *testing.T) {
	b := &mirrorBridge{}
	start := time.Now()
	if b.observeVideo(start, 11, 1, 90_000) {
		t.Fatal("video forwarded before an encoder has been selected")
	}
	b.observeAudio(start.Add(100*time.Millisecond), 21)
	if mode, session := b.desired(start.Add(600 * time.Millisecond)); mode != audioVideoMirror || session != 1 {
		t.Fatalf("first session: mode=%v session=%d", mode, session)
	}
	b.mu.Lock()
	b.activeSession = 1
	b.mu.Unlock()
	if !b.observeVideo(start.Add(time.Second), 11, 2, 180_000) {
		t.Fatal("active session video was dropped")
	}
	if b.observeVideo(start.Add(1100*time.Millisecond), 12, 1, 90_000) {
		t.Fatal("new sender was forwarded into the old encoder")
	}
	if mode, session := b.desired(start.Add(1700 * time.Millisecond)); mode != videoMirror || session != 2 {
		t.Fatalf("old audio selected for new session: mode=%v session=%d", mode, session)
	}
	if b.observeAudio(start.Add(1750*time.Millisecond), 22) {
		t.Fatal("new session audio forwarded before its encoder is ready")
	}
	if mode, _ := b.desired(start.Add(1800 * time.Millisecond)); mode != audioVideoMirror {
		t.Fatal("new audio did not select AV mode")
	}
}

func TestMirrorAudioCanAppearAndDisappearWithoutChangingVideoSession(t *testing.T) {
	b := &mirrorBridge{}
	start := time.Now()
	b.observeVideo(start, 31, 1, 90_000)
	if mode, _ := b.desired(start.Add(600 * time.Millisecond)); mode != videoMirror {
		t.Fatalf("silent sender selected %v", mode)
	}
	b.observeAudio(start.Add(time.Second), 41)
	if mode, session := b.desired(start.Add(1100 * time.Millisecond)); mode != audioVideoMirror || session != 1 {
		t.Fatalf("late audio was not selected: mode=%v session=%d", mode, session)
	}
	b.observeVideo(start.Add(5*time.Second), 31, 2, 540_000)
	if mode, session := b.desired(start.Add(5 * time.Second)); mode != videoMirror || session != 1 {
		t.Fatalf("expired audio blocked video: mode=%v session=%d", mode, session)
	}
}

func TestMirrorRTPRestartWithReusedSSRC(t *testing.T) {
	b := &mirrorBridge{}
	start := time.Now()
	b.observeVideo(start, 71, 42_000, 1_000_000)
	b.mu.Lock()
	b.activeSession = 1
	b.mu.Unlock()
	if !b.observeVideo(start.Add(50*time.Millisecond), 71, 42_001, 1_003_000) {
		t.Fatal("continuous RTP was fenced")
	}
	if b.observeVideo(start.Add(100*time.Millisecond), 71, 11, 90_000) {
		t.Fatal("RTP clock restart leaked into the old encoder")
	}
	if mode, session := b.desired(start.Add(700 * time.Millisecond)); mode != videoMirror || session != 2 {
		t.Fatalf("restart was not fenced: mode=%v session=%d", mode, session)
	}
	if restart, late := videoRTPRestart(65535, 0, ^uint32(0)-1000, 2000, time.Second); restart || late {
		t.Fatal("normal RTP counter wrap was treated as a reconnect")
	}
	if restart, late := videoRTPRestart(100, 99, 100_000, 97_000, time.Second); restart || !late {
		t.Fatal("small out-of-order RTP packet was not dropped")
	}
}

func TestMirrorDiagnosticsTrackOnlyAdvancingVideoRTP(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	b := &mirrorBridge{hlsDir: t.TempDir(), uxplayLogs: newAirPlayLogDiagnostics()}
	initial := b.AirPlayMirrorSnapshot(start)
	if initial.Protocol != "airplay_mirroring_rtp_h264" || initial.Mode != "idle" || initial.VideoRTPAdvancedRecently || initial.VideoRTPPacketCount != 0 {
		t.Fatalf("unexpected initial mirror status: %+v", initial)
	}
	if b.observeVideo(start, 71, 10, 90_000) {
		t.Fatal("first packet unexpectedly forwarded before encoder selection")
	}
	advanced := b.AirPlayMirrorSnapshot(start.Add(500 * time.Millisecond))
	if !advanced.VideoRTPAdvancedRecently || advanced.VideoRTPPacketCount != 1 || advanced.VideoRTPLastPacketAgeMS != 500 || advanced.BridgeStage != "probing_video_input" {
		t.Fatalf("advancing RTP evidence missing: %+v", advanced)
	}
	if b.observeVideo(start.Add(700*time.Millisecond), 71, 10, 90_000) {
		t.Fatal("duplicate sequence was treated as advancing video")
	}
	stale := b.AirPlayMirrorSnapshot(start.Add(2200 * time.Millisecond))
	if stale.VideoRTPAdvancedRecently || stale.VideoRTPPacketCount != 1 || stale.VideoRTPLastPacketAgeMS != 2200 {
		t.Fatalf("duplicate packet changed bounded RTP evidence: %+v", stale)
	}
	b.mu.Lock()
	b.videoPacketCount = ^uint64(0)
	b.mu.Unlock()
	b.observeVideo(start.Add(3*time.Second), 71, 11, 270_000)
	if got := b.AirPlayMirrorSnapshot(start.Add(3 * time.Second)).VideoRTPPacketCount; got != ^uint64(0) {
		t.Fatalf("RTP counter overflowed: %d", got)
	}
}

func TestMirrorDiagnosticsReportHLSReadinessWithoutContents(t *testing.T) {
	dir := t.TempDir()
	manifest := "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\nsegment7.ts\n"
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "segment7.ts"), make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for _, name := range []string{"index.m3u8", "segment7.ts"} {
		path := filepath.Join(dir, name)
		if err := os.Chtimes(path, start, start); err != nil {
			t.Fatal(err)
		}
	}
	b := &mirrorBridge{hlsDir: dir, runningMode: videoMirror, encoderRunning: true, bridgeStage: "awaiting_hls"}
	status := b.AirPlayMirrorSnapshot(start)
	if !status.HLSManifestReady || !status.HLSSegmentReady || status.HLSSegmentAgeMS < 0 || status.BridgeStage != "hls_ready" {
		t.Fatalf("fresh mirror HLS was not reported ready: %+v", status)
	}
	old := start.Add(-20 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "segment7.ts"), old, old); err != nil {
		t.Fatal(err)
	}
	status = b.AirPlayMirrorSnapshot(start)
	if status.HLSSegmentReady || status.BridgeStage == "hls_ready" {
		t.Fatalf("stale segment was reported ready: %+v", status)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\nsegment7.ts\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "index.m3u8"), start, start); err != nil {
		t.Fatal(err)
	}
	status = b.AirPlayMirrorSnapshot(start)
	if !status.HLSManifestReady || status.HLSSegmentReady {
		t.Fatalf("unreferenced TS file was reported as a ready HLS segment: %+v", status)
	}
}

func TestMirrorDiagnosticsRecordSanitizedEncoderFailureStage(t *testing.T) {
	b := &mirrorBridge{hlsDir: t.TempDir(), ffmpegPath: filepath.Join(t.TempDir(), "missing-ffmpeg")}
	if err := b.transition(context.Background(), videoMirror, 1); err == nil {
		t.Fatal("missing FFmpeg executable unexpectedly started")
	}
	status := b.AirPlayMirrorSnapshot(time.Now())
	if status.BridgeStage != "ffmpeg_start_failed" || status.BridgeFailureStage != "ffmpeg_start" || status.Mode != "idle" {
		t.Fatalf("FFmpeg failure was not retained as a fixed stage: %+v", status)
	}
}

func TestAirPlayLogDiagnosticsMatchOnlyFixedDirectVideoWarning(t *testing.T) {
	diagnostics := newAirPlayLogDiagnostics()
	secret := "Authorization: Bearer never-store-this\n"
	phrase := []byte(directAirPlayVideoRequestPhrase)
	if n, err := diagnostics.Write(append([]byte(secret), phrase[:19]...)); err != nil || n != len(secret)+19 {
		t.Fatalf("first log fragment: n=%d err=%v", n, err)
	}
	if n, err := diagnostics.Write(append(phrase[19:], '\n')); err != nil || n != len(phrase)-19+1 {
		t.Fatalf("second log fragment: n=%d err=%v", n, err)
	}
	if got := diagnostics.DirectVideoRequestCount(); got != 1 {
		t.Fatalf("direct-video count = %d, want 1", got)
	}
	if diagnostics.matched != 0 || diagnostics.discardLine {
		t.Fatal("log parser retained source text instead of bounded matcher state")
	}
	_, _ = diagnostics.Write(append([]byte("prefix "), append(phrase, '\n')...))
	_, _ = diagnostics.Write(append([]byte("*** WARNING: "), append(phrase, '\n')...))
	if got := diagnostics.DirectVideoRequestCount(); got != 1 {
		t.Fatalf("embedded or incorrect-prefix warning counted as a UxPlay route line: %d", got)
	}
	for range 300 {
		_, _ = diagnostics.Write(append(phrase, '\n'))
	}
	if got := diagnostics.DirectVideoRequestCount(); got != ^uint8(0) {
		t.Fatalf("counter did not saturate: %d", got)
	}
}

func TestMirrorStopWaitsForEncoderExit(t *testing.T) {
	sleeper, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep unavailable")
	}
	cmd := exec.Command(sleeper, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	b := &mirrorBridge{process: cmd, processExited: make(chan error, 1), runningMode: videoMirror}
	go func() { b.processExited <- cmd.Wait() }()
	if err := b.stopProcess(); err != nil {
		t.Fatal(err)
	}
	if cmd.ProcessState == nil || b.process != nil || b.runningMode != noMirror {
		t.Fatal("encoder was not fully reaped before transition")
	}
}

func TestMirrorRTPAndPlaylistFence(t *testing.T) {
	packet := make([]byte, 12)
	packet[0] = 0x80
	packet[1] = 96
	binary.BigEndian.PutUint32(packet[8:12], 17)
	if !validMirrorRTP(packet, 96) || validMirrorRTP(packet, 97) {
		t.Fatal("RTP payload type validation failed")
	}
	packet[0] = 0x40
	if validMirrorRTP(packet, 96) {
		t.Fatal("non-RTPv2 packet accepted")
	}
	packet[0] = 0x80
	if validMirrorRTP(make([]byte, mirrorPacketLimit), 96) {
		t.Fatal("oversized packet accepted")
	}

	dir := t.TempDir()
	for name, content := range map[string]string{"index.m3u8": "old", "segment17.ts": "old", "audio.m3u8": "keep", "segmentOther.ts": "keep"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := &mirrorBridge{hlsDir: dir}
	if err := b.clearPlaylist(true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.m3u8", "segment17.ts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("old mirror file %s survived", name)
		}
	}
	for _, name := range []string{"audio.m3u8", "segmentOther.ts"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("unrelated file %s was removed: %v", name, err)
		}
	}
}

func TestMirrorCommandsSelectExactlyOneAudioPolicy(t *testing.T) {
	dir := t.TempDir()
	b := &mirrorBridge{hlsDir: dir, videoSDP: "video.sdp", audioVideoSDP: "av.sdp"}
	video := strings.Join(b.commandArgs(videoMirror), " ")
	if !strings.Contains(video, "-i video.sdp -map 0:v:0") || strings.Contains(video, "0:a:0") || strings.Contains(video, "-c:a") {
		t.Fatalf("video-only command unexpectedly maps audio: %s", video)
	}
	if err := os.WriteFile(filepath.Join(dir, "segment5.ts"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	av := strings.Join(b.commandArgs(audioVideoMirror), " ")
	if !strings.Contains(av, "-i av.sdp -map 0:v:0 -map 0:a:0") || !strings.Contains(av, "-c:a aac") || !strings.Contains(av, "-start_number 6") {
		t.Fatalf("AV command does not preserve audio or unique sequence: %s", av)
	}
}

// Run explicitly with ZOMBIE_TEST_AIRPLAY_FFMPEG=1. This uses local synthetic
// RTP only; it does not contact a receiver, account or physical device.
func TestMirrorBridgeSyntheticRTP(t *testing.T) {
	if os.Getenv("ZOMBIE_TEST_AIRPLAY_FFMPEG") != "1" {
		t.Skip("set ZOMBIE_TEST_AIRPLAY_FFMPEG=1 for FFmpeg bridge fixture")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	for _, withAudio := range []bool{false, true} {
		name := "video_only"
		if withAudio {
			name = "video_and_audio"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 16*time.Second)
			defer cancel()
			stateDir := t.TempDir()
			hlsDir := filepath.Join(stateDir, "hls")
			if err := os.Mkdir(hlsDir, 0700); err != nil {
				t.Fatal(err)
			}
			ports := mirrorPorts{freeMirrorUDPPort(t), freeMirrorUDPPort(t), freeMirrorUDPPort(t), freeMirrorUDPPort(t)}
			b, err := newMirrorBridge(hlsDir, stateDir, ffmpeg, ports)
			if err != nil {
				t.Fatal(err)
			}
			bridgeDone := make(chan error, 1)
			go func() { bridgeDone <- b.run(ctx) }()
			videoSender := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=15", "-t", "9", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-g", "15", "-payload_type", "96", "-f", "rtp", "rtp://127.0.0.1:"+strconv.Itoa(ports.videoIn))
			if err := videoSender.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = videoSender.Process.Kill(); _ = videoSender.Wait() }()
			if withAudio {
				audioSender := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "9", "-ac", "2", "-c:a", "pcm_s16be", "-payload_type", "97", "-f", "rtp", "rtp://127.0.0.1:"+strconv.Itoa(ports.audioIn))
				if err := audioSender.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = audioSender.Process.Kill(); _ = audioSender.Wait() }()
			}
			wanted := "video"
			if withAudio {
				wanted = "audio"
			}
			deadline := time.After(12 * time.Second)
			for {
				select {
				case err := <-bridgeDone:
					t.Fatalf("bridge stopped before HLS output: %v", err)
				case <-deadline:
					t.Fatal("no playable HLS segment from synthetic RTP")
				case <-time.After(250 * time.Millisecond):
				}
				manifest, err := os.ReadFile(filepath.Join(hlsDir, "index.m3u8"))
				if err != nil || !strings.Contains(string(manifest), ".ts") {
					continue
				}
				for _, line := range strings.Split(string(manifest), "\n") {
					name := strings.TrimSpace(line)
					if !mirrorSegmentName(name) {
						continue
					}
					probe := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "csv=p=0", filepath.Join(hlsDir, name))
					output, err := probe.Output()
					if err == nil && strings.Contains(string(output), wanted) && strings.Contains(string(output), "h264") {
						cancel()
						if err := <-bridgeDone; err != nil {
							t.Fatal(err)
						}
						return
					}
				}
			}
		})
	}
}

func TestMirrorBridgeSyntheticAudioTransition(t *testing.T) {
	if os.Getenv("ZOMBIE_TEST_AIRPLAY_FFMPEG") != "1" {
		t.Skip("set ZOMBIE_TEST_AIRPLAY_FFMPEG=1 for FFmpeg bridge fixture")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 26*time.Second)
	defer cancel()
	stateDir := t.TempDir()
	hlsDir := filepath.Join(stateDir, "hls")
	if err := os.Mkdir(hlsDir, 0700); err != nil {
		t.Fatal(err)
	}
	ports := mirrorPorts{freeMirrorUDPPort(t), freeMirrorUDPPort(t), freeMirrorUDPPort(t), freeMirrorUDPPort(t)}
	b, err := newMirrorBridge(hlsDir, stateDir, ffmpeg, ports)
	if err != nil {
		t.Fatal(err)
	}
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- b.run(ctx) }()
	videoSender := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=15", "-t", "19", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-g", "15", "-payload_type", "96", "-f", "rtp", "rtp://127.0.0.1:"+strconv.Itoa(ports.videoIn))
	if err := videoSender.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = videoSender.Process.Kill(); _ = videoSender.Wait() }()
	waitMirrorSegment(t, ctx, bridgeDone, hlsDir, ffprobe, false, "")
	audioSender := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "3", "-ac", "2", "-c:a", "pcm_s16be", "-payload_type", "97", "-f", "rtp", "rtp://127.0.0.1:"+strconv.Itoa(ports.audioIn))
	if err := audioSender.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = audioSender.Process.Kill(); _ = audioSender.Wait() }()
	avSegment := waitMirrorSegment(t, ctx, bridgeDone, hlsDir, ffprobe, true, "")
	waitMirrorSegment(t, ctx, bridgeDone, hlsDir, ffprobe, false, avSegment)
	cancel()
	if err := <-bridgeDone; err != nil {
		t.Fatal(err)
	}
}

func waitMirrorSegment(t *testing.T, ctx context.Context, bridgeDone <-chan error, hlsDir, ffprobe string, wantAudio bool, afterSegment string) string {
	t.Helper()
	for {
		select {
		case err := <-bridgeDone:
			t.Fatalf("bridge stopped during transition: %v", err)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for audio=%v after %s", wantAudio, afterSegment)
		case <-time.After(200 * time.Millisecond):
		}
		manifest, err := os.ReadFile(filepath.Join(hlsDir, "index.m3u8"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(manifest), "\n") {
			name := strings.TrimSpace(line)
			if !mirrorSegmentName(name) || name == afterSegment {
				continue
			}
			probe := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", filepath.Join(hlsDir, name))
			output, err := probe.Output()
			if err == nil && strings.Contains(string(output), "video") && strings.Contains(string(output), "audio") == wantAudio {
				return name
			}
		}
	}
}

func freeMirrorUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}
