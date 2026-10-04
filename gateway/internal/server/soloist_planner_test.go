package server

import (
	"context"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
)

func TestSoloistPlannerCannotBypassPCMDeviceProbe(t *testing.T) {
	source := domain.Source{Item: domain.Item{Provider: "spotify", Kind: "audio"}, URL: "http://worker.invalid/audio", MIME: media.PCMStreamMIME, Live: true, RawPCM: true, PCMFormat: "s16le"}
	server := &Server{}
	device := domain.Device{ID: "synthetic-device"}
	for _, mode := range []string{"", "AUTO", "TRANSCODE", "DIRECT_PLAY", "EXTERNAL_PLAYER", "REMUX", "RAW_PCM"} {
		if _, err := server.playbackMode(context.Background(), source, device, mode); err == nil {
			t.Fatalf("mode %s bypassed absent device evidence", mode)
		}
	}
	device.Capabilities = domain.Capabilities{SuiteVersion: 2, CacheKey: devices.ProbeCacheKey(device), Probes: []domain.Probe{{ID: "audio-track-pcm-stream", Status: "PASS", PositionMS: 700, TestedAt: time.Now().Unix()}}}
	for _, mode := range []string{"", "AUTO", "TRANSCODE"} {
		decision, err := server.playbackMode(context.Background(), source, device, mode)
		if err != nil || decision.mode != "RAW_PCM" {
			t.Fatalf("fresh PCM capability rejected: %s %v", mode, err)
		}
	}
	source.PCMFormat = "f32le"
	if _, err := server.playbackMode(context.Background(), source, device, ""); err == nil {
		t.Fatal("unknown PCM format admitted")
	}
}
