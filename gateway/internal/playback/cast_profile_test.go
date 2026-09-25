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
	device.Capabilities.Probes = append(device.Capabilities.Probes, domain.Probe{ID: "hls-h264-aac", Status: "FAIL"})
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
		{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
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

func TestAudioCastDoesNotRequireVideoDecoderButRespectsAudioTransportFailures(t *testing.T) {
	device := domain.Device{Capabilities: domain.Capabilities{Probes: []domain.Probe{{ID: "h264-baseline-360", Status: "FAIL"}}}}
	if _, err := CastProfileForMode(device, "AUDIO", 720, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := CastProfileForMode(device, "SCREEN", 720, time.Now()); err == nil {
		t.Fatal("video failure ignored")
	}
	for _, id := range []string{"aac", "hls-h264-aac"} {
		device.Capabilities.Probes = []domain.Probe{{ID: id, Status: "FAIL"}}
		if _, err := CastProfileForMode(device, "AUDIO", 720, time.Now()); err == nil {
			t.Fatal("failed audio transport ignored", id)
		}
	}
	if _, err := CastProfileForMode(domain.Device{}, "MEDIA", 720, time.Now()); err == nil {
		t.Fatal("unsupported mode accepted")
	}
}

func TestCast4KRequiresOptInFreshAdvancingEvidenceAndDisplay(t *testing.T) {
	now := time.Unix(1800000000, 0)
	device := domain.Device{
		Registration: domain.Registration{
			Memory:   domain.Memory{PhysicalMB: 2048},
			Display:  domain.Display{Width: 3840, Height: 2160},
			Hardware: &domain.HardwareReport{Displays: []domain.DisplayHint{{Default: true, Modes: []domain.DisplayModeHint{{Height: 2160}}}}},
		},
		Capabilities: domain.Capabilities{
			SuiteVersion: 2,
			CacheKey:     "bound-4k",
			Probes: []domain.Probe{
				{ID: "h264-2160-high", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
				{ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
				{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
			},
		},
	}

	// 1. Verified 4K negotiation
	profile, err := CastProfileForSender(device, 2160, now)
	if err != nil || profile.MaxHeight != 2160 || profile.MaxWidth != 3840 || profile.Bitrate != 12000000 {
		t.Fatal("verified 4K grant failed", profile, err)
	}

	// 2. Lower/legacy sender requests on 4K capable device are not raised
	profile, err = CastProfileForSender(device, 1080, now)
	if err != nil || profile.MaxHeight != 1080 || profile.MaxWidth != 1920 || profile.Bitrate != 4000000 {
		t.Fatal("1080 request raised or failed on 4K device", profile, err)
	}
	for _, height := range []int{0, 720} {
		profile, err = CastProfileForSender(device, height, now)
		if err != nil || profile.MaxHeight > 720 {
			t.Fatal("legacy sender request raised on 4K device", profile, err)
		}
	}

	// 3. 1080/Vizio path: 1080p display panel receiver with 1080p probe negotiates down to 1080p
	vizio := domain.Device{
		Registration: domain.Registration{
			Memory:  domain.Memory{PhysicalMB: 1024},
			Display: domain.Display{Width: 1920, Height: 1080},
		},
		Capabilities: domain.Capabilities{
			SuiteVersion: 2,
			CacheKey:     "vizio-1080",
			Probes: []domain.Probe{
				{ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
				{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
			},
		},
	}
	profile, err = CastProfileForSender(vizio, 2160, now)
	if err != nil || profile.MaxHeight != 1080 || profile.MaxWidth != 1920 || profile.Bitrate != 4000000 {
		t.Fatal("1080/Vizio path failed to negotiate down to 1080p", profile, err)
	}

	// 4. Missing/stale/failed probes or display evidence downgrade appropriately
	for _, mutate := range []struct {
		desc string
		fn   func(*domain.Device)
		maxH int
	}{
		{"position 0", func(d *domain.Device) { d.Capabilities.Probes[0].PositionMS = 0 }, 1080},
		{"stalled", func(d *domain.Device) { d.Capabilities.Probes[0].Stalled = true }, 1080},
		{"stale probe", func(d *domain.Device) { d.Capabilities.Probes[0].TestedAt = now.Unix() - 8*24*60*60 }, 1080},
		{"future probe", func(d *domain.Device) { d.Capabilities.Probes[0].TestedAt = now.Unix() + 301 }, 1080},
		{"failed 4k probe", func(d *domain.Device) { d.Capabilities.Probes[0].Status = "FAIL" }, 1080},
		{"output mode height 1080", func(d *domain.Device) { d.Registration.Hardware.Displays[0].Modes[0].Height = 1080 }, 1080},
		{"missing display evidence", func(d *domain.Device) { d.Registration.Hardware.Displays = nil }, 1080},
		{"unknown hls", func(d *domain.Device) { d.Capabilities.Probes[2].Status = "UNKNOWN" }, 720},
		{"suite version 1", func(d *domain.Device) { d.Capabilities.SuiteVersion = 1 }, 720},
		{"empty cache key", func(d *domain.Device) { d.Capabilities.CacheKey = "" }, 720},
		{"low memory", func(d *domain.Device) { d.Registration.Memory.PhysicalMB = 512 }, 640},
	} {
		candidate := device
		candidate.Registration.Hardware = &domain.HardwareReport{Displays: []domain.DisplayHint{{Default: true, Modes: []domain.DisplayModeHint{{Height: 2160}}}}}
		candidate.Capabilities.Probes = append([]domain.Probe(nil), device.Capabilities.Probes...)
		mutate.fn(&candidate)
		profile, err = CastProfileForSender(candidate, 2160, now)
		if err != nil || profile.MaxHeight > mutate.maxH {
			t.Fatalf("mutate %s: expected max height <= %d, got %d (err: %v)", mutate.desc, mutate.maxH, profile.MaxHeight, err)
		}
	}
}
