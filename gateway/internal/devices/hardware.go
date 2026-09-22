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
	if len(report.IntegrationHints) > 32 || len(report.ABIs) > 8 || len(report.Decoders) > 128 || len(report.Encoders) > 32 || len(report.Displays) > 8 || len(report.ExternalPlayers) > 32 {
		return false
	}

	// Discovery hints never certify native DIAL or multicast. Functional evidence
	// belongs in capabilities/probes, not in this software inventory.
	if report.NativeDIAL != "UNKNOWN" || report.Multicast != "UNKNOWN" {
		return false
	}
	for _, name := range append(append(append([]string{}, report.ABIs...), report.ExternalPlayers...), report.IntegrationHints...) {
		if len(name) > 200 {
			return false
		}
	}
	for _, codec := range report.Decoders {
		if !validCodec(codec) {
			return false
		}
	}
	for _, codec := range report.Encoders {
		if len(codec.ProbeCandidates) != 0 || !validCodec(codec) {
			return false
		}
	}
	return validDisplays(report.Displays)
}

func validCodec(codec domain.CodecHint) bool {
	if len(codec.Profiles) > 8 || (codec.Acceleration != "" && codec.Acceleration != "UNKNOWN" && codec.Acceleration != "HARDWARE" && codec.Acceleration != "SOFTWARE") {
		return false
	}
	profiles := map[domain.CodecProfileHint]bool{}
	for _, profile := range codec.Profiles {
		found := false
		for _, mime := range codec.Types {
			if profile.MIME == mime {
				found = true
			}
		}
		if !found || profile.Profile < 0 || profile.Level < 0 || profiles[profile] {
			return false
		}
		profiles[profile] = true
	}
	if len(codec.Name) > 200 || len(codec.Types) > 16 || len(codec.ProbeCandidates) > 3 {
		return false
	}
	seen := map[string]bool{}
	for _, candidate := range codec.ProbeCandidates {
		if seen[candidate] {
			return false
		}
		seen[candidate] = true
		if candidate != "h264-2160-high" && candidate != "hevc-1080-main" && candidate != "hevc-2160-main" {
			return false
		}
	}
	for _, mime := range codec.Types {
		if len(mime) > 100 {
			return false
		}
	}
	return true
}

func validDisplays(displays []domain.DisplayHint) bool {
	seenDisplays := map[int]bool{}
	defaults := 0
	for _, display := range displays {
		if display.ID < 0 || seenDisplays[display.ID] || !validDisplaySize(display.Width, display.Height, display.RefreshMilliHz) || display.ActiveModeID < 0 || len(display.Modes) > 16 {
			return false
		}
		seenDisplays[display.ID] = true
		if display.Default {
			defaults++
		}
		if defaults > 1 {
			return false
		}
		seenModes := map[int]bool{}
		active := display.ActiveModeID == 0
		for _, mode := range display.Modes {
			if mode.ID <= 0 || seenModes[mode.ID] || !validDisplaySize(mode.Width, mode.Height, mode.RefreshMilliHz) {
				return false
			}
			seenModes[mode.ID] = true
			if mode.ID == display.ActiveModeID {
				active = mode.Width == display.Width && mode.Height == display.Height && mode.RefreshMilliHz == display.RefreshMilliHz
			}
		}
		if !active {
			return false
		}
	}
	return true
}

func validDisplaySize(width, height, rate int) bool {
	return width > 0 && width <= 16384 && height > 0 && height <= 16384 && rate >= 0 && rate <= 480000
}
