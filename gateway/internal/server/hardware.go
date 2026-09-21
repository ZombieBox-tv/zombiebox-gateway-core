package server

import (
	"net/http"

	"zombiebox.local/gateway/internal/devices"

	"zombiebox.local/gateway/internal/domain"
)

func (s *Server) hardwareReport(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var report domain.HardwareReport
	if !decode(w, r, &report) {
		return
	}
	if !devices.ValidHardware(report) {
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
