package playback

import (
	"testing"
	"time"

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

func TestCast1080RequiresOptInAndFreshAdvancingEvidence(t *testing.T) {
	now := time.Unix(1800000000, 0)
	device := domain.Device{Capabilities: domain.Capabilities{SuiteVersion: 2, CacheKey: "bound-to-device", Probes: []domain.Probe{
		{ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
		{ID: "hls", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
	}}}
	profile, err := CastProfileForSender(device, 1080, now)
	if err != nil || profile.MaxHeight != 1080 || profile.Bitrate != 4000000 {
		t.Fatal(profile, err)
	}
	for _, height := range []int{0, 720} {
		profile, err = CastProfileForSender(device, height, now)
		if err != nil || profile.MaxHeight > 720 {
			t.Fatal("legacy sender raised", profile, err)
		}
	}
	for _, mutate := range []func(*domain.Device){
		func(d *domain.Device) { d.Capabilities.Probes[0].PositionMS = 0 },
		func(d *domain.Device) { d.Capabilities.Probes[0].Stalled = true },
		func(d *domain.Device) { d.Capabilities.Probes[0].TestedAt = now.Unix() - 8*24*60*60 },
		func(d *domain.Device) { d.Capabilities.Probes[0].TestedAt = now.Unix() + 301 },
		func(d *domain.Device) { d.Capabilities.Probes[1].Status = "UNKNOWN" },
		func(d *domain.Device) { d.Capabilities.CacheKey = "" },
		func(d *domain.Device) { d.Capabilities.SuiteVersion = 1 },
		func(d *domain.Device) { d.Registration.Memory.PhysicalMB = 512 },
	} {
		candidate := device
		candidate.Capabilities.Probes = append([]domain.Probe(nil), device.Capabilities.Probes...)
		mutate(&candidate)
		profile, err = CastProfileForSender(candidate, 1080, now)
		if err != nil || profile.MaxHeight > 720 {
			t.Fatal("unevidenced 1080 grant", profile, err)
		}
	}
}
