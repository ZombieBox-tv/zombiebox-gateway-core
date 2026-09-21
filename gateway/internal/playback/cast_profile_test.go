package playback

import (
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestCastBudgetUsesEvidenceAndMemoryNotDeviceModel(t *testing.T) {
	device := domain.Device{}
	profile, err := CastProfile(device)
	if err != nil || profile.MaxWidth != 640 || profile.FPS != 24 {
		t.Fatal(profile, err)
	}
	device.Capabilities.Probes = []domain.Probe{{ID: "h264-720-main", Status: "PASS"}}
	profile, _ = CastProfile(device)
	if profile.MaxWidth != 1280 {
		t.Fatal(profile)
	}
	device.Registration.Memory.PhysicalMB = 512
	profile, _ = CastProfile(device)
	if profile.MaxWidth != 640 {
		t.Fatal("low-memory budget ignored", profile)
	}
	device.Capabilities.Probes = append(device.Capabilities.Probes, domain.Probe{ID: "hls", Status: "FAIL"})
	if _, err := CastProfile(device); err == nil {
		t.Fatal("known failed transport ignored")
	}
	device.Registration.Memory.PhysicalMB = 2048
	device.Capabilities.Probes = []domain.Probe{{ID: "h264-baseline-360", Status: "FAIL"}, {ID: "h264-720-main", Status: "PASS"}}
	if profile, err := CastProfile(device); err != nil || profile.MaxWidth != 1280 {
		t.Fatal("lower-profile failure discarded working evidence", profile, err)
	}
}
