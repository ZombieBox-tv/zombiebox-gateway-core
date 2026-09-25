package playback

import (
	"errors"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type CastVideo struct {
	Codec     string `json:"codec"`
	MaxWidth  int    `json:"maxWidth"`
	MaxHeight int    `json:"maxHeight"`
	FPS       int    `json:"fps"`
	Bitrate   int    `json:"bitrate"`
}

// Larger grants require explicit sender opt-in and fresh, advancing receiver
// evidence. Existing senders retain the original wire/encoder limits.
func CastProfileForSender(device domain.Device, maxHeight int, now time.Time) (CastVideo, error) {
	memory := device.Registration.Memory.PhysicalMB
	if (maxHeight == 1080 || maxHeight == 2160) && (memory == 0 || memory > 768) && device.Capabilities.SuiteVersion == 2 && device.Capabilities.CacheKey != "" {
		transportReady := probeStatus(device.Capabilities, "hls-h264-aac", now.Unix()) == "PASS"
		if maxHeight == 2160 && probeStatus(device.Capabilities, "h264-2160-high", now.Unix()) == "PASS" && transportReady && maxSupportedOutputHeight(device) >= 2160 {
			return CastVideo{"h264", 3840, 2160, 30, 12000000}, nil
		}
		if probeStatus(device.Capabilities, "h264-1080-high", now.Unix()) == "PASS" && transportReady {
			return CastVideo{"h264", 1920, 1080, 30, 4000000}, nil
		}
	}
	return CastProfile(device)
}

// Unknown receivers get a conservative candidate; measured evidence may raise it.
// These are encoder limits, never a claim of runtime or OEM compatibility.
func CastProfile(device domain.Device) (CastVideo, error) {
	states := map[string]string{}
	for _, probe := range device.Capabilities.Probes {
		states[probe.ID] = probe.Status
	}
	base := CastVideo{"h264", 640, 360, 24, 800000}
	if states["hls-h264-aac"] == "FAIL" {
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

// Audio sessions do not need an H.264 decoder. Unknown support is still only a
// candidate; explicit failed AAC/transport probes must not be ignored.
func CastProfileForMode(device domain.Device, mode string, maxHeight int, now time.Time) (CastVideo, error) {
	if mode == "SCREEN" {
		return CastProfileForSender(device, maxHeight, now)
	}
	if mode != "AUDIO" {
		return CastVideo{}, errors.New("invalid cast mode")
	}
	for _, probe := range device.Capabilities.Probes {
		if (probe.ID == "aac" || probe.ID == "hls-h264-aac") && probe.Status == "FAIL" {
			return CastVideo{}, errors.New("receiver audio transport unsupported")
		}
	}
	return CastVideo{}, nil
}
