package playback

import (
	"testing"

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
