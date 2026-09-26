package playback

import (
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
)

func TestLocalPlannerUsesEvidenceNotAndroidVersion(t *testing.T) {
	now := time.Now().Unix()
	native := media.Metadata{Streams: []media.Stream{{Type: "video", Codec: "h264", Width: 640, Height: 360}, {Type: "audio", Codec: "aac"}}}
	unsupported := media.Metadata{Streams: []media.Stream{{Type: "video", Codec: "vp9"}}}
	passingProbes := []domain.Probe{
		{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: now},
		{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: now},
		{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
	}
	for _, tc := range []struct {
		name, mime, override, want string
		metadata                   media.Metadata
		probes                     []domain.Probe
	}{
		{"native with fresh evidence", "video/mp4", "", "DIRECT_PLAY", native, passingProbes},
		{"native without evidence falls back to conversion", "video/mp4", "", "REMUX", native, nil},
		{"container", "video/x-matroska", "", "REMUX", native, passingProbes},
		{"codec", "video/webm", "", "TRANSCODE", unsupported, nil},
		{"failed fragment probe", "video/x-matroska", "", "EXTERNAL_PLAYER", native, []domain.Probe{{ID: "http-fmp4", Status: "FAIL", TestedAt: now}}},
		{"failed audio probe", "video/webm", "", "EXTERNAL_PLAYER", unsupported, []domain.Probe{{ID: "aac", Status: "FAIL", TestedAt: now}}},
		{"explicit override", "video/mp4", "TRANSCODE", "TRANSCODE", native, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LocalMode(tc.metadata, tc.mime, domain.Capabilities{Probes: tc.probes}, tc.override); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
func TestProfileEvidenceDoesNotRejectOtherProfiles(t *testing.T) {
	state := func(id string) string {
		if id == "h264-baseline-360" {
			return "PASS"
		}
		if id == "h264-720-main" {
			return "FAIL"
		}
		if id == "h264-1080-high" {
			return "PASS"
		}
		return "UNKNOWN"
	}
	if !videoCandidate(media.Stream{Profile: "Constrained Baseline", Width: 640, Height: 360}, state) {
		t.Fatal("Main failure rejected Baseline")
	}
	if videoCandidate(media.Stream{Profile: "Main", Width: 1280, Height: 720}, state) {
		t.Fatal("failed Main probe ignored")
	}
	if !videoCandidate(media.Stream{Profile: "High", Width: 1920, Height: 1080}, state) {
		t.Fatal("measured 1080 High ignored")
	}
}

func TestDetectedContainerOverridesProviderMime(t *testing.T) {
	metadata := domain.Metadata{Streams: []domain.Stream{{Type: "video", Codec: "h264", Width: 640, Height: 360}}}
	metadata.Format.Name = "matroska,webm"
	if mode := LocalMode(metadata, "video/mp4", domain.Capabilities{}, ""); mode != "REMUX" {
		t.Fatal("trusted generic provider MIME over probe", mode)
	}
}

func TestLegacyContainerDoesNotPassThroughWithNativeCodec(t *testing.T) {
	for _, format := range []string{"avi", "flv", "asf"} {
		metadata := domain.Metadata{Streams: []domain.Stream{{Type: "video", Codec: "h264", Width: 640, Height: 360}, {Type: "audio", Codec: "aac"}}}
		metadata.Format.Name = format
		if got := LocalMode(metadata, "video/mp4", domain.Capabilities{}, ""); got != "REMUX" {
			t.Fatalf("%s: %s", format, got)
		}
		caps := domain.Capabilities{Probes: []domain.Probe{{ID: "http-fmp4", Status: "FAIL", TestedAt: time.Now().Unix()}}}
		if got := LocalMode(metadata, "video/mp4", caps, ""); got != "EXTERNAL_PLAYER" {
			t.Fatalf("%s ignored fMP4 failure: %s", format, got)
		}
	}
}

func TestEvidenceGatedNativeHLSPlanning(t *testing.T) {
	now := time.Now().Unix()
	freshHLSCaps := func(edit func(*domain.Capabilities)) domain.Capabilities {
		c := domain.Capabilities{
			SuiteVersion: 2,
			CacheKey:     "cache-key-valid",
			Probes: []domain.Probe{
				{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
				{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
			},
		}
		if edit != nil {
			edit(&c)
		}
		return c
	}
	h264AACMetadata := func(edit func(*domain.Metadata)) domain.Metadata {
		m := domain.Metadata{
			Streams: []domain.Stream{
				{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
				{Type: "audio", Codec: "aac"},
			},
		}
		m.Format.Name = "hls,applehttp"
		if edit != nil {
			edit(&m)
		}
		return m
	}

	for _, tc := range []struct {
		name     string
		metadata domain.Metadata
		mime     string
		caps     domain.Capabilities
		override string
		want     string
	}{
		{
			name:     "fresh HLS PASS selects DIRECT_PLAY",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps:     freshHLSCaps(nil),
			want:     "DIRECT_PLAY",
		},
		{
			name:     "HLS probe FAIL falls back to REMUX",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(func(c *domain.Capabilities) {
				c.Probes[0].Status = "FAIL"
			}),
			want: "REMUX",
		},
		{
			name:     "HLS probe UNKNOWN falls back to REMUX",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(func(c *domain.Capabilities) {
				c.Probes[0].Status = "UNKNOWN"
			}),
			want: "REMUX",
		},
		{
			name:     "missing HLS probe falls back to REMUX",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps:     domain.Capabilities{SuiteVersion: 2, CacheKey: "valid"},
			want:     "REMUX",
		},
		{
			name:     "empty capabilities without evidence falls back to REMUX",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps:     domain.Capabilities{},
			want:     "REMUX",
		},
		{
			name:     "HLS fallback with fmp4 failure gives EXTERNAL_PLAYER",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps: domain.Capabilities{
				SuiteVersion: 2, CacheKey: "valid",
				Probes: []domain.Probe{
					{ID: "hls-h264-aac", Status: "FAIL", TestedAt: now},
					{ID: "http-fmp4", Status: "FAIL", TestedAt: now},
				},
			},
			want: "EXTERNAL_PLAYER",
		},
		{
			name: "incompatible video codec VP9 in HLS transcodes",
			metadata: h264AACMetadata(func(m *domain.Metadata) {
				m.Streams[0].Codec = "vp9"
			}),
			mime: "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(nil),
			want: "TRANSCODE",
		},
		{
			name: "incompatible video codec HEVC in HLS transcodes",
			metadata: h264AACMetadata(func(m *domain.Metadata) {
				m.Streams[0].Codec = "hevc"
			}),
			mime: "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(nil),
			want: "TRANSCODE",
		},
		{
			name: "incompatible audio codec AC3 in HLS transcodes",
			metadata: h264AACMetadata(func(m *domain.Metadata) {
				m.Streams[1].Codec = "ac3"
			}),
			mime: "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(nil),
			want: "TRANSCODE",
		},
		{
			name: "supported MP3 audio in HLS remuxes since probe was AAC only",
			metadata: h264AACMetadata(func(m *domain.Metadata) {
				m.Streams[1].Codec = "mp3"
			}),
			mime: "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(nil),
			want: "REMUX",
		},
		{
			name: "fMP4 container in HLS remuxes because TS probe does not imply fMP4",
			metadata: h264AACMetadata(func(m *domain.Metadata) {
				m.Format.Name = "hls,mp4"
			}),
			mime: "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(nil),
			want: "REMUX",
		},
		{
			name: "fMP4 codecTag in HLS stream remuxes",
			metadata: h264AACMetadata(func(m *domain.Metadata) {
				m.Streams[0].CodecTag = "avc1"
			}),
			mime: "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(nil),
			want: "REMUX",
		},
		{
			name:     "stale fingerprint empty cacheKey falls back to REMUX",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(func(c *domain.Capabilities) {
				c.CacheKey = ""
			}),
			want: "REMUX",
		},
		{
			name:     "old suiteVersion 1 falls back to REMUX",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(func(c *domain.Capabilities) {
				c.SuiteVersion = 1
			}),
			want: "REMUX",
		},
		{
			name:     "expired probe older than 7 days falls back to REMUX",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(func(c *domain.Capabilities) {
				c.Probes[0].TestedAt = now - 8*24*60*60
			}),
			want: "REMUX",
		},
		{
			name:     "stalled probe falls back to REMUX",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(func(c *domain.Capabilities) {
				c.Probes[0].Stalled = true
			}),
			want: "REMUX",
		},
		{
			name:     "prepared only probe with position zero falls back to REMUX",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps: freshHLSCaps(func(c *domain.Capabilities) {
				c.Probes[0].PositionMS = 0
				c.Probes[0].Completed = false
			}),
			want: "REMUX",
		},
		{
			name:     "explicit REMUX override respected",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps:     freshHLSCaps(nil),
			override: "REMUX",
			want:     "REMUX",
		},
		{
			name:     "explicit TRANSCODE override respected",
			metadata: h264AACMetadata(nil),
			mime:     "application/vnd.apple.mpegurl",
			caps:     freshHLSCaps(nil),
			override: "TRANSCODE",
			want:     "TRANSCODE",
		},
		{
			name:     "DASH manifest is not HLS and remains REMUX",
			metadata: h264AACMetadata(func(m *domain.Metadata) { m.Format.Name = "dash" }),
			mime:     "application/dash+xml",
			caps:     freshHLSCaps(nil),
			want:     "REMUX",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := LocalMode(tc.metadata, tc.mime, tc.caps, tc.override)
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestNativeHLSNeedsMatchingVideoProfileProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream domain.Stream
		passed string
		want   bool
	}{
		{"baseline 360 certified by HLS fixture", domain.Stream{Profile: "Constrained Baseline", Width: 640, Height: 360}, "", true},
		{"main 720 unknown", domain.Stream{Profile: "Main", Width: 1280, Height: 720}, "", false},
		{"main 720 proven", domain.Stream{Profile: "Main", Width: 1280, Height: 720}, "h264-720-main", true},
		{"high 720 unknown", domain.Stream{Profile: "High", Width: 1280, Height: 720}, "", false},
		{"high 720 proven", domain.Stream{Profile: "High", Width: 1280, Height: 720}, "h264-720-high", true},
		{"high 1080 proven", domain.Stream{Profile: "High", Width: 1920, Height: 1080}, "h264-1080-high", true},
		{"high 2160 proven", domain.Stream{Profile: "High", Width: 3840, Height: 2160}, "h264-2160-high", true},
		{"high 2160 unknown", domain.Stream{Profile: "High", Width: 3840, Height: 2160}, "", false},
		{"high 2160 with 1080 probe only", domain.Stream{Profile: "High", Width: 3840, Height: 2160}, "h264-1080-high", false},
		{"larger than 4K rejected", domain.Stream{Profile: "High", Width: 4096, Height: 2160}, "h264-2160-high", false},
		{"main 1080 has no fixture", domain.Stream{Profile: "Main", Width: 1920, Height: 1080}, "h264-720-main", false},
		{"unknown profile", domain.Stream{Profile: "", Width: 640, Height: 360}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := func(id string) string {
				if id == tc.passed {
					return "PASS"
				}
				return "UNKNOWN"
			}
			if got := hlsVideoCandidate(tc.stream, status); got != tc.want {
				t.Fatalf("candidate=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestDistinctHTTPProgressiveAndHLSResults(t *testing.T) {
	now := time.Now().Unix()
	hlsMetadata := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
			{Type: "audio", Codec: "aac"},
		},
	}
	hlsMetadata.Format.Name = "hls,applehttp"

	progressiveMetadata := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
			{Type: "audio", Codec: "aac"},
		},
	}
	progressiveMetadata.Format.Name = "mov,mp4,m4a,3gp,3g2,mj2"

	// Scenario 1: http-progressive FAIL, but hls-h264-aac PASS
	// HLS can DIRECT_PLAY, while MP4 progressive cannot DIRECT_PLAY
	caps1 := domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     "device-key",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1200, TestedAt: now},
			{ID: "http-progressive", Status: "FAIL", TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 800, TestedAt: now},
		},
	}
	if got := LocalMode(hlsMetadata, "application/vnd.apple.mpegurl", caps1, ""); got != "DIRECT_PLAY" {
		t.Fatalf("HLS should DIRECT_PLAY when HLS probe PASS, even if http-progressive FAIL: got %s", got)
	}
	if got := LocalMode(progressiveMetadata, "video/mp4", caps1, ""); got == "DIRECT_PLAY" {
		t.Fatalf("MP4 progressive must NOT DIRECT_PLAY when http-progressive FAIL: got %s", got)
	}

	// Scenario 2: http-progressive PASS, but hls-h264-aac FAIL
	// Progressive MP4 can DIRECT_PLAY, while HLS must fall back to REMUX
	caps2 := domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     "device-key",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "FAIL", TestedAt: now},
			{ID: "http-progressive", Status: "PASS", PositionMS: 1200, TestedAt: now},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1200, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 800, TestedAt: now},
		},
	}
	if got := LocalMode(progressiveMetadata, "video/mp4", caps2, ""); got != "DIRECT_PLAY" {
		t.Fatalf("MP4 progressive should DIRECT_PLAY when http-progressive PASS: got %s", got)
	}
	if got := LocalMode(hlsMetadata, "application/vnd.apple.mpegurl", caps2, ""); got != "REMUX" {
		t.Fatalf("HLS must REMUX when HLS probe FAIL: got %s", got)
	}
}

func TestProviderNeutralHLSSources(t *testing.T) {
	now := time.Now().Unix()
	capsWithEvidence := domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     "key-neutral",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
		},
	}
	capsWithoutEvidence := domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     "key-neutral",
		Probes:       []domain.Probe{},
	}

	providers := []struct {
		provider string
		format   string
		mime     string
	}{
		{provider: "iptv", format: "hls,applehttp", mime: "application/vnd.apple.mpegurl"},
		{provider: "plex", format: "hls", mime: "application/x-mpegurl"},
		{provider: "jellyfin", format: "hls,applehttp", mime: "application/vnd.apple.mpegurl"},
		{provider: "stremio", format: "hls", mime: "application/vnd.apple.mpegurl"},
	}

	for _, p := range providers {
		metadata := domain.Metadata{
			Streams: []domain.Stream{
				{Type: "video", Codec: "h264", Profile: "Main", Width: 1280, Height: 720},
				{Type: "audio", Codec: "aac"},
			},
		}
		metadata.Format.Name = p.format

		// With evidence: DIRECT_PLAY (direct beats remux only with evidence)
		if got := LocalMode(metadata, p.mime, capsWithEvidence, ""); got != "DIRECT_PLAY" {
			t.Fatalf("[%s] with evidence expected DIRECT_PLAY, got %s", p.provider, got)
		}
		// Without evidence: REMUX (missing evidence falls back to remux)
		if got := LocalMode(metadata, p.mime, capsWithoutEvidence, ""); got != "REMUX" {
			t.Fatalf("[%s] without evidence expected REMUX, got %s", p.provider, got)
		}
		// With incompatible codec: TRANSCODE (remux beats transcode when codec remains supported)
		badCodecMeta := metadata
		badCodecMeta.Streams = []domain.Stream{
			{Type: "video", Codec: "vp9", Width: 1280, Height: 720},
			{Type: "audio", Codec: "aac"},
		}
		if got := LocalMode(badCodecMeta, p.mime, capsWithEvidence, ""); got != "TRANSCODE" {
			t.Fatalf("[%s] with VP9 codec expected TRANSCODE, got %s", p.provider, got)
		}
	}
}

func TestHLSFreshMatchingVideoEvidenceVersusStaleAndFailed(t *testing.T) {
	now := time.Now().Unix()
	baseHLSCaps := domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     "valid-cache-key",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 1000, TestedAt: now},
		},
	}

	hlsStreamMeta := func(width, height int, profile string) domain.Metadata {
		m := domain.Metadata{
			Streams: []domain.Stream{
				{Type: "video", Codec: "h264", Profile: profile, Width: width, Height: height},
				{Type: "audio", Codec: "aac"},
			},
		}
		m.Format.Name = "hls,applehttp"
		return m
	}

	tests := []struct {
		name     string
		metadata domain.Metadata
		videoP   *domain.Probe
		want     string
	}{
		// 1080p High tests
		{
			name:     "1080p High fresh PASS selects DIRECT_PLAY",
			metadata: hlsStreamMeta(1920, 1080, "High"),
			videoP:   &domain.Probe{ID: "h264-1080-high", Status: "PASS", PositionMS: 1500, TestedAt: now},
			want:     "DIRECT_PLAY",
		},
		{
			name:     "1080p High stale probe older than 7 days falls back to TRANSCODE",
			metadata: hlsStreamMeta(1920, 1080, "High"),
			videoP:   &domain.Probe{ID: "h264-1080-high", Status: "PASS", PositionMS: 1500, TestedAt: now - 8*24*60*60},
			want:     "TRANSCODE",
		},
		{
			name:     "1080p High stalled probe falls back to TRANSCODE",
			metadata: hlsStreamMeta(1920, 1080, "High"),
			videoP:   &domain.Probe{ID: "h264-1080-high", Status: "PASS", Stalled: true, PositionMS: 1500, TestedAt: now},
			want:     "TRANSCODE",
		},
		{
			name:     "1080p High failed probe falls back to TRANSCODE",
			metadata: hlsStreamMeta(1920, 1080, "High"),
			videoP:   &domain.Probe{ID: "h264-1080-high", Status: "FAIL", TestedAt: now},
			want:     "TRANSCODE",
		},
		{
			name:     "1080p High absent probe falls back to TRANSCODE",
			metadata: hlsStreamMeta(1920, 1080, "High"),
			videoP:   nil,
			want:     "TRANSCODE",
		},
		{
			name:     "1080p High probe with zero position and not completed falls back to TRANSCODE",
			metadata: hlsStreamMeta(1920, 1080, "High"),
			videoP:   &domain.Probe{ID: "h264-1080-high", Status: "PASS", PositionMS: 0, Completed: false, TestedAt: now},
			want:     "TRANSCODE",
		},

		// 720p Main tests
		{
			name:     "720p Main fresh PASS selects DIRECT_PLAY",
			metadata: hlsStreamMeta(1280, 720, "Main"),
			videoP:   &domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			want:     "DIRECT_PLAY",
		},
		{
			name:     "720p Main stale probe older than 7 days falls back to REMUX",
			metadata: hlsStreamMeta(1280, 720, "Main"),
			videoP:   &domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now - 8*24*60*60},
			want:     "REMUX",
		},
		{
			name:     "720p Main stalled probe falls back to TRANSCODE",
			metadata: hlsStreamMeta(1280, 720, "Main"),
			videoP:   &domain.Probe{ID: "h264-720-main", Status: "PASS", Stalled: true, PositionMS: 1500, TestedAt: now},
			want:     "TRANSCODE",
		},
		{
			name:     "720p Main failed probe falls back to TRANSCODE",
			metadata: hlsStreamMeta(1280, 720, "Main"),
			videoP:   &domain.Probe{ID: "h264-720-main", Status: "FAIL", TestedAt: now},
			want:     "TRANSCODE",
		},
		{
			name:     "720p Main absent probe falls back to REMUX",
			metadata: hlsStreamMeta(1280, 720, "Main"),
			videoP:   nil,
			want:     "REMUX",
		},

		// 4K High tests
		{
			name:     "4K High fresh PASS selects DIRECT_PLAY",
			metadata: hlsStreamMeta(3840, 2160, "High"),
			videoP:   &domain.Probe{ID: "h264-2160-high", Status: "PASS", PositionMS: 1500, TestedAt: now},
			want:     "DIRECT_PLAY",
		},
		{
			name:     "4K High stale probe older than 7 days falls back to TRANSCODE",
			metadata: hlsStreamMeta(3840, 2160, "High"),
			videoP:   &domain.Probe{ID: "h264-2160-high", Status: "PASS", PositionMS: 1500, TestedAt: now - 8*24*60*60},
			want:     "TRANSCODE",
		},
		{
			name:     "4K High stalled probe falls back to TRANSCODE",
			metadata: hlsStreamMeta(3840, 2160, "High"),
			videoP:   &domain.Probe{ID: "h264-2160-high", Status: "PASS", Stalled: true, PositionMS: 1500, TestedAt: now},
			want:     "TRANSCODE",
		},
		{
			name:     "4K High failed probe falls back to TRANSCODE",
			metadata: hlsStreamMeta(3840, 2160, "High"),
			videoP:   &domain.Probe{ID: "h264-2160-high", Status: "FAIL", TestedAt: now},
			want:     "TRANSCODE",
		},
		{
			name:     "4K High absent probe falls back to TRANSCODE",
			metadata: hlsStreamMeta(3840, 2160, "High"),
			videoP:   nil,
			want:     "TRANSCODE",
		},

		// 480p Baseline tests
		{
			name:     "480p Baseline fresh PASS selects DIRECT_PLAY",
			metadata: hlsStreamMeta(854, 480, "Baseline"),
			videoP:   &domain.Probe{ID: "h264-baseline-480", Status: "PASS", PositionMS: 1500, TestedAt: now},
			want:     "DIRECT_PLAY",
		},
		{
			name:     "480p Baseline stale probe falls back to REMUX",
			metadata: hlsStreamMeta(854, 480, "Baseline"),
			videoP:   &domain.Probe{ID: "h264-baseline-480", Status: "PASS", PositionMS: 1500, TestedAt: now - 8*24*60*60},
			want:     "REMUX",
		},
		{
			name:     "480p Baseline failed probe falls back to TRANSCODE",
			metadata: hlsStreamMeta(854, 480, "Baseline"),
			videoP:   &domain.Probe{ID: "h264-baseline-480", Status: "FAIL", TestedAt: now},
			want:     "TRANSCODE",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := baseHLSCaps
			caps.Probes = append([]domain.Probe{}, baseHLSCaps.Probes...)
			if tc.videoP != nil {
				caps.Probes = append(caps.Probes, *tc.videoP)
			}
			got := LocalMode(tc.metadata, "application/vnd.apple.mpegurl", caps, "")
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestAdaptiveCapabilitiesLegacyAndModernMatrix(t *testing.T) {
	now := time.Now().Unix()

	// Legacy device: API 13 reference profile (e.g. Vizio Co-Star), 1GB RAM, max 720p validated, no 1080p probe, no 4K, no HEVC
	legacyCaps := domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     "vizio-costar-legacy",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1200, TestedAt: now},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1200, TestedAt: now},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1200, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: now},
		},
	}

	// Modern device: 4K UHD capable, full probe suite, fresh evidence
	modernCaps := domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     "modern-android-tv-4k",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-baseline-480", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-high", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-1080-high", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-2160-high", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "hevc-1080-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "hevc-2160-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "http-progressive", Status: "PASS", PositionMS: 1500, TestedAt: now},
		},
	}

	makeMeta := func(format, codec, profile string, width, height int, codecTag string) domain.Metadata {
		level := 51
		if codec == "hevc" {
			level = 150
		}
		m := domain.Metadata{
			Streams: []domain.Stream{
				{Type: "video", Codec: codec, Profile: profile, Level: level, Width: width, Height: height, CodecTag: codecTag, PixelFormat: "yuv420p", FrameRate: "30/1", AverageFrameRate: "30/1", ColorTransfer: "bt709", ColorPrimaries: "bt709"},
				{Type: "audio", Codec: "aac"},
			},
		}
		m.Format.Name = format
		m.Format.BitRate = "4000000"
		return m
	}

	type matrixCase struct {
		name     string
		metadata domain.Metadata
		mime     string
		caps     domain.Capabilities
		want     string
	}

	cases := []matrixCase{
		// Legacy device cases
		{
			name:     "legacy: HLS 360p Baseline MPEG-TS direct plays",
			metadata: makeMeta("hls,applehttp", "h264", "Baseline", 640, 360, ""),
			mime:     "application/vnd.apple.mpegurl",
			caps:     legacyCaps,
			want:     "DIRECT_PLAY",
		},
		{
			name:     "legacy: HLS 720p Main MPEG-TS direct plays with fresh probe",
			metadata: makeMeta("hls,applehttp", "h264", "Main", 1280, 720, ""),
			mime:     "application/vnd.apple.mpegurl",
			caps:     legacyCaps,
			want:     "DIRECT_PLAY",
		},
		{
			name:     "legacy: HLS 1080p High MPEG-TS transcodes without 1080p probe",
			metadata: makeMeta("hls,applehttp", "h264", "High", 1920, 1080, ""),
			mime:     "application/vnd.apple.mpegurl",
			caps:     legacyCaps,
			want:     "TRANSCODE",
		},
		{
			name:     "legacy: HLS 4K High transcodes",
			metadata: makeMeta("hls,applehttp", "h264", "High", 3840, 2160, ""),
			mime:     "application/vnd.apple.mpegurl",
			caps:     legacyCaps,
			want:     "TRANSCODE",
		},
		{
			name:     "legacy: HLS fMP4 container remuxes",
			metadata: makeMeta("hls,mp4", "h264", "Main", 1280, 720, "avc1"),
			mime:     "application/vnd.apple.mpegurl",
			caps:     legacyCaps,
			want:     "REMUX",
		},
		{
			name:     "legacy: HLS fMP4 with failed http-fmp4 gives EXTERNAL_PLAYER",
			metadata: makeMeta("hls,mp4", "h264", "Main", 1280, 720, "avc1"),
			mime:     "application/vnd.apple.mpegurl",
			caps: domain.Capabilities{
				SuiteVersion: 2, CacheKey: "legacy",
				Probes: []domain.Probe{
					{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1200, TestedAt: now},
					{ID: "http-fmp4", Status: "FAIL", TestedAt: now},
				},
			},
			want: "EXTERNAL_PLAYER",
		},
		{
			name:     "legacy: progressive MP4 720p Main direct plays",
			metadata: makeMeta("mov,mp4,m4a,3gp,3g2,mj2", "h264", "Main", 1280, 720, ""),
			mime:     "video/mp4",
			caps:     legacyCaps,
			want:     "DIRECT_PLAY",
		},
		{
			name:     "legacy: progressive MP4 1080p High transcodes without probe",
			metadata: makeMeta("mov,mp4,m4a,3gp,3g2,mj2", "h264", "High", 1920, 1080, ""),
			mime:     "video/mp4",
			caps:     legacyCaps,
			want:     "TRANSCODE",
		},
		{
			name:     "legacy: MKV 720p Main remuxes to MP4",
			metadata: makeMeta("matroska,webm", "h264", "Main", 1280, 720, ""),
			mime:     "video/x-matroska",
			caps:     legacyCaps,
			want:     "REMUX",
		},
		{
			name:     "legacy: raw MPEG-TS remuxes to MP4",
			metadata: makeMeta("mpegts", "h264", "Main", 1280, 720, ""),
			mime:     "video/mp2t",
			caps:     legacyCaps,
			want:     "REMUX",
		},

		// Modern device cases
		{
			name:     "modern: HLS 1080p High MPEG-TS direct plays with fresh probe",
			metadata: makeMeta("hls,applehttp", "h264", "High", 1920, 1080, ""),
			mime:     "application/vnd.apple.mpegurl",
			caps:     modernCaps,
			want:     "DIRECT_PLAY",
		},
		{
			name:     "modern: HLS 4K High MPEG-TS direct plays with fresh 4K probe (not capped at 1080p)",
			metadata: makeMeta("hls,applehttp", "h264", "High", 3840, 2160, ""),
			mime:     "application/vnd.apple.mpegurl",
			caps:     modernCaps,
			want:     "DIRECT_PLAY",
		},
		{
			name:     "modern: HLS fMP4 remuxes despite full probe suite",
			metadata: makeMeta("hls,mp4", "h264", "High", 1920, 1080, "avc1"),
			mime:     "application/vnd.apple.mpegurl",
			caps:     modernCaps,
			want:     "REMUX",
		},
		{
			name:     "modern: progressive MP4 4K HEVC direct plays with fresh evidence",
			metadata: makeMeta("mov,mp4,m4a,3gp,3g2,mj2", "hevc", "Main", 3840, 2160, "hvc1"),
			mime:     "video/mp4",
			caps:     modernCaps,
			want:     "DIRECT_PLAY",
		},
		{
			name:     "modern: progressive MP4 4K H.264 direct plays with fresh evidence",
			metadata: makeMeta("mov,mp4,m4a,3gp,3g2,mj2", "h264", "High", 3840, 2160, ""),
			mime:     "video/mp4",
			caps:     modernCaps,
			want:     "DIRECT_PLAY",
		},
		{
			name:     "modern: progressive MP4 4K HEVC with stale probe transcodes",
			metadata: makeMeta("mov,mp4,m4a,3gp,3g2,mj2", "hevc", "Main", 3840, 2160, "hvc1"),
			mime:     "video/mp4",
			caps: func() domain.Capabilities {
				c := modernCaps
				c.Probes = make([]domain.Probe, len(modernCaps.Probes))
				copy(c.Probes, modernCaps.Probes)
				for i := range c.Probes {
					if c.Probes[i].ID == "hevc-2160-main" {
						c.Probes[i].TestedAt = now - 8*24*60*60
					}
				}
				return c
			}(),
			want: "TRANSCODE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LocalMode(tc.metadata, tc.mime, tc.caps, "")
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestCrossProviderPlaybackDecisionMatrix(t *testing.T) {
	now := time.Now().Unix()
	capableCaps := domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     "provider-matrix-device",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-1080-high", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "http-fmp4", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "http-progressive", Status: "PASS", PositionMS: 1500, TestedAt: now},
		},
	}

	providers := []struct {
		name     string
		provider string
		format   string
		mime     string
		codec    string
		profile  string
		width    int
		height   int
		tag      string
		override string
		want     string
	}{
		// IPTV: HLS live channel (MPEG-TS)
		{name: "iptv: HLS live channel direct plays", provider: "iptv", format: "hls,applehttp", mime: "application/vnd.apple.mpegurl", codec: "h264", profile: "Main", width: 1280, height: 720, want: "DIRECT_PLAY"},
		// IPTV: Raw MPEG-TS stream remuxes to MP4
		{name: "iptv: raw MPEG-TS remuxes", provider: "iptv", format: "mpegts", mime: "video/mp2t", codec: "h264", profile: "Main", width: 1280, height: 720, want: "REMUX"},
		// IPTV: HLS fMP4 stream remuxes
		{name: "iptv: HLS fMP4 remuxes", provider: "iptv", format: "hls,mp4", mime: "application/vnd.apple.mpegurl", codec: "h264", profile: "Main", width: 1280, height: 720, tag: "avc1", want: "REMUX"},

		// Plex: Progressive MP4 direct plays
		{name: "plex: MP4 direct plays", provider: "plex", format: "mov,mp4,m4a,3gp,3g2,mj2", mime: "video/mp4", codec: "h264", profile: "High", width: 1920, height: 1080, want: "DIRECT_PLAY"},
		// Plex: MKV remuxes to MP4
		{name: "plex: MKV remuxes", provider: "plex", format: "matroska,webm", mime: "video/x-matroska", codec: "h264", profile: "High", width: 1920, height: 1080, want: "REMUX"},
		// Plex: HLS stream direct plays
		{name: "plex: HLS direct plays", provider: "plex", format: "hls", mime: "application/x-mpegurl", codec: "h264", profile: "High", width: 1920, height: 1080, want: "DIRECT_PLAY"},

		// Jellyfin: Progressive MP4 direct plays
		{name: "jellyfin: MP4 direct plays", provider: "jellyfin", format: "mov,mp4,m4a,3gp,3g2,mj2", mime: "video/mp4", codec: "h264", profile: "High", width: 1920, height: 1080, want: "DIRECT_PLAY"},
		// Jellyfin: MKV remuxes to MP4
		{name: "jellyfin: MKV remuxes", provider: "jellyfin", format: "matroska,webm", mime: "video/x-matroska", codec: "h264", profile: "High", width: 1920, height: 1080, want: "REMUX"},
		// Jellyfin: HLS stream direct plays
		{name: "jellyfin: HLS direct plays", provider: "jellyfin", format: "hls,applehttp", mime: "application/vnd.apple.mpegurl", codec: "h264", profile: "Main", width: 1280, height: 720, want: "DIRECT_PLAY"},

		// Stremio: Progressive MP4 direct plays
		{name: "stremio: MP4 direct plays", provider: "stremio", format: "mov,mp4,m4a,3gp,3g2,mj2", mime: "video/mp4", codec: "h264", profile: "High", width: 1920, height: 1080, want: "DIRECT_PLAY"},
		// Stremio: MKV remuxes to MP4
		{name: "stremio: MKV remuxes", provider: "stremio", format: "matroska,webm", mime: "video/x-matroska", codec: "h264", profile: "High", width: 1920, height: 1080, want: "REMUX"},
		// Stremio: AVI remuxes to MP4
		{name: "stremio: AVI remuxes", provider: "stremio", format: "avi", mime: "video/x-msvideo", codec: "h264", profile: "Main", width: 1280, height: 720, want: "REMUX"},
		// Stremio: HLS stream direct plays
		{name: "stremio: HLS direct plays", provider: "stremio", format: "hls", mime: "application/vnd.apple.mpegurl", codec: "h264", profile: "Main", width: 1280, height: 720, want: "DIRECT_PLAY"},

		// Overrides respected across providers
		{name: "stremio: explicit TRANSCODE override", provider: "stremio", format: "hls", mime: "application/vnd.apple.mpegurl", codec: "h264", profile: "Main", width: 1280, height: 720, override: "TRANSCODE", want: "TRANSCODE"},
		{name: "plex: explicit REMUX override", provider: "plex", format: "mov,mp4,m4a,3gp,3g2,mj2", mime: "video/mp4", codec: "h264", profile: "High", width: 1920, height: 1080, override: "REMUX", want: "REMUX"},
	}

	for _, tc := range providers {
		t.Run(tc.name, func(t *testing.T) {
			meta := domain.Metadata{
				Streams: []domain.Stream{
					{Type: "video", Codec: tc.codec, Profile: tc.profile, Width: tc.width, Height: tc.height, CodecTag: tc.tag},
					{Type: "audio", Codec: "aac"},
				},
			}
			meta.Format.Name = tc.format
			got := LocalMode(meta, tc.mime, capableCaps, tc.override)
			if got != tc.want {
				t.Fatalf("[%s] got %s want %s", tc.provider, got, tc.want)
			}
		})
	}
}

func TestProbeStatusFreshnessAdvancementAndTimestamps(t *testing.T) {
	now := time.Now().Unix()

	tests := []struct {
		name  string
		probe domain.Probe
		want  string
	}{
		{
			name:  "missing timestamp (TestedAt == 0) returns UNKNOWN",
			probe: domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: 0},
			want:  "UNKNOWN",
		},
		{
			name:  "no playback progress (PositionMS == 0 and not completed) returns UNKNOWN",
			probe: domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 0, Completed: false, TestedAt: now},
			want:  "UNKNOWN",
		},
		{
			name:  "minimal progress under 500ms and not completed returns UNKNOWN",
			probe: domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 200, Completed: false, TestedAt: now},
			want:  "UNKNOWN",
		},
		{
			name:  "completed playback with PositionMS 0 returns PASS",
			probe: domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 0, Completed: true, TestedAt: now},
			want:  "PASS",
		},
		{
			name:  "advancing playback (PositionMS >= 500) returns PASS",
			probe: domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 600, Completed: false, TestedAt: now},
			want:  "PASS",
		},
		{
			name:  "stale probe older than 7 days returns UNKNOWN",
			probe: domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now - 8*24*60*60},
			want:  "UNKNOWN",
		},
		{
			name:  "probe far in the future (> 300s) returns UNKNOWN",
			probe: domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now + 1000},
			want:  "UNKNOWN",
		},
		{
			name:  "slight future clock skew within 300s returns PASS",
			probe: domain.Probe{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now + 60},
			want:  "PASS",
		},
		{
			name:  "stalled probe returns FAIL",
			probe: domain.Probe{ID: "h264-720-main", Status: "PASS", Stalled: true, PositionMS: 1500, TestedAt: now},
			want:  "FAIL",
		},
		{
			name:  "explicit FAIL status returns FAIL",
			probe: domain.Probe{ID: "h264-720-main", Status: "FAIL", TestedAt: now},
			want:  "FAIL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := domain.Capabilities{Probes: []domain.Probe{tc.probe}}
			got := probeStatus(caps, tc.probe.ID, now)
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestProbeStatusDuplicateResolutionPolicy(t *testing.T) {
	now := time.Now().Unix()

	// 1. Older PASS and newer FAIL: newer FAIL must win
	capsNewerFail := domain.Capabilities{
		Probes: []domain.Probe{
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now - 3600},
			{ID: "h264-720-main", Status: "FAIL", TestedAt: now},
		},
	}
	if got := probeStatus(capsNewerFail, "h264-720-main", now); got != "FAIL" {
		t.Fatalf("expected newer FAIL to override older PASS, got %s", got)
	}

	// 2. Newer FAIL ordered first in slice, older PASS second: newer FAIL must still win
	capsNewerFailFirst := domain.Capabilities{
		Probes: []domain.Probe{
			{ID: "h264-720-main", Status: "FAIL", TestedAt: now},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now - 3600},
		},
	}
	if got := probeStatus(capsNewerFailFirst, "h264-720-main", now); got != "FAIL" {
		t.Fatalf("expected newer FAIL ordered first to override older PASS, got %s", got)
	}

	// 3. Older FAIL and newer PASS: device re-probed and passed, newer PASS must win
	capsNewerPass := domain.Capabilities{
		Probes: []domain.Probe{
			{ID: "h264-720-main", Status: "FAIL", TestedAt: now - 3600},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
		},
	}
	if got := probeStatus(capsNewerPass, "h264-720-main", now); got != "PASS" {
		t.Fatalf("expected newer PASS to override older FAIL, got %s", got)
	}

	// 4. Same timestamp: latter entry in slice represents more recent update
	capsTie := domain.Capabilities{
		Probes: []domain.Probe{
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-main", Status: "FAIL", TestedAt: now},
		},
	}
	if got := probeStatus(capsTie, "h264-720-main", now); got != "FAIL" {
		t.Fatalf("expected latter entry to win timestamp tie, got %s", got)
	}

	// 5. Credible PASS at now vs corrupt future probe at now + 5000: credible PASS must be chosen
	capsFutureCorrupt := domain.Capabilities{
		Probes: []domain.Probe{
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-main", Status: "FAIL", TestedAt: now + 5000},
		},
	}
	if got := probeStatus(capsFutureCorrupt, "h264-720-main", now); got != "PASS" {
		t.Fatalf("expected credible probe to win over corrupt future probe, got %s", got)
	}

	// 6. Only future probe exists: cannot be PASS
	capsOnlyFuture := domain.Capabilities{
		Probes: []domain.Probe{
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now + 5000},
		},
	}
	if got := probeStatus(capsOnlyFuture, "h264-720-main", now); got != "UNKNOWN" {
		t.Fatalf("expected only future probe to be UNKNOWN, got %s", got)
	}
}

func TestDirectRequiresMatchingTransportAndAudioEvidence(t *testing.T) {
	now := time.Now().Unix()

	progMeta := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
			{Type: "audio", Codec: "aac"},
		},
	}
	progMeta.Format.Name = "mov,mp4,m4a,3gp,3g2,mj2"

	hlsMeta := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
			{Type: "audio", Codec: "aac"},
		},
	}
	hlsMeta.Format.Name = "hls,applehttp"

	// 1. Progressive MP4 with missing http-progressive probe -> must NOT DIRECT_PLAY; falls back to REMUX
	capsMissingProg := domain.Capabilities{
		SuiteVersion: 2, CacheKey: "key-1",
		Probes: []domain.Probe{
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
		},
	}
	if got := LocalMode(progMeta, "video/mp4", capsMissingProg, ""); got != "REMUX" {
		t.Fatalf("expected REMUX when http-progressive probe is missing, got %s", got)
	}

	// 2. Progressive MP4 with stale http-progressive probe -> must NOT DIRECT_PLAY; falls back to REMUX
	capsStaleProg := domain.Capabilities{
		SuiteVersion: 2, CacheKey: "key-1",
		Probes: []domain.Probe{
			{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: now - 8*24*60*60},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
		},
	}
	if got := LocalMode(progMeta, "video/mp4", capsStaleProg, ""); got != "REMUX" {
		t.Fatalf("expected REMUX when http-progressive probe is stale, got %s", got)
	}

	// 3. Progressive MP4 with missing aac probe -> must NOT DIRECT_PLAY; falls back to REMUX
	capsMissingAAC := domain.Capabilities{
		SuiteVersion: 2, CacheKey: "key-1",
		Probes: []domain.Probe{
			{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: now},
		},
	}
	if got := LocalMode(progMeta, "video/mp4", capsMissingAAC, ""); got != "REMUX" {
		t.Fatalf("expected REMUX when aac probe is missing, got %s", got)
	}

	// 4. Progressive MP4 with stale aac probe -> must NOT DIRECT_PLAY; falls back to REMUX
	capsStaleAAC := domain.Capabilities{
		SuiteVersion: 2, CacheKey: "key-1",
		Probes: []domain.Probe{
			{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now - 8*24*60*60},
		},
	}
	if got := LocalMode(progMeta, "video/mp4", capsStaleAAC, ""); got != "REMUX" {
		t.Fatalf("expected REMUX when aac probe is stale, got %s", got)
	}

	// 5. Progressive MP4 with failed aac probe -> falls back to EXTERNAL_PLAYER
	capsFailedAAC := domain.Capabilities{
		SuiteVersion: 2, CacheKey: "key-1",
		Probes: []domain.Probe{
			{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "aac", Status: "FAIL", TestedAt: now},
		},
	}
	if got := LocalMode(progMeta, "video/mp4", capsFailedAAC, ""); got != "EXTERNAL_PLAYER" {
		t.Fatalf("expected EXTERNAL_PLAYER when aac probe fails, got %s", got)
	}

	// 6. Progressive MP4 with fresh matching transport, audio, and video PASS -> DIRECT_PLAY
	capsFullProg := domain.Capabilities{
		SuiteVersion: 2, CacheKey: "key-1",
		Probes: []domain.Probe{
			{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
		},
	}
	if got := LocalMode(progMeta, "video/mp4", capsFullProg, ""); got != "DIRECT_PLAY" {
		t.Fatalf("expected DIRECT_PLAY with full fresh evidence, got %s", got)
	}

	// 7. HLS MPEG-TS with missing aac probe -> must NOT DIRECT_PLAY; falls back to REMUX
	capsHLSMissingAAC := domain.Capabilities{
		SuiteVersion: 2, CacheKey: "key-1",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
		},
	}
	if got := LocalMode(hlsMeta, "application/vnd.apple.mpegurl", capsHLSMissingAAC, ""); got != "REMUX" {
		t.Fatalf("expected REMUX for HLS when aac probe is missing, got %s", got)
	}

	// 8. HLS MPEG-TS with fresh aac and HLS probe -> DIRECT_PLAY
	capsHLSFresh := domain.Capabilities{
		SuiteVersion: 2, CacheKey: "key-1",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
		},
	}
	if got := LocalMode(hlsMeta, "application/vnd.apple.mpegurl", capsHLSFresh, ""); got != "DIRECT_PLAY" {
		t.Fatalf("expected DIRECT_PLAY for HLS with fresh aac and hls probes, got %s", got)
	}
}

func TestDirectCodecDecisionsRequireFreshMatchingVideoEvidence(t *testing.T) {
	now := time.Now().Unix()

	make720p := func() domain.Metadata {
		m := domain.Metadata{
			Streams: []domain.Stream{
				{Type: "video", Codec: "h264", Profile: "Main", Width: 1280, Height: 720},
				{Type: "audio", Codec: "aac"},
			},
		}
		m.Format.Name = "mov,mp4,m4a,3gp,3g2,mj2"
		return m
	}

	make1080p := func() domain.Metadata {
		m := domain.Metadata{
			Streams: []domain.Stream{
				{Type: "video", Codec: "h264", Profile: "High", Width: 1920, Height: 1080},
				{Type: "audio", Codec: "aac"},
			},
		}
		m.Format.Name = "mov,mp4,m4a,3gp,3g2,mj2"
		return m
	}

	baseProbes := []domain.Probe{
		{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: now},
		{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
	}

	// 1. 720p with missing video probe timestamp (TestedAt == 0): cannot direct play, remuxes
	caps720NoTimestamp := domain.Capabilities{
		Probes: append([]domain.Probe{
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: 0},
		}, baseProbes...),
	}
	if got := LocalMode(make720p(), "video/mp4", caps720NoTimestamp, ""); got != "REMUX" {
		t.Fatalf("expected 720p with TestedAt==0 to fall back to REMUX, got %s", got)
	}

	// 2. 720p with no advancement (PositionMS == 0, not completed): cannot direct play, remuxes
	caps720NoProgress := domain.Capabilities{
		Probes: append([]domain.Probe{
			{ID: "h264-720-main", Status: "PASS", PositionMS: 0, Completed: false, TestedAt: now},
		}, baseProbes...),
	}
	if got := LocalMode(make720p(), "video/mp4", caps720NoProgress, ""); got != "REMUX" {
		t.Fatalf("expected 720p with no advancement to fall back to REMUX, got %s", got)
	}

	// 3. 720p with stale video probe (> 7 days): cannot direct play, remuxes
	caps720Stale := domain.Capabilities{
		Probes: append([]domain.Probe{
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now - 8*24*60*60},
		}, baseProbes...),
	}
	if got := LocalMode(make720p(), "video/mp4", caps720Stale, ""); got != "REMUX" {
		t.Fatalf("expected 720p with stale probe to fall back to REMUX, got %s", got)
	}

	// 4. 720p with future video probe (> 300s): cannot direct play, remuxes
	caps720Future := domain.Capabilities{
		Probes: append([]domain.Probe{
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now + 1000},
		}, baseProbes...),
	}
	if got := LocalMode(make720p(), "video/mp4", caps720Future, ""); got != "REMUX" {
		t.Fatalf("expected 720p with future probe to fall back to REMUX, got %s", got)
	}

	// 5. 720p with newer FAIL probe overriding older PASS: cannot direct play or remux, transcodes
	caps720NewerFail := domain.Capabilities{
		Probes: append([]domain.Probe{
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now - 3600},
			{ID: "h264-720-main", Status: "FAIL", TestedAt: now},
		}, baseProbes...),
	}
	if got := LocalMode(make720p(), "video/mp4", caps720NewerFail, ""); got != "TRANSCODE" {
		t.Fatalf("expected 720p with newer FAIL probe to fall back to TRANSCODE, got %s", got)
	}

	// 6. 1080p with missing video probe timestamp (TestedAt == 0): cannot direct play, transcodes
	caps1080NoTimestamp := domain.Capabilities{
		Probes: append([]domain.Probe{
			{ID: "h264-1080-high", Status: "PASS", PositionMS: 1500, TestedAt: 0},
		}, baseProbes...),
	}
	if got := LocalMode(make1080p(), "video/mp4", caps1080NoTimestamp, ""); got != "TRANSCODE" {
		t.Fatalf("expected 1080p with TestedAt==0 to fall back to TRANSCODE, got %s", got)
	}

	// 7. 1080p with no advancement: cannot direct play, transcodes
	caps1080NoProgress := domain.Capabilities{
		Probes: append([]domain.Probe{
			{ID: "h264-1080-high", Status: "PASS", PositionMS: 0, Completed: false, TestedAt: now},
		}, baseProbes...),
	}
	if got := LocalMode(make1080p(), "video/mp4", caps1080NoProgress, ""); got != "TRANSCODE" {
		t.Fatalf("expected 1080p with no advancement to fall back to TRANSCODE, got %s", got)
	}
}

func TestVerifiedTiersDirectPlayProgressiveAndHLS(t *testing.T) {
	now := time.Now().Unix()

	makeMeta := func(format, codec, profile string, width, height int) domain.Metadata {
		m := domain.Metadata{
			Streams: []domain.Stream{
				{
					Type: "video", Codec: codec, Profile: profile, Level: 51,
					Width: width, Height: height, PixelFormat: "yuv420p",
					FrameRate: "30/1", AverageFrameRate: "30/1",
					ColorTransfer: "bt709", ColorPrimaries: "bt709",
				},
				{Type: "audio", Codec: "aac"},
			},
		}
		m.Format.Name = format
		m.Format.BitRate = "4000000"
		return m
	}

	fullCaps := domain.Capabilities{
		SuiteVersion: 2,
		CacheKey:     "full-caps-key",
		Probes: []domain.Probe{
			{ID: "hls-h264-aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "http-progressive", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "aac", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-baseline-480", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-720-high", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-1080-high", Status: "PASS", PositionMS: 1500, TestedAt: now},
			{ID: "h264-2160-high", Status: "PASS", PositionMS: 1500, TestedAt: now},
		},
	}

	tiers := []struct {
		name   string
		format string
		mime   string
		width  int
		height int
		prof   string
	}{
		{"progressive baseline 360", "mov,mp4,m4a,3gp,3g2,mj2", "video/mp4", 640, 360, "Baseline"},
		{"progressive baseline 480", "mov,mp4,m4a,3gp,3g2,mj2", "video/mp4", 854, 480, "Baseline"},
		{"progressive 720 main", "mov,mp4,m4a,3gp,3g2,mj2", "video/mp4", 1280, 720, "Main"},
		{"progressive 720 high", "mov,mp4,m4a,3gp,3g2,mj2", "video/mp4", 1280, 720, "High"},
		{"progressive 1080 high", "mov,mp4,m4a,3gp,3g2,mj2", "video/mp4", 1920, 1080, "High"},
		{"progressive 4K high", "mov,mp4,m4a,3gp,3g2,mj2", "video/mp4", 3840, 2160, "High"},
		{"HLS baseline 360", "hls,applehttp", "application/vnd.apple.mpegurl", 640, 360, "Baseline"},
		{"HLS 720 main", "hls,applehttp", "application/vnd.apple.mpegurl", 1280, 720, "Main"},
		{"HLS 1080 high", "hls,applehttp", "application/vnd.apple.mpegurl", 1920, 1080, "High"},
		{"HLS 4K high", "hls,applehttp", "application/vnd.apple.mpegurl", 3840, 2160, "High"},
	}

	for _, tc := range tiers {
		t.Run(tc.name, func(t *testing.T) {
			meta := makeMeta(tc.format, "h264", tc.prof, tc.width, tc.height)
			got := LocalMode(meta, tc.mime, fullCaps, "")
			if got != "DIRECT_PLAY" {
				t.Fatalf("expected DIRECT_PLAY for verified %s, got %s", tc.name, got)
			}
		})
	}
}

func TestSafeFallbackHierarchyPreservesPipeline(t *testing.T) {
	meta360 := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
			{Type: "audio", Codec: "aac"},
		},
	}
	meta360.Format.Name = "mov,mp4,m4a,3gp,3g2,mj2"

	metaVP9 := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "vp9", Width: 1280, Height: 720},
			{Type: "audio", Codec: "aac"},
		},
	}
	metaVP9.Format.Name = "matroska,webm"

	metaAC3 := domain.Metadata{
		Streams: []domain.Stream{
			{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
			{Type: "audio", Codec: "ac3"},
		},
	}
	metaAC3.Format.Name = "mov,mp4,m4a,3gp,3g2,mj2"

	// 1. Unprobed device (zero probes): does not fail, safe fallback to REMUX for 360p
	if got := LocalMode(meta360, "video/mp4", domain.Capabilities{}, ""); got != "REMUX" {
		t.Fatalf("expected unprobed device to safely fall back to REMUX, got %s", got)
	}

	// 2. Incompatible container (MKV) with supported codec: falls back to REMUX
	metaMKV := meta360
	metaMKV.Format.Name = "matroska,webm"
	if got := LocalMode(metaMKV, "video/x-matroska", domain.Capabilities{}, ""); got != "REMUX" {
		t.Fatalf("expected MKV to fall back to REMUX, got %s", got)
	}

	// 3. Incompatible container with http-fmp4 failure: falls back to EXTERNAL_PLAYER
	capsFmp4Fail := domain.Capabilities{Probes: []domain.Probe{{ID: "http-fmp4", Status: "FAIL", TestedAt: time.Now().Unix()}}}
	if got := LocalMode(metaMKV, "video/x-matroska", capsFmp4Fail, ""); got != "EXTERNAL_PLAYER" {
		t.Fatalf("expected EXTERNAL_PLAYER when http-fmp4 fails, got %s", got)
	}

	// 4. Incompatible video codec (VP9): falls back to TRANSCODE
	if got := LocalMode(metaVP9, "video/webm", domain.Capabilities{}, ""); got != "TRANSCODE" {
		t.Fatalf("expected VP9 to fall back to TRANSCODE, got %s", got)
	}

	// 5. Incompatible video codec with baseline H.264 failure: falls back to EXTERNAL_PLAYER
	capsBaselineFail := domain.Capabilities{Probes: []domain.Probe{{ID: "h264-baseline-360", Status: "FAIL", TestedAt: time.Now().Unix()}}}
	if got := LocalMode(metaVP9, "video/webm", capsBaselineFail, ""); got != "EXTERNAL_PLAYER" {
		t.Fatalf("expected EXTERNAL_PLAYER when baseline H.264 fails, got %s", got)
	}

	// 6. Incompatible video codec with audio failure: falls back to EXTERNAL_PLAYER
	capsAACFail := domain.Capabilities{Probes: []domain.Probe{{ID: "aac", Status: "FAIL", TestedAt: time.Now().Unix()}}}
	if got := LocalMode(metaVP9, "video/webm", capsAACFail, ""); got != "EXTERNAL_PLAYER" {
		t.Fatalf("expected EXTERNAL_PLAYER when aac fails, got %s", got)
	}

	// 7. Audio-only conversion requires positive evidence for both H.264 video
	// copying and AAC output; an unprobed receiver keeps the full transcode path.
	if got := LocalMode(metaAC3, "video/mp4", domain.Capabilities{}, ""); got != "TRANSCODE" {
		t.Fatalf("expected unprobed AC3 to select TRANSCODE, got %s", got)
	}
}

func TestHybridPlannerRequiresFreshCopyAndAACEvidence(t *testing.T) {
	now := time.Now().Unix()
	metadata := domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
		{Type: "audio", Codec: "opus"},
	}}
	probes := func(video, audio domain.Probe) domain.Capabilities {
		return domain.Capabilities{Probes: []domain.Probe{video, audio}}
	}
	videoPass := domain.Probe{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: now}
	audioPass := domain.Probe{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now}
	for _, tc := range []struct {
		name string
		caps domain.Capabilities
		want string
	}{
		{"verified H264 and AAC", probes(videoPass, audioPass), "HYBRID"},
		{"missing AAC probe", domain.Capabilities{Probes: []domain.Probe{videoPass}}, "TRANSCODE"},
		{"stale AAC probe", probes(videoPass, domain.Probe{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now - 8*24*60*60}), "TRANSCODE"},
		{"missing video probe", domain.Capabilities{Probes: []domain.Probe{audioPass}}, "TRANSCODE"},
		{"stale video probe", probes(domain.Probe{ID: videoPass.ID, Status: "PASS", PositionMS: 1000, TestedAt: now - 8*24*60*60}, audioPass), "TRANSCODE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LocalMode(metadata, "video/mp4", tc.caps, ""); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
	if got := LocalMode(metadata, "video/mp4", domain.Capabilities{}, "HYBRID"); got != "TRANSCODE" {
		t.Fatalf("requested HYBRID bypassed missing probe evidence: %s", got)
	}
	unsupported := domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "vp9", Width: 640, Height: 360},
		{Type: "audio", Codec: "opus"},
	}}
	if got := LocalMode(unsupported, "video/webm", probes(videoPass, audioPass), ""); got != "TRANSCODE" {
		t.Fatalf("incompatible video selected HYBRID: %s", got)
	}
}

func TestHybridPlannerKeepsCopyableAACOnRemux(t *testing.T) {
	now := time.Now().Unix()
	metadata := domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
		{Type: "audio", Codec: "aac"},
	}}
	caps := domain.Capabilities{Probes: []domain.Probe{
		{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: now},
		{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
	}}
	if got := LocalMode(metadata, "video/mp4", caps, ""); got != "REMUX" {
		t.Fatalf("copyable AAC must remain REMUX, got %s", got)
	}
}

func TestLiveAACHLSUsesADTSWhenFragmentedMP4Fails(t *testing.T) {
	now := time.Now().Unix()
	metadata := domain.Metadata{Streams: []domain.Stream{{Type: "audio", Codec: "aac"}}}
	metadata.Format.Name = "hls"
	source := domain.Source{MIME: "application/vnd.apple.mpegurl", Live: true}

	// 1. Fresh advancing aac-adts PASS allows live audio-only HLS to REMUX via ADTS even when fMP4 fails
	caps := domain.Capabilities{Probes: []domain.Probe{
		{ID: "http-fmp4", Status: "FAIL", TestedAt: now},
		{ID: "aac-adts", Status: "PASS", PositionMS: 1000, TestedAt: now},
	}}
	if got := LocalModeSource(metadata, source, caps, "AUTO"); got != "REMUX" {
		t.Fatalf("live AAC should use ADTS despite failed fMP4 probe, got %s", got)
	}

	// 2. Container-agnostic M4A aac alone (without aac-adts) must NOT qualify as ADTS proof
	capsM4AOnly := domain.Capabilities{Probes: []domain.Probe{
		{ID: "http-fmp4", Status: "FAIL", TestedAt: now},
		{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
	}}
	if got := LocalModeSource(metadata, source, capsM4AOnly, "AUTO"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("M4A aac probe alone must NOT be accepted as ADTS proof, got %s", got)
	}

	// 3. Explicit aac-adts FAIL falls back to EXTERNAL_PLAYER
	caps.Probes[1] = domain.Probe{ID: "aac-adts", Status: "FAIL", TestedAt: now}
	if got := LocalModeSource(metadata, source, caps, "AUTO"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("failed ADTS decoder must fall back to EXTERNAL_PLAYER, got %s", got)
	}

	// 4. Missing/UNKNOWN aac-adts falls back to EXTERNAL_PLAYER
	capsMissing := domain.Capabilities{Probes: []domain.Probe{
		{ID: "http-fmp4", Status: "FAIL", TestedAt: now},
	}}
	if got := LocalModeSource(metadata, source, capsMissing, "AUTO"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("missing ADTS probe must fall back to EXTERNAL_PLAYER, got %s", got)
	}

	// 5. Stale aac-adts (> 7 days ago) falls back to EXTERNAL_PLAYER
	capsStale := domain.Capabilities{Probes: []domain.Probe{
		{ID: "http-fmp4", Status: "FAIL", TestedAt: now},
		{ID: "aac-adts", Status: "PASS", PositionMS: 1000, TestedAt: now - 8*24*3600},
	}}
	if got := LocalModeSource(metadata, source, capsStale, "AUTO"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("stale ADTS probe must fall back to EXTERNAL_PLAYER, got %s", got)
	}

	// 6. Mere prepare success (positionMS=0, completed=false) is UNKNOWN and must NOT qualify as PASS
	capsMerePrepare := domain.Capabilities{Probes: []domain.Probe{
		{ID: "http-fmp4", Status: "FAIL", TestedAt: now},
		{ID: "aac-adts", Status: "PASS", PositionMS: 0, Completed: false, TestedAt: now},
	}}
	if got := LocalModeSource(metadata, source, capsMerePrepare, "AUTO"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("unadvancing ADTS probe must not qualify as PASS, got %s", got)
	}

	// 7. Stalled playback must fall back to EXTERNAL_PLAYER
	capsStalled := domain.Capabilities{Probes: []domain.Probe{
		{ID: "http-fmp4", Status: "FAIL", TestedAt: now},
		{ID: "aac-adts", Status: "PASS", PositionMS: 1000, Stalled: true, TestedAt: now},
	}}
	if got := LocalModeSource(metadata, source, capsStalled, "AUTO"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("stalled ADTS playback must fall back to EXTERNAL_PLAYER, got %s", got)
	}

	// 8. Stream with video must NOT bypass fMP4 failure
	caps.Probes[1] = domain.Probe{ID: "aac-adts", Status: "PASS", PositionMS: 1000, TestedAt: now}
	metadataWithVideo := metadata
	metadataWithVideo.Streams = append([]domain.Stream{}, metadata.Streams...)
	metadataWithVideo.Streams = append(metadataWithVideo.Streams, domain.Stream{Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360})
	if got := LocalModeSource(metadataWithVideo, source, caps, "AUTO"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("video HLS must not bypass failed fMP4 probe, got %s", got)
	}
}

func TestLiveAACHLSPCMIsLastEvidenceBackedAudioRoute(t *testing.T) {
	now := time.Now().Unix()
	metadata := domain.Metadata{Streams: []domain.Stream{{Type: "audio", Codec: "aac"}}}
	metadata.Format.Name = "hls"
	source := domain.Source{MIME: "application/vnd.apple.mpegurl", Live: true}
	caps := domain.Capabilities{Probes: []domain.Probe{
		{ID: "http-fmp4", Status: "FAIL", TestedAt: now},
		{ID: "aac-adts", Status: "UNKNOWN", TestedAt: now},
		{ID: "audio-track-pcm-stream", Status: "PASS", PositionMS: 500, TestedAt: now},
	}}
	if got := LocalModeSource(metadata, source, caps, "AUTO"); got != "PCM_STREAM" {
		t.Fatalf("advancing local PCM probe should enable last audio route, got %s", got)
	}
	if got := LocalModeSource(metadata, source, caps, "REMUX"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("explicit REMUX must not silently transcode to PCM, got %s", got)
	}
	if got := LocalModeSource(metadata, source, caps, "TRANSCODE"); got != "PCM_STREAM" {
		t.Fatalf("explicit TRANSCODE may use probed PCM, got %s", got)
	}
	caps.Probes[1] = domain.Probe{ID: "aac-adts", Status: "PASS", PositionMS: 1000, TestedAt: now}
	if got := LocalModeSource(metadata, source, caps, "AUTO"); got != "REMUX" {
		t.Fatalf("copied ADTS should precede PCM when both work, got %s", got)
	}
	caps.Probes[2].PositionMS = 0
	caps.Probes[1].Status = "UNKNOWN"
	if got := LocalModeSource(metadata, source, caps, "AUTO"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("PCM preparation without advancing evidence cannot enable route, got %s", got)
	}
	caps.Probes[2].PositionMS = 500
	caps.Probes[2].TestedAt = now - 8*24*3600
	if got := LocalModeSource(metadata, source, caps, "AUTO"); got != "EXTERNAL_PLAYER" {
		t.Fatalf("stale PCM probe cannot enable route, got %s", got)
	}
	caps.Probes[2].TestedAt = now
	video := metadata
	video.Streams = append([]domain.Stream{}, metadata.Streams...)
	video.Streams = append(video.Streams, domain.Stream{Type: "video", Codec: "h264", Width: 640, Height: 360})
	if got := LocalModeSource(video, source, caps, "AUTO"); got == "PCM_STREAM" {
		t.Fatal("video HLS cannot be sent to audio-only PCM route")
	}
	source.Live = false
	if got := LocalModeSource(metadata, source, caps, "AUTO"); got == "PCM_STREAM" {
		t.Fatal("finite media must preserve its normal playback ladder")
	}
}

func TestLiveMP3UsesProbeBackedDirectPCMHierarchy(t *testing.T) {
	now := time.Now().Unix()
	metadata := domain.Metadata{Streams: []domain.Stream{{Type: "audio", Codec: "mp3"}}}
	metadata.Format.Name = "mp3"
	source := domain.Source{URL: "https://spotify.example/live", MIME: "audio/mpeg", Live: true}
	capabilities := func(mp3, fmp4, pcm domain.Probe) domain.Capabilities {
		return domain.Capabilities{Probes: []domain.Probe{mp3, fmp4, pcm}}
	}
	pass := func(id string) domain.Probe {
		return domain.Probe{ID: id, Status: "PASS", PositionMS: 1000, TestedAt: now}
	}
	fail := func(id string) domain.Probe {
		return domain.Probe{ID: id, Status: "FAIL", TestedAt: now}
	}
	unknown := func(id string) domain.Probe {
		return domain.Probe{ID: id, Status: "UNKNOWN", TestedAt: now}
	}
	for _, test := range []struct {
		name      string
		requested string
		caps      domain.Capabilities
		want      string
	}{
		{
			name:      "verified chunked MP3 stays direct even when lower tiers fail",
			requested: "AUTO",
			caps:      capabilities(pass("mp3-chunked"), pass("http-fmp4-chunked"), fail("audio-track-pcm-stream")),
			want:      "DIRECT_PLAY",
		},
		{
			name:      "H264 AAC fMP4 pass does not skip the MP3 compatibility fallback",
			requested: "AUTO",
			caps:      capabilities(fail("mp3-chunked"), pass("http-fmp4-chunked"), pass("audio-track-pcm-stream")),
			want:      "PCM_STREAM",
		},
		{
			name:      "PCM follows an unverified MP3 direct route",
			requested: "AUTO",
			caps:      capabilities(unknown("mp3-chunked"), fail("http-fmp4-chunked"), pass("audio-track-pcm-stream")),
			want:      "PCM_STREAM",
		},
		{
			name:      "fMP4 evidence alone does not claim an MP3 route",
			requested: "AUTO",
			caps:      capabilities(fail("mp3-chunked"), pass("http-fmp4-chunked"), unknown("audio-track-pcm-stream")),
			want:      "EXTERNAL_PLAYER",
		},
		{
			name:      "PCM is unavailable without advancing positive evidence",
			requested: "AUTO",
			caps: capabilities(fail("mp3-chunked"), fail("http-fmp4-chunked"), domain.Probe{
				ID: "audio-track-pcm-stream", Status: "PASS", TestedAt: now,
			}),
			want: "EXTERNAL_PLAYER",
		},
		{
			name:      "stale PCM evidence is unavailable",
			requested: "AUTO",
			caps: capabilities(fail("mp3-chunked"), fail("http-fmp4-chunked"), domain.Probe{
				ID: "audio-track-pcm-stream", Status: "PASS", PositionMS: 1000, TestedAt: now - 8*24*3600,
			}),
			want: "EXTERNAL_PLAYER",
		},
		{
			name:      "explicit remux remains respected",
			requested: "REMUX",
			caps:      capabilities(fail("mp3-chunked"), fail("http-fmp4-chunked"), pass("audio-track-pcm-stream")),
			want:      "REMUX",
		},
		{
			name:      "explicit transcode may use the probed PCM route",
			requested: "TRANSCODE",
			caps:      capabilities(pass("mp3-chunked"), pass("http-fmp4-chunked"), pass("audio-track-pcm-stream")),
			want:      "PCM_STREAM",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := LocalModeSource(metadata, source, test.caps, test.requested); got != test.want {
				t.Fatalf("LocalModeSource() = %s, want %s", got, test.want)
			}
		})
	}

	video := metadata
	video.Streams = append(append([]domain.Stream(nil), metadata.Streams...), domain.Stream{Type: "video", Codec: "h264", Width: 640, Height: 360})
	if got := LocalModeSource(video, source, capabilities(fail("mp3-chunked"), fail("http-fmp4-chunked"), pass("audio-track-pcm-stream")), "AUTO"); got == "PCM_STREAM" {
		t.Fatal("video-bearing input selected the audio-only PCM route")
	}
	finite := source
	finite.Live = false
	if got := LocalModeSource(metadata, finite, capabilities(fail("mp3-chunked"), fail("http-fmp4-chunked"), pass("audio-track-pcm-stream")), "AUTO"); got == "PCM_STREAM" {
		t.Fatal("finite MP3 selected the live PCM route")
	}
}
