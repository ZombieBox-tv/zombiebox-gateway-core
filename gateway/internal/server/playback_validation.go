package server

import (
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
)

const playbackValidationFreshness = 7 * 24 * time.Hour

type playbackValidation struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func unavailablePlaybackValidation(reason string) playbackValidation {
	return playbackValidation{Status: "UNAVAILABLE", Reason: reason}
}

func refreshPlaybackValidation(reason string) playbackValidation {
	return playbackValidation{Status: "REFRESH_REQUIRED", Reason: reason}
}

// playbackValidationFor describes whether recorded playback measurements are
// current. Probe outcomes do not affect freshness: a recent FAIL or UNKNOWN is
// still a current measurement of the device.
func playbackValidationFor(
	device domain.Device,
	now time.Time,
	available map[string]bool,
) playbackValidation {
	if device.ID == "" {
		return unavailablePlaybackValidation("device_context_unavailable")
	}
	caps := device.Capabilities
	if caps.SuiteVersion == 0 {
		return refreshPlaybackValidation("suite_missing")
	}
	if caps.SuiteVersion != devices.ProbeSuiteVersion {
		return refreshPlaybackValidation("suite_mismatch")
	}
	if caps.CacheKey == "" {
		return refreshPlaybackValidation("cache_missing")
	}
	if caps.CacheKey != devices.ProbeCacheKey(device) {
		return refreshPlaybackValidation("cache_mismatch")
	}
	if caps.DeviceID == "" {
		return refreshPlaybackValidation("device_binding_missing")
	}
	if caps.DeviceID != device.ID {
		return refreshPlaybackValidation("device_mismatch")
	}

	checked := make(map[string]bool)
	hasRelevantEvidence := false
	hasExpiredEvidence := false
	hasInvalidTimestamp := false
	for _, probe := range caps.Probes {
		if !available[probe.ID] || checked[probe.ID] {
			continue
		}
		checked[probe.ID] = true
		hasRelevantEvidence = true
		if freshProbe(caps, probe.ID, now) != nil {
			continue
		}

		latest := latestPlaybackValidationProbe(caps, probe.ID, now)
		if latest == nil || latest.TestedAt <= 0 || latest.TestedAt > now.Unix()+300 {
			hasInvalidTimestamp = true
			continue
		}
		if latest.TestedAt <= now.Add(-playbackValidationFreshness).Unix() {
			hasExpiredEvidence = true
		}
	}
	if !hasRelevantEvidence {
		return refreshPlaybackValidation("evidence_missing")
	}
	if hasExpiredEvidence {
		return refreshPlaybackValidation("evidence_expired")
	}
	if hasInvalidTimestamp {
		return refreshPlaybackValidation("evidence_invalid")
	}
	return playbackValidation{Status: "CURRENT"}
}

// availablePlaybackProbeIDs mirrors the suite-2 extended manifest's asset
// filtering so unsupported or locally unavailable optional probes do not
// create a permanent refresh prompt.
func (s *Server) availablePlaybackProbeIDs(device domain.Device) map[string]bool {
	available := make(map[string]bool, len(probeAssets)+2)
	for _, asset := range probeAssets {
		if asset.Requires != "" && !probeCandidate(device, asset.ID) {
			continue
		}
		if asset.ID == hlsEventProbeID {
			if s.hlsEventAssetsAvailable() {
				available[asset.ID] = true
			}
			continue
		}
		file, err := s.probeFile(asset)
		if err != nil {
			continue
		}
		_ = file.Close()
		available[asset.ID] = true
	}
	if available["h264-baseline-360"] {
		available["texture-output"] = true
	}
	// The Client adds this local playback check to the same suite manifest.
	available["audio-track-pcm-stream"] = true
	return available
}

// Keep the same duplicate selection as freshProbe: a newer credible result
// supersedes stale records, while a far-future duplicate cannot hide a valid
// timestamp.
func latestPlaybackValidationProbe(
	caps domain.Capabilities,
	probeID string,
	now time.Time,
) *domain.Probe {
	nowSeconds := now.Unix()
	var latest *domain.Probe
	for index := range caps.Probes {
		probe := &caps.Probes[index]
		if probe.ID != probeID {
			continue
		}
		if probe.TestedAt > nowSeconds+300 {
			if latest == nil {
				latest = probe
			}
			continue
		}
		if latest == nil || latest.TestedAt > nowSeconds+300 || probe.TestedAt >= latest.TestedAt {
			latest = probe
		}
	}
	return latest
}
