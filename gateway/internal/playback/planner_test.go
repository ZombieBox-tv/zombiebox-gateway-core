package playback

import (
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
)

func TestLocalPlannerUsesEvidenceNotAndroidVersion(t *testing.T) {
	native := media.Metadata{Streams: []media.Stream{{Type: "video", Codec: "h264", Width: 640, Height: 360}, {Type: "audio", Codec: "aac"}}}
	unsupported := media.Metadata{Streams: []media.Stream{{Type: "video", Codec: "vp9"}}}
	for _, tc := range []struct {
		name, mime, override, want string
		metadata                   media.Metadata
		probes                     []domain.Probe
	}{
		{"native", "video/mp4", "", "DIRECT_PLAY", native, nil},
		{"container", "video/x-matroska", "", "REMUX", native, nil},
		{"codec", "video/webm", "", "TRANSCODE", unsupported, nil},
		{"failed fragment probe", "video/x-matroska", "", "EXTERNAL_PLAYER", native, []domain.Probe{{ID: "http-fmp4", Status: "FAIL"}}},
		{"failed audio probe", "video/webm", "", "EXTERNAL_PLAYER", unsupported, []domain.Probe{{ID: "aac", Status: "FAIL"}}},
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
		caps := domain.Capabilities{Probes: []domain.Probe{{ID: "http-fmp4", Status: "FAIL"}}}
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
