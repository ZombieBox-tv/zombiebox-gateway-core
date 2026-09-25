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

var screenTiers = []CastVideo{
	{"h264", 3840, 2160, 30, 12000000},
	{"h264", 1920, 1080, 30, 4000000},
	{"h264", 1280, 720, 30, 2000000},
	{"h264", 854, 480, 24, 1200000},
	{"h264", 640, 360, 24, 800000},
}

// CapCastProfileForNetwork conservatively caps a selected SCREEN CastVideo profile
// using measured gateway->TV goodput (kbps) with about 30% transport headroom.
//
// NOTE: This sample only measures the gateway->TV HLS leg, not the phone->gateway
// RTSP transmission. This is a conservative initial grant cap based on paired-client
// evidence, not full runtime congestion control.
//
// If goodput is unknown or unmeasured (kbps <= 0), the profile is returned unchanged.
// If goodput is insufficient to sustain the explicit viable 360p floor (800 kbps),
// it rejects the session rather than fabricating success.
func CapCastProfileForNetwork(profile CastVideo, goodputKbps int64) (CastVideo, error) {
	if goodputKbps <= 0 {
		return profile, nil
	}
	// 30% transport headroom: budget = goodput_bps * 0.70 = kbps * 1000 * 0.70 = kbps * 700.
	budget := goodputKbps * 700
	if budget < 800000 {
		return CastVideo{}, errors.New("insufficient network goodput for cast")
	}
	if int64(profile.Bitrate) <= budget {
		return profile, nil
	}
	codec := profile.Codec
	if codec == "" {
		codec = "h264"
	}
	for _, tier := range screenTiers {
		if int64(tier.Bitrate) <= budget &&
			tier.Bitrate <= profile.Bitrate &&
			tier.MaxHeight <= profile.MaxHeight &&
			tier.MaxWidth <= profile.MaxWidth {
			return CastVideo{
				Codec:     codec,
				MaxWidth:  tier.MaxWidth,
				MaxHeight: tier.MaxHeight,
				FPS:       min(profile.FPS, tier.FPS),
				Bitrate:   tier.Bitrate,
			}, nil
		}
	}
	return CastVideo{
		Codec:     codec,
		MaxWidth:  min(profile.MaxWidth, 640),
		MaxHeight: min(profile.MaxHeight, 360),
		FPS:       min(profile.FPS, 24),
		Bitrate:   min(profile.Bitrate, 800000),
	}, nil
}

// Audio sessions do not need an H.264 decoder. Unknown support is still only a
// candidate; explicit failed AAC/transport probes must not be ignored.
// SCREEN sessions conservatively cap the granted profile if a valid goodput sample
// is provided. AUDIO sessions leave goodput unconstrained.
func CastProfileForMode(device domain.Device, mode string, maxHeight int, now time.Time, goodputKbps ...int64) (CastVideo, error) {
	if mode == "SCREEN" {
		video, err := CastProfileForSender(device, maxHeight, now)
		if err != nil {
			return video, err
		}
		if len(goodputKbps) > 0 {
			return CapCastProfileForNetwork(video, goodputKbps[0])
		}
		return video, nil
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
