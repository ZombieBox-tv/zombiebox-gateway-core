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

// Pure tier boundaries, never-raise semantics, unknown goodput, and low sample rejection.
// NOTE: Goodput measures gateway->TV HLS leg, not phone->gateway RTSP; this is a
// conservative initial grant cap, not full runtime congestion control.
func TestCapCastProfileForNetworkPureTierBoundaries(t *testing.T) {
	p4K := CastVideo{"h264", 3840, 2160, 30, 12000000}
	p1080 := CastVideo{"h264", 1920, 1080, 30, 4000000}
	p720 := CastVideo{"h264", 1280, 720, 30, 2000000}
	p480 := CastVideo{"h264", 854, 480, 24, 1200000}
	p360 := CastVideo{"h264", 640, 360, 24, 800000}

	// 1. Pure tier boundaries from 4K candidate
	// 4K fits (budget >= 12,000,000)
	c, err := CapCastProfileForNetwork(p4K, 20000) // 14 Mbps budget
	if err != nil || c != p4K {
		t.Fatalf("expected 4K unchanged, got %v, err: %v", c, err)
	}
	c, err = CapCastProfileForNetwork(p4K, 17143) // 12,000,100 budget
	if err != nil || c != p4K {
		t.Fatalf("expected 4K at threshold, got %v, err: %v", c, err)
	}
	// Just below 4K threshold -> caps to 1080p
	c, err = CapCastProfileForNetwork(p4K, 17142) // 11,999,400 budget
	if err != nil || c != p1080 {
		t.Fatalf("expected 1080p below 4K threshold, got %v, err: %v", c, err)
	}
	c, err = CapCastProfileForNetwork(p4K, 6000) // 4.2 Mbps budget
	if err != nil || c != p1080 {
		t.Fatalf("expected 1080p, got %v, err: %v", c, err)
	}
	c, err = CapCastProfileForNetwork(p4K, 5715) // 4,000,500 budget
	if err != nil || c != p1080 {
		t.Fatalf("expected 1080p at threshold, got %v, err: %v", c, err)
	}
	// Just below 1080p threshold -> caps to 720p
	c, err = CapCastProfileForNetwork(p4K, 5714) // 3,999,800 budget
	if err != nil || c != p720 {
		t.Fatalf("expected 720p below 1080p threshold, got %v, err: %v", c, err)
	}
	c, err = CapCastProfileForNetwork(p4K, 3000) // 2.1 Mbps budget
	if err != nil || c != p720 {
		t.Fatalf("expected 720p, got %v, err: %v", c, err)
	}
	c, err = CapCastProfileForNetwork(p4K, 2858) // 2,000,600 budget
	if err != nil || c != p720 {
		t.Fatalf("expected 720p at threshold, got %v, err: %v", c, err)
	}
	// Just below 720p threshold -> caps to 480p
	c, err = CapCastProfileForNetwork(p4K, 2857) // 1,999,900 budget
	if err != nil || c != p480 {
		t.Fatalf("expected 480p below 720p threshold, got %v, err: %v", c, err)
	}
	c, err = CapCastProfileForNetwork(p4K, 1800) // 1.26 Mbps budget
	if err != nil || c != p480 {
		t.Fatalf("expected 480p, got %v, err: %v", c, err)
	}
	c, err = CapCastProfileForNetwork(p4K, 1715) // 1,200,500 budget
	if err != nil || c != p480 {
		t.Fatalf("expected 480p at threshold, got %v, err: %v", c, err)
	}
	// Just below 480p threshold -> caps to 360p
	c, err = CapCastProfileForNetwork(p4K, 1714) // 1,199,800 budget
	if err != nil || c != p360 {
		t.Fatalf("expected 360p below 480p threshold, got %v, err: %v", c, err)
	}
	c, err = CapCastProfileForNetwork(p4K, 1200) // 840,000 budget
	if err != nil || c != p360 {
		t.Fatalf("expected 360p, got %v, err: %v", c, err)
	}
	c, err = CapCastProfileForNetwork(p4K, 1143) // 800,100 budget
	if err != nil || c != p360 {
		t.Fatalf("expected 360p at threshold, got %v, err: %v", c, err)
	}

	// 2. Never-raise: candidate profile limits are never raised by abundant goodput
	for _, candidate := range []CastVideo{p1080, p720, p480, p360} {
		for _, highGoodput := range []int64{20000, 50000, 200000} {
			result, err := CapCastProfileForNetwork(candidate, highGoodput)
			if err != nil {
				t.Fatalf("unexpected error for %v at %d kbps: %v", candidate, highGoodput, err)
			}
			if result.MaxHeight > candidate.MaxHeight || result.Bitrate > candidate.Bitrate || result.MaxWidth > candidate.MaxWidth || result.FPS > candidate.FPS {
				t.Fatalf("candidate %v raised by goodput %d: got %v", candidate, highGoodput, result)
			}
			if result != candidate {
				t.Fatalf("candidate %v modified by abundant goodput %d: got %v", candidate, highGoodput, result)
			}
		}
	}

	// 3. Unknown/unmeasured samples (kbps <= 0) preserve the profile untouched
	for _, candidate := range []CastVideo{p4K, p1080, p720, p480, p360} {
		for _, zeroOrNegative := range []int64{0, -1, -500} {
			result, err := CapCastProfileForNetwork(candidate, zeroOrNegative)
			if err != nil || result != candidate {
				t.Fatalf("unknown goodput %d altered candidate %v: got %v, err: %v", zeroOrNegative, candidate, result, err)
			}
		}
	}

	// 4. Low sample / insufficient goodput (< 800 kbps budget) rejects with error
	for _, candidate := range []CastVideo{p4K, p1080, p720, p480, p360} {
		for _, lowGoodput := range []int64{1142, 1000, 700, 500, 100, 1} {
			result, err := CapCastProfileForNetwork(candidate, lowGoodput)
			if err == nil {
				t.Fatalf("expected error for low goodput %d on candidate %v, got success: %v", lowGoodput, candidate, result)
			}
		}
	}
}

func TestCastProfileForModeAppliesGoodputCapAndLeavesAudioUnaffected(t *testing.T) {
	now := time.Unix(1800000000, 0)
	device := domain.Device{
		Registration: domain.Registration{
			Memory: domain.Memory{PhysicalMB: 2048},
		},
		Capabilities: domain.Capabilities{
			SuiteVersion: 2,
			CacheKey:     "bound-1080",
			Probes: []domain.Probe{
				{ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
				{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1000, TestedAt: now.Unix()},
			},
		},
	}

	// SCREEN: fresh sample of 3000 kbps caps 1080p to 720p
	video, err := CastProfileForMode(device, "SCREEN", 1080, now, 3000)
	if err != nil || video.MaxHeight != 720 || video.Bitrate != 2000000 {
		t.Fatalf("expected 720p cap, got %v, err: %v", video, err)
	}

	// SCREEN: low sample of 500 kbps rejects
	_, err = CastProfileForMode(device, "SCREEN", 1080, now, 500)
	if err == nil {
		t.Fatal("expected low goodput to reject SCREEN")
	}

	// SCREEN: unknown sample (0) preserves 1080p
	video, err = CastProfileForMode(device, "SCREEN", 1080, now, 0)
	if err != nil || video.MaxHeight != 1080 || video.Bitrate != 4000000 {
		t.Fatalf("expected 1080p uncapped, got %v, err: %v", video, err)
	}

	// AUDIO: low sample of 500 kbps does NOT reject AUDIO
	audioVideo, err := CastProfileForMode(device, "AUDIO", 720, now, 500)
	if err != nil || audioVideo != (CastVideo{}) {
		t.Fatalf("AUDIO should succeed unaffected by low goodput, got %v, err: %v", audioVideo, err)
	}

	// AUDIO: fresh goodput of 3000 kbps also unaffected
	audioVideo, err = CastProfileForMode(device, "AUDIO", 720, now, 3000)
	if err != nil || audioVideo != (CastVideo{}) {
		t.Fatalf("AUDIO should succeed unaffected, got %v, err: %v", audioVideo, err)
	}
}
