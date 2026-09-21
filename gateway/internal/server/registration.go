// Package server exposes the versioned client protocol. Provider DTOs stay here.
package server

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"time"

	"zombiebox.local/gateway/internal/devices"

	"zombiebox.local/gateway/internal/domain"
)

var installationID = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`)

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req domain.Registration
	if !decode(w, r, &req) {
		return
	}
	if req.ProtocolVersion != 1 {
		fail(w, 426, "unsupported_protocol")
		return
	}
	if (req.Hardware != nil && !devices.ValidHardware(*req.Hardware)) || req.Platform.AndroidAPI < 9 || req.Display.Width < 0 || req.Display.Height < 0 || req.Memory.ClassMB < 0 || len(req.Platform.ABIs) > 8 || req.ClientVersion == "" || len(req.ClientVersion) > 40 || !installationID.MatchString(req.InstallationID) {
		fail(w, 400, "invalid_registration")
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, a := range s.attempts {
		if now.After(a.until) {
			delete(s.attempts, key)
		}
	}
	a := s.attempts[ip]
	if a.count >= 5 || len(s.attempts) >= 256 {
		fail(w, 429, "pairing_rate_limited")
		return
	}
	if s.opt.PairingCode == "" || subtle.ConstantTimeCompare([]byte(req.PairingCode), []byte(s.opt.PairingCode)) != 1 {
		a.count++
		a.until = now.Add(time.Minute)
		s.attempts[ip] = a
		fail(w, 403, "pairing_required")
		return
	}
	delete(s.attempts, ip)
	var existing domain.Device
	err := s.db.Get(r.Context(), "devices", req.InstallationID, &existing)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		fail(w, 500, "storage_error")
		return
	}
	if errors.Is(err, domain.ErrNotFound) {
		count, e := s.db.Count(r.Context(), "devices")
		if e != nil {
			fail(w, 500, "storage_error")
			return
		}
		if count >= 64 {
			fail(w, 409, "device_limit")
			return
		}
		existing = domain.Device{ID: req.InstallationID, Preferences: domain.DefaultPreferences(), Capabilities: domain.Capabilities{Version: 1, DeviceID: req.InstallationID, Probes: []domain.Probe{}}}
	}
	if req.Platform.ABIs == nil {
		req.Platform.ABIs = []string{}
	}
	req.PairingCode = ""
	if !reflect.DeepEqual(existing.Registration.Platform, req.Platform) || existing.Registration.Memory != req.Memory {
		existing.Capabilities = domain.Capabilities{Version: 1, DeviceID: existing.ID, Probes: []domain.Probe{}}
	} else if req.Hardware == nil {
		req.Hardware = existing.Registration.Hardware
	}
	if req.Hardware != nil && (existing.Registration.Hardware == nil || req.Hardware.Fingerprint != existing.Registration.Hardware.Fingerprint) {
		existing.Capabilities = domain.Capabilities{Version: 1, DeviceID: existing.ID, Probes: []domain.Probe{}}
	}
	existing.Registration = req
	token := randomID(32)
	if s.db.PutMany(r.Context(), domain.Record{Bucket: "devices", ID: existing.ID, Value: existing}, domain.Record{Bucket: "tokens", ID: existing.ID, Value: tokenHash(token)}) != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.events.publish(existing.ID, "device.registered", map[string]string{"deviceId": existing.ID})
	mode := "HANDHELD"
	if req.Display.Dpad || !req.Display.Touch {
		mode = "TV"
	}
	respond(w, 201, map[string]any{"apiVersion": 1, "uiSchemaVersion": 1, "playbackVersion": 1, "capabilitiesVersion": 1, "deviceId": existing.ID, "deviceToken": token, "pairingRequired": false, "presentation": map[string]string{"suggestedMode": mode}, "preferences": existing.Preferences})
}
