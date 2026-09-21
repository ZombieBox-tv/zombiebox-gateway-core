package server

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sort"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

// Export is an allowlist, not a dump of SQLite, provider configs or session DTOs.
// It makes no network/process probes and cannot promote discovery into evidence.
func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request, device domain.Device) {
	modules := []map[string]any{}
	for _, module := range s.moduleList(r.Context()) {
		modules = append(modules, map[string]any{"id": module.ID, "state": module.State, "features": module.Features})
	}
	active := []map[string]any{}
	s.mu.Lock()
	for _, session := range s.sessions {
		if session.device == device.ID && time.Now().Before(session.expires) {
			active = append(active, map[string]any{"mode": session.mode, "live": session.source.Live, "provider": session.source.Item.Provider})
		}
	}
	s.mu.Unlock()
	failures := []map[string]any{}
	failed := []domain.Progress{}
	records, err := s.db.List(r.Context(), "progress:"+device.ID)
	if err != nil {
		fail(w, 500, "diagnostics_unavailable")
		return
	}
	for _, raw := range records {
		var progress domain.Progress
		if json.Unmarshal(raw, &progress) == nil && progress.State == "FAILED" {
			failed = append(failed, progress)
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].UpdatedAt > failed[j].UpdatedAt })
	for _, progress := range failed {
		failures = append(failures, map[string]any{"provider": progress.Item.Provider, "state": progress.State, "at": progress.UpdatedAt})
		if len(failures) == 20 {
			break
		}
	}
	probes := device.Capabilities.Probes
	if probes == nil {
		probes = []domain.Probe{}
	}
	report := map[string]any{
		"reportVersion": 1, "createdAt": time.Now().UTC().Format(time.RFC3339),
		"gateway": map[string]any{"apiVersion": 1, "os": runtime.GOOS, "arch": runtime.GOARCH, "goVersion": runtime.Version(), "localMedia": s.deps.Media != nil, "remoteMedia": s.deps.RemoteMedia != nil},
		"client":  map[string]any{"version": device.Registration.ClientVersion, "platform": device.Registration.Platform, "display": device.Registration.Display, "memory": device.Registration.Memory},
		"probes":  probes, "modules": modules, "activePlayback": active, "recentFailures": failures,
		"limitations": []string{"Module state is not account or media acceptance.", "Absent functional probe evidence remains UNKNOWN.", "URLs, credentials, installation IDs, fingerprints, titles and session tickets are excluded."},
	}
	if hardware := device.Registration.Hardware; hardware != nil {
		report["hardware"] = map[string]any{"cores": hardware.CPUCores, "physicalMb": hardware.PhysicalMB, "storageFreeMb": hardware.StorageFreeMB, "glesVersion": hardware.GLES, "network": hardware.Network, "gatewayLatencyMs": hardware.GatewayLatencyMS, "decoderCount": len(hardware.Decoders), "nativeDial": hardware.NativeDIAL, "multicast": hardware.Multicast}
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, report)
}
