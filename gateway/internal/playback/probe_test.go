package playback

import (
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestProbeStatusRequiresFreshAdvancingEvidence(t *testing.T) {
	now := time.Now().Unix()
	for _, tc := range []struct {
		name  string
		probe domain.Probe
		want  string
	}{
		{"advancing pass", domain.Probe{ID: "h264-1080-high", Status: "PASS", TestedAt: now, PositionMS: 1000}, "PASS"},
		{"no progress", domain.Probe{ID: "h264-1080-high", Status: "PASS", TestedAt: now}, "UNKNOWN"},
		{"fresh fail", domain.Probe{ID: "h264-1080-high", Status: "FAIL", TestedAt: now}, "FAIL"},
		{"stale fail", domain.Probe{ID: "h264-1080-high", Status: "FAIL", TestedAt: now - 8*24*3600}, "UNKNOWN"},
		{"undated fail", domain.Probe{ID: "h264-1080-high", Status: "FAIL"}, "UNKNOWN"},
		{"future fail", domain.Probe{ID: "h264-1080-high", Status: "FAIL", TestedAt: now + 301}, "UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeStatus(domain.Capabilities{Probes: []domain.Probe{tc.probe}}, tc.probe.ID, now)
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
