package playback

import (
	"zombiebox.local/gateway/internal/domain"
)

// selectLatestProbe resolves duplicate probe entries for the same ID by
// choosing the latest credible result (not in the future by > 300s).
// If multiple entries have the same highest timestamp, the latest occurring in
// the slice is chosen. If only future timestamps exist, the last occurrence is returned.
func selectLatestProbe(probes []domain.Probe, probeID string, now int64) *domain.Probe {
	var latest *domain.Probe
	for i := range probes {
		p := &probes[i]
		if p.ID != probeID {
			continue
		}
		// A probe with timestamp far in the future is not credible.
		if p.TestedAt > now+300 {
			if latest == nil {
				latest = p
			}
			continue
		}
		if latest == nil || latest.TestedAt > now+300 || p.TestedAt >= latest.TestedAt {
			latest = p
		}
	}
	return latest
}

// probeStatus evaluates the latest credible probe result for a given probe ID.
// It returns:
//   - "PASS" if the probe is fresh (tested within 7 days, <= 300s in the future), non-stalled,
//     and shows verified playback progress (position >= 500ms or completed).
//   - "FAIL" if the probe explicitly failed or experienced a playback stall.
//   - "UNKNOWN" if no probe exists, the timestamp is missing (0), stale (> 7 days),
//     in the future (> 300s), or playback made no progress.
func probeStatus(caps domain.Capabilities, probeID string, now int64) string {
	p := selectLatestProbe(caps.Probes, probeID, now)
	if p == nil {
		return "UNKNOWN"
	}
	if p.TestedAt <= 0 || p.TestedAt <= now-7*24*60*60 || p.TestedAt > now+300 {
		return "UNKNOWN"
	}
	// Check for fresh, non-stalled, advancing/completed PASS:
	if p.Status == "PASS" && !p.Stalled && (p.PositionMS >= 500 || p.Completed) {
		return "PASS"
	}
	// Explicit failure or stalled playback is a FAIL:
	if p.Status == "FAIL" || p.Stalled {
		return "FAIL"
	}
	return "UNKNOWN"
}
