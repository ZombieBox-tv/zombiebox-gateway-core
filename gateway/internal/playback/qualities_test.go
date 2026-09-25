package playback

import (
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func qualitiesWithFreshEvidence(metadata domain.Metadata, source domain.Source, device domain.Device, selected string) domain.QualityInventory {
	device.Capabilities.Probes = append([]domain.Probe(nil), device.Capabilities.Probes...)
	for i := range device.Capabilities.Probes {
		if device.Capabilities.Probes[i].Status == "PASS" && device.Capabilities.Probes[i].TestedAt == 0 {
			device.Capabilities.Probes[i].TestedAt = time.Now().Unix()
		}
	}
	return Qualities(metadata, source, device, selected)
}

func TestQualitiesRejectsUndatedPassEvidence(t *testing.T) {
	metadata := domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "h264", Width: 1280, Height: 720},
		{Type: "audio", Codec: "aac"},
	}}
	source := domain.Source{Item: domain.Item{Kind: "video", Provider: "youtube"}, MIME: "video/mp4"}
	device := domain.Device{Capabilities: domain.Capabilities{Probes: []domain.Probe{
		{ID: "http-fmp4", Status: "PASS"},
		{ID: "aac", Status: "PASS"},
		{ID: "h264-720-main", Status: "PASS"},
	}}}
	if inventory := Qualities(metadata, source, device, ""); HasQuality(inventory, "720p") {
		t.Fatalf("undated PASS cannot establish usable quality: %+v", inventory.Options)
	}
	for i := range device.Capabilities.Probes {
		device.Capabilities.Probes[i].TestedAt = time.Now().Unix()
	}
	if inventory := Qualities(metadata, source, device, ""); !HasQuality(inventory, "720p") {
		t.Fatalf("dated current PASS should establish quality: %+v", inventory.Options)
	}
}

func TestQualitiesSourceBoundingAndDeviceCapabilities(t *testing.T) {
	meta1080 := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "High", Width: 1920, Height: 1080},
			{Type: "audio", Codec: "aac"},
		},
	}
	srcVideo := domain.Source{Item: domain.Item{ID: "yt-1", Provider: "youtube", Kind: "video", Playable: true}}

	// 1. API 13 device without 1080p probe evidence: 1080p must NOT be offered.
	api13Device := domain.Device{
		Registration: domain.Registration{Platform: domain.Platform{AndroidAPI: 13}},
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-baseline-480", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS"},
		}},
	}
	inv := qualitiesWithFreshEvidence(meta1080, srcVideo, api13Device, "")
	if HasQuality(inv, "1080p") {
		t.Fatalf("1080p should not be offered without passing 1080p probe: %+v", inv.Options)
	}
	if HasQuality(inv, "2160p") || HasQuality(inv, "1440p") {
		t.Fatalf("2160p/1440p must never be fabricated: %+v", inv.Options)
	}
	if !HasQuality(inv, "720p") || !HasQuality(inv, "480p") || !HasQuality(inv, "360p") || !HasQuality(inv, "240p") || !HasQuality(inv, "144p") || !HasQuality(inv, "auto") {
		t.Fatalf("expected 720p, 480p, 360p, 240p, 144p, auto: %+v", inv.Options)
	}

	// 2. Capable device with 1080p probe PASS: 1080p IS offered.
	capableDevice := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-baseline-480", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS"},
			{ID: "h264-1080-high", Status: "PASS"},
		}},
	}
	invCapable := qualitiesWithFreshEvidence(meta1080, srcVideo, capableDevice, "")
	if !HasQuality(invCapable, "1080p") {
		t.Fatalf("1080p should be offered for passing probe: %+v", invCapable.Options)
	}
	if HasQuality(invCapable, "2160p") || HasQuality(invCapable, "1440p") {
		t.Fatalf("2160p/1440p must not be fabricated on 1080p source: %+v", invCapable.Options)
	}

	// 3. Source is 720p: 1080p must NOT be offered regardless of device capability.
	meta720 := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "High", Width: 1280, Height: 720},
			{Type: "audio", Codec: "aac"},
		},
	}
	inv720 := qualitiesWithFreshEvidence(meta720, srcVideo, capableDevice, "")
	if HasQuality(inv720, "1080p") || HasQuality(inv720, "2160p") || HasQuality(inv720, "1440p") {
		t.Fatalf("1080p/2160p/1440p should not be offered for 720p source: %+v", inv720.Options)
	}
	if !HasQuality(inv720, "720p") || !HasQuality(inv720, "480p") {
		t.Fatalf("expected 720p and 480p: %+v", inv720.Options)
	}

	// 4. Source is 480p: 720p and 1080p must not be offered.
	meta480 := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Width: 854, Height: 480},
			{Type: "audio", Codec: "aac"},
		},
	}
	inv480 := qualitiesWithFreshEvidence(meta480, srcVideo, capableDevice, "")
	if HasQuality(inv480, "720p") || HasQuality(inv480, "1080p") {
		t.Fatalf("qualities higher than 480p must not be offered: %+v", inv480.Options)
	}
	if !HasQuality(inv480, "480p") || !HasQuality(inv480, "360p") {
		t.Fatalf("expected 480p and 360p: %+v", inv480.Options)
	}
}

func TestQualities720ProbeOutranksLowMemoryHintButCannotInventDecode(t *testing.T) {
	metadata := domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "h264", Width: 1920, Height: 1080},
		{Type: "audio", Codec: "aac"},
	}}
	source := domain.Source{Item: domain.Item{ID: "video-1", Provider: "youtube", Kind: "video"}, MIME: "video/mp4"}
	device := domain.Device{
		Registration: domain.Registration{
			Display: domain.Display{Width: 1826, Height: 1026},
			Memory:  domain.Memory{PhysicalMB: 628},
		},
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS"},
		}},
	}
	verified := qualitiesWithFreshEvidence(metadata, source, device, "")
	if !HasQuality(verified, "720p") {
		t.Fatalf("recent 720p PASS should outrank a low-memory hint: %+v", verified.Options)
	}
	if HasQuality(verified, "1080p") {
		t.Fatalf("1026px output must not advertise 1080p: %+v", verified.Options)
	}
	device.Capabilities.Probes[2].Status = "FAIL"
	failed := qualitiesWithFreshEvidence(metadata, source, device, "")
	if HasQuality(failed, "720p") {
		t.Fatalf("failed decode probe must not advertise 720p: %+v", failed.Options)
	}
	device.Capabilities.Probes = device.Capabilities.Probes[:2]
	missing := qualitiesWithFreshEvidence(metadata, source, device, "")
	if HasQuality(missing, "720p") {
		t.Fatalf("missing decode probe must not advertise 720p: %+v", missing.Options)
	}
}

func TestQualitiesPositivePassRequired(t *testing.T) {
	meta := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Width: 1920, Height: 1080, Codec: "h264"},
			{Type: "audio", Codec: "aac"},
		},
	}
	src := domain.Source{Item: domain.Item{ID: "movie-1", Provider: "stremio", Kind: "movie"}}

	// 1. Missing http-fmp4: cannot transcode downscale tiers
	noFmp4Device := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS"},
		}},
	}
	invNoFmp4 := qualitiesWithFreshEvidence(meta, src, noFmp4Device, "")
	if HasQuality(invNoFmp4, "720p") {
		t.Fatalf("720p transcode should not be offered when http-fmp4 is missing: %+v", invNoFmp4.Options)
	}

	// 2. Missing aac: cannot transcode
	noAACDevice := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS"},
		}},
	}
	invNoAAC := qualitiesWithFreshEvidence(meta, src, noAACDevice, "")
	if HasQuality(invNoAAC, "720p") {
		t.Fatalf("720p transcode should not be offered when aac probe is missing: %+v", invNoAAC.Options)
	}

	// 3. UNKNOWN 720p probe status: must not be treated as selectable
	unknown720Device := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-baseline-480", Status: "PASS"},
			{ID: "h264-720-main", Status: "UNKNOWN"},
		}},
	}
	invUnknown720 := qualitiesWithFreshEvidence(meta, src, unknown720Device, "")
	if HasQuality(invUnknown720, "720p") {
		t.Fatalf("720p should not be offered for UNKNOWN probe status: %+v", invUnknown720.Options)
	}
	if !HasQuality(invUnknown720, "480p") {
		t.Fatalf("480p should be offered for passing probe: %+v", invUnknown720.Options)
	}

	// 4. Stale probe (TestedAt > 7 days ago): cannot manufacture support
	staleDevice := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-baseline-480", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS", TestedAt: time.Now().Unix() - 8*24*60*60},
		}},
	}
	invStale := qualitiesWithFreshEvidence(meta, src, staleDevice, "")
	if HasQuality(invStale, "720p") {
		t.Fatalf("720p should not be offered for stale probe: %+v", invStale.Options)
	}

	// 5. Stalled probe: cannot manufacture support
	stalledDevice := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS", Stalled: true},
		}},
	}
	invStalled := qualitiesWithFreshEvidence(meta, src, stalledDevice, "")
	if HasQuality(invStalled, "720p") {
		t.Fatalf("720p should not be offered for stalled probe: %+v", invStalled.Options)
	}

	// 6. Failed probe: cannot manufacture support
	failDevice := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-720-main", Status: "FAIL"},
		}},
	}
	invFail := qualitiesWithFreshEvidence(meta, src, failDevice, "")
	if HasQuality(invFail, "720p") {
		t.Fatalf("720p should not be offered when probe FAILs: %+v", invFail.Options)
	}
}

func TestQualitiesMatchH264FFmpegOutputCodec(t *testing.T) {
	// Source is 1440p HEVC
	meta := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "hevc", Width: 2560, Height: 1440},
			{Type: "audio", Codec: "aac"},
		},
	}
	src := domain.Source{Item: domain.Item{ID: "movie-1440", Provider: "local", Kind: "movie"}}

	// Device has passing HEVC 1080p probe, but NOT H.264 1080p probe.
	// Because gateway transcodes down to H.264, hevc-1080-main does NOT prove H.264 1080p transcode!
	hevcOnlyDevice := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS"},
			{ID: "hevc-1080-main", Status: "PASS"},
		}},
	}
	inv := qualitiesWithFreshEvidence(meta, src, hevcOnlyDevice, "")
	if HasQuality(inv, "1080p") {
		t.Fatalf("1080p transcode should not be offered based on hevc-1080-main probe: %+v", inv.Options)
	}
	if !HasQuality(inv, "720p") {
		t.Fatalf("720p should still be offered: %+v", inv.Options)
	}

	// When h264-1080-high is PASS, 1080p transcode IS offered!
	h264CapableDevice := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS"},
			{ID: "h264-1080-high", Status: "PASS"},
		}},
	}
	invH264 := qualitiesWithFreshEvidence(meta, src, h264CapableDevice, "")
	if !HasQuality(invH264, "1080p") {
		t.Fatalf("1080p transcode should be offered when h264-1080-high passes: %+v", invH264.Options)
	}
}

func TestQualitiesNativeDirectPlayPreservedWithoutTranscode(t *testing.T) {
	// Source is 1080p MP4 with AAC audio that can direct play
	meta := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "High", Width: 1920, Height: 1080},
			{Type: "audio", Codec: "aac"},
		},
	}
	meta.Format.Name = "mp4"
	src := domain.Source{MIME: "video/mp4", Item: domain.Item{ID: "direct-1", Provider: "local", Kind: "movie"}}

	// Device has 1080p decode and AAC audio PASS, but lacks http-fmp4 (transcode unsupported)
	noTranscodeDevice := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "aac", Status: "PASS"},
			{ID: "h264-1080-high", Status: "PASS"},
		}},
	}

	inv := qualitiesWithFreshEvidence(meta, src, noTranscodeDevice, "")
	// 1080p native direct play MUST NOT be filtered out solely for lack of transcode evidence!
	if !HasQuality(inv, "1080p") {
		t.Fatalf("native direct-play quality 1080p should be offered even without transcode evidence: %+v", inv.Options)
	}
	// Downscaled tiers (720p, 480p) must NOT be offered without transcode evidence
	if HasQuality(inv, "720p") || HasQuality(inv, "480p") {
		t.Fatalf("downscale tiers should not be offered without transcode evidence: %+v", inv.Options)
	}
}

func TestQualities1440pAnd4KOutputEvidence(t *testing.T) {
	meta1440 := domain.Metadata{
		Streams: []domain.Stream{
			{
				Type: "video", Codec: "hevc", Profile: "Main", Level: 150, CodecTag: "hvc1",
				Width: 2560, Height: 1440, PixelFormat: "yuv420p",
				ColorTransfer: "bt709", ColorPrimaries: "bt709",
				FrameRate: "30/1", AverageFrameRate: "30/1",
			},
			{Type: "audio", Codec: "aac"},
		},
	}
	meta1440.Format.Name = "mp4"
	meta1440.Format.BitRate = "8000000"
	src1440 := domain.Source{MIME: "video/mp4", Item: domain.Item{ID: "vid-1440", Provider: "local", Kind: "movie"}}

	// 1. Device with 1080p display panel: 1440p must NOT be offered (impossible output)
	panel1080Device := domain.Device{
		Registration: domain.Registration{
			Display: domain.Display{Width: 1920, Height: 1080},
		},
		Capabilities: domain.Capabilities{
			SuiteVersion: 2,
			CacheKey:     "synth-1440",
			Probes: []domain.Probe{
				{ID: "hevc-2160-main", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
				{ID: "aac", Status: "PASS"},
				{ID: "http-fmp4", Status: "PASS"},
				{ID: "h264-baseline-360", Status: "PASS"},
				{ID: "h264-720-main", Status: "PASS"},
				{ID: "h264-1080-high", Status: "PASS"},
			},
		},
	}
	inv1080Panel := qualitiesWithFreshEvidence(meta1440, src1440, panel1080Device, "")
	if HasQuality(inv1080Panel, "1440p") {
		t.Fatalf("1440p should not be offered when display output is only 1080p: %+v", inv1080Panel.Options)
	}
	if !HasQuality(inv1080Panel, "1080p") {
		t.Fatalf("1080p downscale should be offered: %+v", inv1080Panel.Options)
	}

	// 2. Device with 1440p capable display and passing UHD probe: 1440p IS offered!
	panel1440Device := domain.Device{
		Registration: domain.Registration{
			Display: domain.Display{Width: 2560, Height: 1440},
		},
		Capabilities: domain.Capabilities{
			SuiteVersion: 2,
			CacheKey:     "synth-1440",
			Probes: []domain.Probe{
				{ID: "hevc-2160-main", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
				{ID: "aac", Status: "PASS"},
			},
		},
	}
	inv1440 := qualitiesWithFreshEvidence(meta1440, src1440, panel1440Device, "")
	if !HasQuality(inv1440, "1440p") {
		t.Fatalf("1440p should be offered for verified 1440p source on capable display: %+v", inv1440.Options)
	}

	// 3. 4K source on 1080p display: 4K must NOT be offered (impossible 4K option)
	meta4K := domain.Metadata{
		Streams: []domain.Stream{
			{
				Type: "video", Codec: "hevc", Profile: "Main", Level: 150, CodecTag: "hvc1",
				Width: 3840, Height: 2160, PixelFormat: "yuv420p",
				ColorTransfer: "bt709", ColorPrimaries: "bt709",
				FrameRate: "30/1", AverageFrameRate: "30/1",
			},
			{Type: "audio", Codec: "aac"},
		},
	}
	meta4K.Format.Name = "mp4"
	meta4K.Format.BitRate = "10000000"
	src4K := domain.Source{MIME: "video/mp4", Item: domain.Item{ID: "vid-4k", Provider: "local", Kind: "movie"}}

	inv4KPanel1080 := qualitiesWithFreshEvidence(meta4K, src4K, panel1080Device, "")
	if HasQuality(inv4KPanel1080, "2160p") {
		t.Fatalf("2160p should not be offered when display output is only 1080p: %+v", inv4KPanel1080.Options)
	}

	// 4. Absent source resolution yields Auto only
	metaNoRes := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Width: 0, Height: 0},
		},
	}
	invNoRes := qualitiesWithFreshEvidence(metaNoRes, src1440, panel1440Device, "")
	if len(invNoRes.Options) != 1 || invNoRes.Options[0].ID != "auto" {
		t.Fatalf("absent source resolution should yield Auto only: %+v", invNoRes.Options)
	}
}

func TestQualitiesProviderAndLiveLimits(t *testing.T) {
	meta := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Width: 1920, Height: 1080},
		},
	}
	device := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "h264-1080-high", Status: "PASS"},
		}},
	}

	// Live source offers Auto only
	liveSrc := domain.Source{Live: true, Item: domain.Item{ID: "live-1", Provider: "iptv", Kind: "channel"}}
	invLive := qualitiesWithFreshEvidence(meta, liveSrc, device, "")
	if len(invLive.Options) != 1 || invLive.Options[0].ID != "auto" {
		t.Fatalf("live source should only offer Auto: %+v", invLive.Options)
	}

	// Audio track offers Auto only
	audioSrc := domain.Source{Item: domain.Item{ID: "audio-1", Provider: "local", Kind: "track"}}
	invAudio := qualitiesWithFreshEvidence(meta, audioSrc, device, "")
	if len(invAudio.Options) != 1 || invAudio.Options[0].ID != "auto" {
		t.Fatalf("audio track should only offer Auto: %+v", invAudio.Options)
	}

	// IPTV provider offers Auto only
	iptvSrc := domain.Source{Item: domain.Item{ID: "iptv-1", Provider: "iptv", Kind: "video"}}
	invIPTV := qualitiesWithFreshEvidence(meta, iptvSrc, device, "")
	if len(invIPTV.Options) != 1 || invIPTV.Options[0].ID != "auto" {
		t.Fatalf("iptv provider should only offer Auto: %+v", invIPTV.Options)
	}
}

func TestQualities4KEvidenceRequired(t *testing.T) {
	meta4K := domain.Metadata{
		Streams: []domain.Stream{
			{
				Type: "video", Codec: "hevc", Profile: "Main", Level: 150, CodecTag: "hvc1",
				Width: 3840, Height: 2160, PixelFormat: "yuv420p",
				ColorTransfer: "bt709", ColorPrimaries: "bt709",
				FrameRate: "30/1", AverageFrameRate: "30/1",
			},
			{Type: "audio", Codec: "aac"},
		},
	}
	meta4K.Format.Name = "mp4"
	meta4K.Format.BitRate = "10000000"
	src := domain.Source{MIME: "video/mp4", Item: domain.Item{ID: "movie-4k", Provider: "local", Kind: "movie"}}

	// Without valid 4K probe evidence: 2160p is NOT offered
	unverifiedDevice := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "h264-1080-high", Status: "PASS"},
		}},
	}
	invUnverified := qualitiesWithFreshEvidence(meta4K, src, unverifiedDevice, "")
	if HasQuality(invUnverified, "2160p") {
		t.Fatalf("2160p must not be offered without verified 4K probe: %+v", invUnverified.Options)
	}

	// With valid 4K probe evidence (suiteVersion 2, fresh PASS): 2160p IS offered
	verifiedDevice := domain.Device{
		Capabilities: domain.Capabilities{
			SuiteVersion: 2,
			CacheKey:     "synthetic-4k-probe",
			Probes: []domain.Probe{
				{
					ID:         "hevc-2160-main",
					Status:     "PASS",
					PositionMS: 600,
					TestedAt:   time.Now().Unix(),
				},
				{ID: "aac", Status: "PASS"},
				{ID: "h264-1080-high", Status: "PASS"},
			},
		},
	}
	invVerified := qualitiesWithFreshEvidence(meta4K, src, verifiedDevice, "")
	if !HasQuality(invVerified, "2160p") {
		t.Fatalf("2160p should be offered with verified 4K probe: %+v", invVerified.Options)
	}
}

func TestSelectedQualityModeAndPositionPreservation(t *testing.T) {
	meta := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "High", Width: 1920, Height: 1080},
			{Type: "audio", Codec: "aac"},
		},
	}
	meta.Format.Name = "mp4"
	src := domain.Source{MIME: "video/mp4", Item: domain.Item{ID: "movie-1", Provider: "local", Kind: "movie"}}
	device := domain.Device{
		Capabilities: domain.Capabilities{Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS"},
			{ID: "aac", Status: "PASS"},
			{ID: "h264-baseline-360", Status: "PASS"},
			{ID: "h264-baseline-480", Status: "PASS"},
			{ID: "h264-720-main", Status: "PASS"},
			{ID: "h264-1080-high", Status: "PASS"},
		}},
	}

	// Downscaled quality (720p on 1080p source) requires TRANSCODE
	mode, q := SelectedQualityMode(meta, src, device, "720p", 15000, "DIRECT_PLAY")
	if mode != "TRANSCODE" || q != "720p" {
		t.Fatalf("expected TRANSCODE 720p, got %s %s", mode, q)
	}

	// Auto mode preserves native DIRECT_PLAY
	autoMode, autoQ := SelectedQualityMode(meta, src, device, "auto", 0, "DIRECT_PLAY")
	if autoMode != "DIRECT_PLAY" || autoQ != "" {
		t.Fatalf("expected DIRECT_PLAY in auto, got %s %s", autoMode, autoQ)
	}

	// Auto mode with position > 0 on a remux source switches to TRANSCODE for resume accuracy
	splitSrc := domain.Source{MIME: "video/mp4", AudioURL: "https://example.com/audio", Item: domain.Item{ID: "yt-1", Provider: "youtube", Kind: "video"}}
	remuxMode, _ := SelectedQualityMode(meta, splitSrc, device, "auto", 15000, "REMUX")
	if remuxMode != "TRANSCODE" {
		t.Fatalf("expected TRANSCODE for accurate non-zero resume on adaptive source, got %s", remuxMode)
	}

	// RequiresTranscodeForQuality check
	if !RequiresTranscodeForQuality(meta, "720p") {
		t.Fatal("720p on 1080p source should require transcoding")
	}
	if RequiresTranscodeForQuality(meta, "1080p") {
		t.Fatal("1080p on 1080p source should not require downscale transcoding")
	}
	if RequiresTranscodeForQuality(meta, "auto") {
		t.Fatal("auto should not require forced downscale transcoding")
	}
}
