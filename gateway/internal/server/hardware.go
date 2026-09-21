package server

import (
	"net/http"
	"regexp"

	"zombiebox.local/gateway/internal/domain"
)

var fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validHardware(report domain.HardwareReport) bool {
	if report.Version != 1 || !fingerprintPattern.MatchString(report.Fingerprint) || len(report.Product) > 200 || len(report.Device) > 200 || report.CPUCores < 0 || report.CPUCores > 1024 || report.GLES < 0 || report.PhysicalMB < 0 || report.StorageFreeMB < 0 || report.GatewayLatencyMS < 0 || report.GatewayLatencyMS > 60000 || len(report.Network) > 40 || len(report.ABIs) > 8 || len(report.Decoders) > 128 || len(report.ExternalPlayers) > 32 {
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
func (s *Server) hardwareReport(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var report domain.HardwareReport
	if !decode(w, r, &report) {
		return
	}
	if !validHardware(report) {
		fail(w, 400, "invalid_hardware_report")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.Get(r.Context(), "devices", d.ID, &d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	previous := d.Registration.Hardware
	if previous == nil || previous.Fingerprint != report.Fingerprint {
		d.Capabilities = domain.Capabilities{Version: 1, DeviceID: d.ID, Probes: []domain.Probe{}}
	}
	d.Registration.Hardware = &report
	if s.db.Put(r.Context(), "devices", d.ID, d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.events.publish(d.ID, "hardware.changed", map[string]string{"fingerprint": report.Fingerprint})
	respond(w, 200, report)
}
