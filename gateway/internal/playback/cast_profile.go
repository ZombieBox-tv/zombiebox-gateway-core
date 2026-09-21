package playback

import (
	"errors"

	"zombiebox.local/gateway/internal/domain"
)

type CastVideo struct {
	Codec     string `json:"codec"`
	MaxWidth  int    `json:"maxWidth"`
	MaxHeight int    `json:"maxHeight"`
	FPS       int    `json:"fps"`
	Bitrate   int    `json:"bitrate"`
}

// Unknown receivers get a conservative candidate; measured evidence may raise it.
// These are encoder limits, never a claim of runtime or OEM compatibility.
func CastProfile(device domain.Device) (CastVideo, error) {
	states := map[string]string{}
	for _, probe := range device.Capabilities.Probes {
		states[probe.ID] = probe.Status
	}
	base := CastVideo{"h264", 640, 360, 24, 800000}
	if states["hls"] == "FAIL" {
		return base, errors.New("receiver transport unsupported")
	}
	memory := device.Registration.Memory.PhysicalMB
	lowMemory := memory > 0 && memory <= 768
	if !lowMemory {
		if states["h264-720-main"] == "PASS" || states["h264-720-high"] == "PASS" {
			return CastVideo{"h264", 1280, 720, 30, 2000000}, nil
		}
		if states["h264-baseline-480"] == "PASS" {
			return CastVideo{"h264", 854, 480, 24, 1200000}, nil
		}
	}
	if states["h264-baseline-360"] == "FAIL" {
		return base, errors.New("receiver video unsupported")
	}
	return base, nil
}
