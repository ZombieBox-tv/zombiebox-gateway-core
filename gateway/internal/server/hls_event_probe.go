package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	hlsEventProbeID            = "hls-event-h264-aac"
	hlsEventInitialSegments    = 2
	hlsEventSegmentCount       = 7
	hlsEventPublishInterval    = 4 * time.Second
	hlsEventMaxPlaylistReloads = 32
	hlsEventRunsPerServerLimit = 32
	hlsEventRunsGlobalLimit    = 512
	hlsEventTicketLifetime     = 10 * time.Minute
)

var hlsEventSegmentFiles = [...]string{
	"hls-event-00.ts",
	"hls-event-01.ts",
	"hls-event-02.ts",
	"hls-event-03.ts",
	"hls-event-04.ts",
	"hls-event-05.ts",
	"hls-event-06.ts",
}

type hlsEventRun struct {
	deviceID             string
	expiresUnix          int64
	startedAt            time.Time
	laterPlaylistReloads int
	deliveredSegments    [hlsEventSegmentCount]bool
}

type hlsEventRunRegistryType struct {
	sync.Mutex
	byServer map[string]map[string]*hlsEventRun
}

// Runs are keyed by each server's random ticket key, which keeps this bounded
// registry isolated across Server instances without changing the server core.
var hlsEventRunRegistry = hlsEventRunRegistryType{
	byServer: make(map[string]map[string]*hlsEventRun),
}

type hlsEventProbeEvidence struct {
	APIVersion                   int    `json:"apiVersion"`
	ProbeID                      string `json:"probeId"`
	RunID                        string `json:"runId"`
	ExpiresUnixSeconds           int64  `json:"expiresUnixSeconds"`
	Started                      bool   `json:"started"`
	InitialSegmentCount          int    `json:"initialSegmentCount"`
	PublishedSegmentCount        int    `json:"publishedSegmentCount"`
	LaterPlaylistReloadCount     int    `json:"laterPlaylistReloadCount"`
	DeliveredSegmentIndexes      []int  `json:"deliveredSegmentIndexes"`
	LaterPlaylistReloadObserved  bool   `json:"laterPlaylistReloadObserved"`
	LaterSegmentDeliveryObserved bool   `json:"laterSegmentDeliveryObserved"`
	PublicationComplete          bool   `json:"publicationComplete"`
}

func (s *Server) hlsEventURLs(deviceID, expires, runID string) (string, string) {
	query := url.Values{}
	query.Set("device", deviceID)
	query.Set("expires", expires)
	query.Set("run", runID)
	query.Set("ticket", s.hlsEventSignature(deviceID, expires, runID))
	playlistURL := hlsEventURL(query)
	evidenceQuery := cloneHLSEventQuery(query)
	evidenceQuery.Set("evidence", "1")
	return playlistURL, hlsEventURL(evidenceQuery)
}

func hlsEventURL(query url.Values) string {
	return "/v1/probes/" + hlsEventProbeID + "?" + query.Encode()
}

func cloneHLSEventQuery(query url.Values) url.Values {
	clone := make(url.Values, len(query))
	for key, values := range query {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

func (s *Server) hlsEventSignature(deviceID, expires, runID string) string {
	mac := hmac.New(sha256.New, []byte(s.probeKey))
	_, _ = mac.Write([]byte(hlsEventProbeID + "\n" + deviceID + "\n" + expires + "\n" + runID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) hlsEventAssetsAvailable() bool {
	for _, name := range hlsEventSegmentFiles {
		file, err := s.probeFile(probeAsset{File: name})
		if err != nil {
			return false
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || info.Size() <= 0 {
			return false
		}
	}
	return true
}

func (s *Server) hlsEventProbe(w http.ResponseWriter, r *http.Request) {
	s.hlsEventProbeAt(w, r, time.Now())
}

// hlsEventProbeAt accepts the current time explicitly so the publication
// timeline can be exercised without sleeping in host tests.
func (s *Server) hlsEventProbeAt(w http.ResponseWriter, r *http.Request, now time.Time) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}

	deviceID, expires, runID, expiryUnix, ok := s.authorizeHLSEventProbe(r, now)
	if !ok {
		fail(w, http.StatusForbidden, "invalid_probe_ticket")
		return
	}

	query := r.URL.Query()
	_, hasEvidence := query["evidence"]
	_, hasSegment := query["segment"]
	if hasEvidence && hasSegment {
		fail(w, http.StatusBadRequest, "invalid_probe_request")
		return
	}
	if hasEvidence {
		if query.Get("evidence") != "1" {
			fail(w, http.StatusBadRequest, "invalid_probe_request")
			return
		}
		s.serveHLSEventEvidence(w, deviceID, expires, runID, expiryUnix, now)
		return
	}
	if hasSegment {
		values := query["segment"]
		if len(values) != 1 {
			fail(w, http.StatusBadRequest, "invalid_probe_request")
			return
		}
		index, err := strconv.Atoi(values[0])
		if err != nil || strconv.Itoa(index) != values[0] || index < 0 || index >= hlsEventSegmentCount {
			fail(w, http.StatusBadRequest, "invalid_probe_request")
			return
		}
		s.serveHLSEventSegment(w, r, deviceID, expires, runID, expiryUnix, index, now)
		return
	}
	s.serveHLSEventPlaylist(w, deviceID, expires, runID, expiryUnix, now)
}

func (s *Server) authorizeHLSEventProbe(r *http.Request, now time.Time) (string, string, string, int64, bool) {
	query := r.URL.Query()
	deviceID := query.Get("device")
	expires := query.Get("expires")
	runID := query.Get("run")
	ticket := query.Get("ticket")
	expiryUnix, err := strconv.ParseInt(expires, 10, 64)
	if err != nil ||
		!installationID.MatchString(deviceID) ||
		!validHLSEventRunID(runID) ||
		now.Unix() > expiryUnix ||
		expiryUnix > now.Add(hlsEventTicketLifetime).Unix() ||
		!hmac.Equal([]byte(s.hlsEventSignature(deviceID, expires, runID)), []byte(ticket)) {
		return "", "", "", 0, false
	}
	return deviceID, expires, runID, expiryUnix, true
}

func validHLSEventRunID(runID string) bool {
	if len(runID) != 32 || strings.ToLower(runID) != runID {
		return false
	}
	decoded, err := hex.DecodeString(runID)
	return err == nil && len(decoded) == 16
}

func (s *Server) serveHLSEventPlaylist(
	w http.ResponseWriter,
	deviceID, expires, runID string,
	expiryUnix int64,
	now time.Time,
) {
	if !s.hlsEventAssetsAvailable() {
		fail(w, http.StatusNotFound, "probe_unavailable")
		return
	}
	run, ok := hlsEventRunRegistry.observePlaylist(s.probeKey, runID, deviceID, expiryUnix, now)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "probe_run_limit")
		return
	}
	published := hlsEventPublishedSegments(run.startedAt, now)
	query := url.Values{}
	query.Set("device", deviceID)
	query.Set("expires", expires)
	query.Set("run", runID)
	query.Set("ticket", s.hlsEventSignature(deviceID, expires, runID))

	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n")
	playlist.WriteString("#EXT-X-VERSION:3\n")
	playlist.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
	playlist.WriteString("#EXT-X-TARGETDURATION:4\n")
	playlist.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	for index := 0; index < published; index++ {
		playlist.WriteString("#EXTINF:4.000,\n")
		segmentQuery := cloneHLSEventQuery(query)
		segmentQuery.Set("segment", strconv.Itoa(index))
		// This is relative to the playlist itself. The Co-Star's HLS parser
		// concatenates its base path even when a URI begins with '/', which
		// otherwise produces /v1/probes//v1/probes/... and a failed fetch.
		playlist.WriteString(hlsEventProbeID)
		playlist.WriteByte('?')
		playlist.WriteString(segmentQuery.Encode())
		playlist.WriteByte('\n')
	}
	if published == hlsEventSegmentCount {
		playlist.WriteString("#EXT-X-ENDLIST\n")
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(playlist.String()))
}

func hlsEventPublishedSegments(startedAt, now time.Time) int {
	if startedAt.IsZero() || !now.After(startedAt) {
		return hlsEventInitialSegments
	}
	additional := int(now.Sub(startedAt) / hlsEventPublishInterval)
	published := hlsEventInitialSegments + additional
	if published > hlsEventSegmentCount {
		return hlsEventSegmentCount
	}
	return published
}

func (s *Server) serveHLSEventSegment(
	w http.ResponseWriter,
	r *http.Request,
	deviceID, expires, runID string,
	expiryUnix int64,
	index int,
	now time.Time,
) {
	run, ok := hlsEventRunRegistry.get(s.probeKey, runID, deviceID, expiryUnix)
	if !ok || run.startedAt.IsZero() || index >= hlsEventPublishedSegments(run.startedAt, now) {
		fail(w, http.StatusNotFound, "probe_unavailable")
		return
	}
	file, err := s.probeFile(probeAsset{File: hlsEventSegmentFiles[index]})
	if err != nil {
		fail(w, http.StatusNotFound, "probe_unavailable")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 8<<20 {
		fail(w, http.StatusNotFound, "probe_unavailable")
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	tracked := &hlsEventDeliveryWriter{ResponseWriter: w}
	http.ServeContent(tracked, r, hlsEventSegmentFiles[index], info.ModTime(), file)
	if r.Method == http.MethodGet && tracked.complete(info.Size()) {
		hlsEventRunRegistry.recordSegment(s.probeKey, runID, deviceID, expiryUnix, index)
	}
}

type hlsEventDeliveryWriter struct {
	http.ResponseWriter
	status     int
	written    int64
	writeError bool
}

func (w *hlsEventDeliveryWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *hlsEventDeliveryWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	written, err := w.ResponseWriter.Write(data)
	w.written += int64(written)
	if err != nil || written != len(data) {
		w.writeError = true
	}
	return written, err
}

func (w *hlsEventDeliveryWriter) complete(segmentSize int64) bool {
	return !w.writeError && w.written == segmentSize && (w.status == http.StatusOK || w.status == http.StatusPartialContent)
}

func (s *Server) serveHLSEventEvidence(
	w http.ResponseWriter,
	deviceID, expires, runID string,
	expiryUnix int64,
	now time.Time,
) {
	evidence := hlsEventProbeEvidence{
		APIVersion:              1,
		ProbeID:                 hlsEventProbeID,
		RunID:                   runID,
		ExpiresUnixSeconds:      expiryUnix,
		DeliveredSegmentIndexes: []int{},
	}
	if run, ok := hlsEventRunRegistry.get(s.probeKey, runID, deviceID, expiryUnix); ok {
		evidence.Started = true
		evidence.InitialSegmentCount = hlsEventInitialSegments
		evidence.PublishedSegmentCount = hlsEventPublishedSegments(run.startedAt, now)
		evidence.LaterPlaylistReloadCount = run.laterPlaylistReloads
		evidence.LaterPlaylistReloadObserved = run.laterPlaylistReloads > 0
		evidence.PublicationComplete = evidence.PublishedSegmentCount == hlsEventSegmentCount
		for index, delivered := range run.deliveredSegments {
			if !delivered {
				continue
			}
			evidence.DeliveredSegmentIndexes = append(evidence.DeliveredSegmentIndexes, index)
			if index >= hlsEventInitialSegments {
				evidence.LaterSegmentDeliveryObserved = true
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, http.StatusOK, evidence)
}

func (registry *hlsEventRunRegistryType) observePlaylist(serverKey, runID, deviceID string, expiryUnix int64, now time.Time) (hlsEventRun, bool) {
	registry.Lock()
	defer registry.Unlock()
	registry.pruneLocked(now)
	serverRuns := registry.byServer[serverKey]
	if serverRuns == nil {
		serverRuns = make(map[string]*hlsEventRun)
		registry.byServer[serverKey] = serverRuns
	}
	if run := serverRuns[runID]; run != nil {
		if run.deviceID != deviceID || run.expiresUnix != expiryUnix {
			return hlsEventRun{}, false
		}
		if run.laterPlaylistReloads < hlsEventMaxPlaylistReloads {
			run.laterPlaylistReloads++
		}
		return *run, true
	}
	if len(serverRuns) >= hlsEventRunsPerServerLimit || registry.countLocked() >= hlsEventRunsGlobalLimit {
		if len(serverRuns) == 0 {
			delete(registry.byServer, serverKey)
		}
		return hlsEventRun{}, false
	}
	run := &hlsEventRun{deviceID: deviceID, expiresUnix: expiryUnix, startedAt: now}
	serverRuns[runID] = run
	return *run, true
}

func (registry *hlsEventRunRegistryType) get(serverKey, runID, deviceID string, expiryUnix int64) (hlsEventRun, bool) {
	registry.Lock()
	defer registry.Unlock()
	serverRuns := registry.byServer[serverKey]
	if serverRuns == nil {
		return hlsEventRun{}, false
	}
	run := serverRuns[runID]
	if run == nil || run.deviceID != deviceID || run.expiresUnix != expiryUnix {
		return hlsEventRun{}, false
	}
	return *run, true
}

func (registry *hlsEventRunRegistryType) recordSegment(serverKey, runID, deviceID string, expiryUnix int64, index int) {
	registry.Lock()
	defer registry.Unlock()
	serverRuns := registry.byServer[serverKey]
	if serverRuns == nil {
		return
	}
	run := serverRuns[runID]
	if run == nil || run.deviceID != deviceID || run.expiresUnix != expiryUnix || index < 0 || index >= len(run.deliveredSegments) {
		return
	}
	run.deliveredSegments[index] = true
}

func (registry *hlsEventRunRegistryType) pruneLocked(now time.Time) {
	for serverKey, serverRuns := range registry.byServer {
		for runID, run := range serverRuns {
			if now.Unix() > run.expiresUnix {
				delete(serverRuns, runID)
			}
		}
		if len(serverRuns) == 0 {
			delete(registry.byServer, serverKey)
		}
	}
}

func (registry *hlsEventRunRegistryType) countLocked() int {
	count := 0
	for _, serverRuns := range registry.byServer {
		count += len(serverRuns)
	}
	return count
}
