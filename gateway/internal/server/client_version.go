package server

import (
	"net/http"
	"regexp"

	"zombiebox.local/gateway/internal/domain"
)

var clientVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,39}$`)

// clientVersion refreshes the authenticated installation's app identity without
// requiring the operator pairing code again. A changed APK invalidates measured
// capabilities, but does not rotate the device token or alter consent settings.
func (s *Server) clientVersion(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var request struct {
		ClientVersion string `json:"clientVersion"`
	}
	if !decode(w, r, &request) {
		return
	}
	if !clientVersionPattern.MatchString(request.ClientVersion) {
		fail(w, 400, "invalid_client_version")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.Get(r.Context(), "devices", d.ID, &d) != nil {
		fail(w, 500, "storage_error")
		return
	}
	if d.Registration.ClientVersion != request.ClientVersion {
		d.Registration.ClientVersion = request.ClientVersion
		d.Capabilities = domain.Capabilities{Version: 1, DeviceID: d.ID, Probes: []domain.Probe{}}
		if s.db.Put(r.Context(), "devices", d.ID, d) != nil {
			fail(w, 500, "storage_error")
			return
		}
		s.events.publish(d.ID, "device.client_changed", map[string]string{"clientVersion": request.ClientVersion})
	}
	respond(w, 200, map[string]string{"clientVersion": d.Registration.ClientVersion})
}
