package devices

import (
	"regexp"

	"zombiebox.local/gateway/internal/domain"
)

var fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidHardware(report domain.HardwareReport) bool {
	if report.Version != 1 || !fingerprintPattern.MatchString(report.Fingerprint) {
		return false
	}
	if len(report.Product) > 200 || len(report.Device) > 200 {
		return false
	}
	if report.CPUCores < 0 || report.CPUCores > 1024 || report.GLES < 0 || report.PhysicalMB < 0 || report.StorageFreeMB < 0 {
		return false
	}
	if report.GatewayLatencyMS < 0 || report.GatewayLatencyMS > 60000 || len(report.Network) > 40 {
		return false
	}
	if len(report.ABIs) > 8 || len(report.Decoders) > 128 || len(report.ExternalPlayers) > 32 {
		return false
	}

	// Discovery hints never certify native DIAL or multicast. Functional evidence
	// belongs in capabilities/probes, not in this software inventory.
	if report.NativeDIAL != "UNKNOWN" || report.Multicast != "UNKNOWN" {
		return false
	}
	for _, name := range append(append([]string{}, report.ABIs...), report.ExternalPlayers...) {
		if len(name) > 200 {
			return false
		}
	}
	for _, codec := range report.Decoders {
		if len(codec.Name) > 200 || len(codec.Types) > 16 {
			return false
		}
		for _, mime := range codec.Types {
			if len(mime) > 100 {
				return false
			}
		}
	}
	return true
}
