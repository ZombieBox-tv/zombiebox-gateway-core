package server

import (
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestExtendedProbeRequiresExplicitDiscoveredCandidate(t *testing.T) {
	device := domain.Device{}
	if probeCandidate(device, "hevc-2160-main") {
		t.Fatal("missing discovery enabled UHD")
	}
	device.Registration.Hardware = &domain.HardwareReport{Decoders: []domain.CodecHint{{Types: []string{"video/hevc"}}}}
	if probeCandidate(device, "hevc-2160-main") {
		t.Fatal("MIME alone enabled UHD")
	}
	device.Registration.Hardware.Decoders[0].ProbeCandidates = []string{"hevc-1080-main"}
	if !probeCandidate(device, "hevc-1080-main") || probeCandidate(device, "hevc-2160-main") {
		t.Fatal("candidate bounds ignored")
	}
}
