package server

import (
	"crypto/rand"
	"net/http"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/playback"
)

const networkSampleBytes = 1 << 20

type networkSample struct {
	id       string
	started  time.Time
	measured time.Time
	kbps     int64
}

// A single fixed-size, uncompressed sample, never a general download proxy.
func (s *Server) networkDownload(w http.ResponseWriter, r *http.Request, d domain.Device) {
	s.mu.Lock()
	now := time.Now()
	previous := s.networkSamples[d.ID]
	if now.Sub(previous.started) < time.Minute {
		s.mu.Unlock()
		fail(w, 429, "network_sample_cooldown")
		return
	}
	select {
	case s.networkJobs <- struct{}{}:
	default:
		s.mu.Unlock()
		fail(w, 429, "network_sample_busy")
		return
	}
	defer func() { <-s.networkJobs }()
	for id, sample := range s.networkSamples {
		if now.Sub(sample.started) > 5*time.Minute {
			delete(s.networkSamples, id)
		}
	}
	if len(s.networkSamples) >= 128 {
		s.mu.Unlock()
		fail(w, 429, "network_sample_limit")
		return
	}
	previous.id, previous.started = randomID(16), now
	s.networkSamples[d.ID] = previous
	s.mu.Unlock()
	buffer := make([]byte, 32<<10)
	if _, err := rand.Read(buffer); err != nil {
		fail(w, 500, "network_sample_unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Encoding", "identity")
	w.Header().Set("Content-Length", "1048576")
	w.Header().Set("X-Zombie-Sample", previous.id)
	_ = http.NewResponseController(w).SetWriteDeadline(now.Add(4 * time.Second))
	for remaining := networkSampleBytes; remaining > 0; remaining -= len(buffer) {
		if r.Context().Err() != nil {
			return
		}
		if _, err := w.Write(buffer); err != nil {
			return
		}
	}
}

func (s *Server) networkReport(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var request struct {
		ID        string `json:"sampleId"`
		Bytes     int64  `json:"bytes"`
		ElapsedMS int64  `json:"elapsedMs"`
	}
	if !decode(w, r, &request) {
		return
	}
	if request.Bytes < 32768 || request.Bytes > networkSampleBytes || request.ElapsedMS < 1 || request.ElapsedMS > 4500 {
		fail(w, 400, "invalid_network_sample")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sample := s.networkSamples[d.ID]
	if sample.id == "" || sample.id != request.ID || time.Since(sample.started) > 15*time.Second {
		fail(w, 409, "expired_network_sample")
		return
	}
	sample.id = "" // One report per issued download; scoped to the paired client.
	sample.kbps = min(200000, request.Bytes*8/request.ElapsedMS)
	sample.measured = time.Now()
	s.networkSamples[d.ID] = sample
	respond(w, 200, map[string]any{"kbps": sample.kbps, "validForSeconds": 300})
}

func (s *Server) networkQuality(d domain.Device, decision playbackDecision) string {
	s.mu.Lock()
	sample := s.networkSamples[d.ID]
	s.mu.Unlock()
	if time.Since(sample.measured) > 5*time.Minute || decision.mode == "EXTERNAL_PLAYER" {
		return ""
	}
	if networkQualityHasKnownLengthOnlyFMP4(d, time.Now()) {
		return ""
	}
	// Conversion must not bypass known failures of its output path.
	for _, probe := range d.Capabilities.Probes {
		if probe.Status == "FAIL" && (probe.ID == "http-fmp4" || probe.ID == "h264-baseline-360" || probe.ID == "aac") {
			return ""
		}
	}
	return playback.NetworkQuality(decision.metadata, sample.kbps)
}

// networkQualityHasKnownLengthOnlyFMP4 prevents a low network sample from
// selecting chunked conversion when current device evidence accepts fMP4 only
// with a known content length.
func networkQualityHasKnownLengthOnlyFMP4(device domain.Device, now time.Time) bool {
	caps := devices.CurrentCapabilities(device)
	if caps.SuiteVersion != devices.ProbeSuiteVersion || caps.DeviceID != device.ID || caps.CacheKey == "" || caps.CacheKey != devices.ProbeCacheKey(device) {
		return false
	}

	knownLengthStatus, knownLengthFresh := freshProbeOutcome(caps, "http-fmp4", now)
	if !knownLengthFresh || knownLengthStatus != "PASS" {
		return false
	}

	chunkedProbe := freshProbe(caps, "http-fmp4-chunked", now)
	if chunkedProbe == nil {
		return false
	}
	if chunkedProbe.Status == "FAIL" || chunkedProbe.Stalled {
		return true
	}
	return chunkedProbe.Status == "UNKNOWN" && probeOutcome(chunkedProbe) == "UNKNOWN" && isRecordedChunkedPrepareRejection(chunkedProbe.Detail)
}
