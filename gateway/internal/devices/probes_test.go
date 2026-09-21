package devices

import (
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestProbeEvidenceIdentity(t *testing.T) {
	d := domain.Device{ID: "fixture", Registration: domain.Registration{ClientVersion: "one", Platform: domain.Platform{AndroidAPI: 13}}}
	key := ProbeCacheKey(d)
	d.Capabilities = domain.Capabilities{Version: 1, DeviceID: d.ID, SuiteVersion: ProbeSuiteVersion, CacheKey: key, Probes: []domain.Probe{{ID: "aac", Status: "PASS"}}}
	if len(CurrentCapabilities(d).Probes) != 1 {
		t.Fatal("current evidence discarded")
	}
	for _, mutate := range []func(*domain.Device){
		func(d *domain.Device) { d.Registration.ClientVersion = "two" },
		func(d *domain.Device) { d.Registration.Platform.Release = "new firmware" },
		func(d *domain.Device) { d.Capabilities.SuiteVersion++ },
		func(d *domain.Device) { d.Registration.Hardware = &domain.HardwareReport{Fingerprint: "changed"} },
	} {
		changed := d
		mutate(&changed)
		if len(CurrentCapabilities(changed).Probes) != 0 {
			t.Fatal("stale evidence reused")
		}
	}
}
