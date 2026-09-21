package devices

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"zombiebox.local/gateway/internal/domain"
)

const ProbeSuiteVersion = 2

// ProbeCacheKey includes only stable discovery data, never free memory, network
// state or credentials. Changing the application or suite invalidates evidence.
func ProbeCacheKey(d domain.Device) string {
	fingerprint := ""
	if d.Registration.Hardware != nil {
		fingerprint = d.Registration.Hardware.Fingerprint
	}
	data, _ := json.Marshal(struct {
		Platform    domain.Platform
		Application string
		Hardware    string
		Suite       int
	}{d.Registration.Platform, d.Registration.ClientVersion, fingerprint, ProbeSuiteVersion})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func CurrentCapabilities(d domain.Device) domain.Capabilities {
	c := d.Capabilities
	if c.SuiteVersion != 0 && (c.SuiteVersion != ProbeSuiteVersion || c.CacheKey != ProbeCacheKey(d)) {
		return domain.Capabilities{Version: 1, DeviceID: d.ID, Probes: []domain.Probe{}}
	}
	return c
}
