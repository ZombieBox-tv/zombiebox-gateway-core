package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/worker"
)

func TestAirplayAudioHLSTuningAllowsOnlyQAProfiles(t *testing.T) {
	tests := []struct {
		profile string
		segment string
		list    string
		valid   bool
	}{
		{profile: "", segment: "0.6", list: "4", valid: true},
		{profile: "default", segment: "0.6", list: "4", valid: true},
		{profile: "1s-4", segment: "1", list: "4", valid: true},
		{profile: "1s-10", segment: "1", list: "10", valid: true},
		{profile: "0.8-10", valid: false},
		{profile: "1s-128", valid: false},
		{profile: "1; -hls_flags delete_segments", valid: false},
		{profile: "--help", valid: false},
	}
	for _, test := range tests {
		t.Run(test.profile, func(t *testing.T) {
			tuning, err := airplayAudioHLSTuningForProfile(test.profile)
			if (err == nil) != test.valid {
				t.Fatalf("profile %q validity = %v, want %v", test.profile, err == nil, test.valid)
			}
			if !test.valid {
				return
			}
			if tuning.segmentDuration != test.segment || tuning.listSize != test.list {
				t.Fatalf("profile %q tuning = %+v", test.profile, tuning)
			}
		})
	}
}

func TestAirplayAudioRTPTargetListIncludesOnlyBoundDiagnosticListener(t *testing.T) {
	withoutListener := airplayAudioRTPTargets(false)
	if withoutListener != "127.0.0.1:35012,127.0.0.1:35014" {
		t.Fatalf("targets without listener = %q", withoutListener)
	}
	withListener := airplayAudioRTPTargets(true)
	if withListener != "127.0.0.1:35012,127.0.0.1:35014,127.0.0.1:35016" {
		t.Fatalf("targets with listener = %q", withListener)
	}
}

func TestAirplayAudioRTPDiagnosticFlagIsOptInAndExact(t *testing.T) {
	tests := []struct {
		value   string
		enabled bool
		valid   bool
	}{
		{value: "", enabled: false, valid: true},
		{value: "0", enabled: false, valid: true},
		{value: "1", enabled: true, valid: true},
		{value: "true", valid: false},
		{value: "yes", valid: false},
		{value: "1;anything", valid: false},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			enabled, err := airplayAudioRTPDiagnosticEnabled(test.value)
			if (err == nil) != test.valid || enabled != test.enabled {
				t.Fatalf("flag %q = (%v, %v), want (%v, valid=%v)", test.value, enabled, err, test.enabled, test.valid)
			}
		})
	}
}

func TestAirplayRTPSequenceWindowHandlesWrapGapsReorderDuplicatesAndReset(t *testing.T) {
	diagnostics := newAirplayAudioDiagnostics(t.TempDir())
	start := time.Unix(100, 0)
	for index, sequence := range []uint16{65534, 65535, 0, 1} {
		diagnostics.ObserveRTPPacket(testAirplayRTPPacket(sequence), start.Add(time.Duration(index)*10*time.Millisecond))
	}
	status := diagnostics.AirPlayAudioSnapshot(start.Add(time.Second))
	if status.RTPPacketCount != 4 || status.RTPSequenceGapCount != 0 || status.RTPSequenceResetCount != 0 {
		t.Fatalf("sequence wrap misclassified: %+v", status)
	}
	if status.RTPArrivalGapCount != 3 || status.RTPMaxArrivalGapMS != 10 || status.RTPLongArrivalGapCount != 0 {
		t.Fatalf("arrival timing counters incorrect: %+v", status)
	}

	diagnostics = newAirplayAudioDiagnostics(t.TempDir())
	for index, sequence := range []uint16{10, 13, 12, 12, 11} {
		diagnostics.ObserveRTPPacket(testAirplayRTPPacket(sequence), start.Add(2*time.Second+time.Duration(index)*10*time.Millisecond))
	}
	status = diagnostics.AirPlayAudioSnapshot(start.Add(3 * time.Second))
	if status.RTPSequenceGapCount != 2 || status.RTPRecoveredSequenceGapCount != 2 || status.RTPOutstandingSequenceGapCount != 0 {
		t.Fatalf("gap recovery incorrect: %+v", status)
	}
	if status.RTPDuplicatePacketCount != 1 || status.RTPReorderedPacketCount != 2 {
		t.Fatalf("reorder/duplicate classification incorrect: %+v", status)
	}

	diagnostics = newAirplayAudioDiagnostics(t.TempDir())
	for index, sequence := range []uint16{100, 4500, 4501, 2000} {
		diagnostics.ObserveRTPPacket(testAirplayRTPPacket(sequence), start.Add(4*time.Second+time.Duration(index)*300*time.Millisecond))
	}
	status = diagnostics.AirPlayAudioSnapshot(start.Add(6 * time.Second))
	if status.RTPSequenceResetCount != 1 || status.RTPSequenceGapCount != 0 || status.RTPOldPacketCount != 1 {
		t.Fatalf("reset or old packet classification incorrect: %+v", status)
	}
	if status.RTPLongArrivalGapCount != 3 {
		t.Fatalf("long arrival gaps not counted: %+v", status)
	}

	halfRangeDiagnostics := newAirplayAudioDiagnostics(t.TempDir())
	halfRangeDiagnostics.ObserveRTPPacket(testAirplayRTPPacket(200), start)
	halfRangeDiagnostics.ObserveRTPPacket(testAirplayRTPPacket(200+32768), start.Add(time.Millisecond))
	halfRangeDiagnostics.ObserveRTPPacket(testAirplayRTPPacket(200+32769), start.Add(2*time.Millisecond))
	status = halfRangeDiagnostics.AirPlayAudioSnapshot(start.Add(time.Second))
	if status.RTPSequenceResetCount != 1 || status.RTPSequenceGapCount != 0 || status.RTPPacketCount != 3 {
		t.Fatalf("ambiguous half-range jump did not reset bounded tracking: %+v", status)
	}
}

func TestAirplayOutstandingGapCounterCannotUnderflow(t *testing.T) {
	diagnostics := newAirplayAudioDiagnostics(t.TempDir())
	diagnostics.mu.Lock()
	diagnostics.rtp.hasSequence = true
	diagnostics.rtp.highestSequence = 100
	diagnostics.rtp.missing[1] = true
	diagnostics.mu.Unlock()
	diagnostics.ObserveRTPPacket(testAirplayRTPPacket(99), time.Unix(300, 0))
	status := diagnostics.AirPlayAudioSnapshot(time.Unix(301, 0))
	if status.RTPOutstandingSequenceGapCount != 0 || status.RTPRecoveredSequenceGapCount != 1 {
		t.Fatalf("outstanding gap underflowed: %+v", status)
	}
}

func TestAirplayRTPHeaderValidationAndBoundedSequenceWindow(t *testing.T) {
	diagnostics := newAirplayAudioDiagnostics(t.TempDir())
	start := time.Unix(200, 0)
	validWithCSRC := testAirplayRTPPacket(4)
	validWithCSRC[0] = 0x81
	validWithCSRC = append(validWithCSRC, 1, 2, 3, 4)
	validWithExtension := make([]byte, 16)
	validWithExtension[0] = 0x90
	validWithExtension[3] = 5
	validWithPadding := testAirplayRTPPacket(6)
	validWithPadding[0] = 0xa0
	validWithPadding = append(validWithPadding, 0, 2)
	malformed := [][]byte{
		{},
		make([]byte, 11),
		append([]byte{0x40}, make([]byte, 11)...),
		func() []byte { packet := testAirplayRTPPacket(1); packet[0] = 0x8f; return packet }(),
		func() []byte { packet := testAirplayRTPPacket(1); packet[0] = 0x90; return packet }(),
		func() []byte { packet := testAirplayRTPPacket(1); packet[0] = 0xa0; return append(packet, 0) }(),
	}
	for _, packet := range malformed {
		diagnostics.ObserveRTPPacket(packet, start)
	}
	for index, packet := range [][]byte{testAirplayRTPPacket(3), validWithCSRC, validWithExtension, validWithPadding} {
		diagnostics.ObserveRTPPacket(packet, start.Add(time.Duration(index+1)*10*time.Millisecond))
	}
	status := diagnostics.AirPlayAudioSnapshot(start.Add(time.Second))
	if status.RTPMalformedPacketCount != uint64(len(malformed)) || status.RTPPacketCount != 4 {
		t.Fatalf("header validation counters incorrect: %+v", status)
	}
	if status.RTPSequenceGapCount != 0 {
		t.Fatalf("valid optional RTP headers changed sequence accounting: %+v", status)
	}

	windowDiagnostics := newAirplayAudioDiagnostics(t.TempDir())
	windowDiagnostics.ObserveRTPPacket(testAirplayRTPPacket(100), start)
	windowDiagnostics.ObserveRTPPacket(testAirplayRTPPacket(300), start.Add(time.Millisecond))
	windowDiagnostics.ObserveRTPPacket(testAirplayRTPPacket(299), start.Add(2*time.Millisecond))
	status = windowDiagnostics.AirPlayAudioSnapshot(start.Add(time.Second))
	if status.RTPSequenceGapCount != 199 || status.RTPRecoveredSequenceGapCount != 1 || status.RTPOutstandingSequenceGapCount != 126 || status.RTPExpiredSequenceGapCount != 72 {
		t.Fatalf("sequence reorder window was not bounded: %+v", status)
	}
}

func TestAirplayRTPListenerIsLoopbackOnlyAndConsumesPackets(t *testing.T) {
	diagnostics := newAirplayAudioDiagnostics(t.TempDir())
	if err := diagnostics.StartRTPListener(context.Background(), "0.0.0.0:0"); err == nil {
		t.Fatal("listener accepted a non-loopback bind address")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := diagnostics.StartRTPListener(ctx, "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()

	diagnostics.mu.Lock()
	localAddress := diagnostics.listener.LocalAddr().(*net.UDPAddr)
	diagnostics.mu.Unlock()
	sender, err := net.DialUDP("udp4", nil, localAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	if _, err := sender.Write(testAirplayRTPPacket(77)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if diagnostics.AirPlayAudioSnapshot(time.Now()).RTPPacketCount == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("loopback RTP packet was not observed")
}

func TestAirplayAudioSnapshotReportsBoundedHLSOutputAndRequestEvidence(t *testing.T) {
	stateDir := t.TempDir()
	hlsDir := filepath.Join(stateDir, "hls")
	if err := os.MkdirAll(hlsDir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	manifest := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n" +
		"#EXTINF:0.600000,\naudio5.ts\n#EXTINF:0.600000,\naudio6.ts\n"
	if err := os.WriteFile(filepath.Join(hlsDir, "audio.m3u8"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hlsDir, "audio5.ts"), make([]byte, 500), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hlsDir, "audio6.ts"), make([]byte, 500), 0600); err != nil {
		t.Fatal(err)
	}
	playlistTime := now.Add(-200 * time.Millisecond)
	segmentTime := now.Add(-80 * time.Millisecond)
	if err := os.Chtimes(filepath.Join(hlsDir, "audio.m3u8"), playlistTime, playlistTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(hlsDir, "audio6.ts"), segmentTime, segmentTime); err != nil {
		t.Fatal(err)
	}

	diagnostics := newAirplayAudioDiagnostics(stateDir)
	diagnostics.RecordAirPlayHLSRequest(worker.AirPlayAudioPlaylistRequest, 200, 25*time.Millisecond, now)
	diagnostics.RecordAirPlayHLSRequest(worker.AirPlayAudioSegmentRequest, 404, time.Minute, now.Add(10*time.Millisecond))
	status := diagnostics.AirPlayAudioSnapshot(now.Add(time.Second))
	if !status.HLSPlaylistPresent || !status.HLSPlaylistValid || status.HLSPlaylistSegmentCount != 2 || status.HLSPlaylistMissingSegmentCount != 0 {
		t.Fatalf("playlist readiness not reported: %+v", status)
	}
	if status.HLSPlaylistSpanMS != 1200 || status.HLSPlaylistAgeMS < 190 || status.HLSLatestSegmentAgeMS < 970 {
		t.Fatalf("playlist span or output age incorrect: %+v", status)
	}
	if status.HLSPlaylistRequestCount != 1 || status.HLSPlaylistSuccessCount != 1 || status.HLSPlaylistLastStatusCode != 200 || status.HLSPlaylistLastRequestDurationMS != 25 {
		t.Fatalf("playlist request evidence incorrect: %+v", status)
	}
	if status.HLSSegmentRequestCount != 1 || status.HLSSegmentMissCount != 1 || status.HLSSegmentLastStatusCode != 404 || status.HLSSegmentLastRequestDurationMS != 30000 {
		t.Fatalf("segment miss or duration bound incorrect: %+v", status)
	}

	if err := os.WriteFile(filepath.Join(hlsDir, "audio.m3u8"), []byte("#EXTM3U\n#EXTINF:0.5,\n../secret.ts\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status = diagnostics.AirPlayAudioSnapshot(now)
	if status.HLSPlaylistValid || status.HLSPlaylistSegmentCount != 0 {
		t.Fatalf("untrusted playlist path accepted: %+v", status)
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(string(mustJSON(t, status)))), "secret") {
		t.Fatal("diagnostics retained or exposed a playlist path")
	}
}

func testAirplayRTPPacket(sequence uint16) []byte {
	packet := make([]byte, 13)
	packet[0] = 0x80
	packet[1] = 97
	packet[2] = byte(sequence >> 8)
	packet[3] = byte(sequence)
	packet[8] = 1
	packet[12] = 1
	return packet
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
