package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/playback"
)

const (
	youtubeHLSSessionMode       = "YOUTUBE_HLS"
	youtubeHLSPublisherStartup  = 18 * time.Second
	youtubeHLSPublisherPoll     = 50 * time.Millisecond
	youtubeHLSPublisherMaxVideo = 6 * time.Hour
)

var errYouTubeHLSPublisherUnavailable = errors.New("YouTube HLS publisher unavailable")
var errYouTubeHLSPublisherFirstSegmentTimeout = errors.New("YouTube HLS first segment timeout")

func youtubeHLSPublisherFailureDiagnostic(err error) (string, string) {
	switch {
	case errors.Is(err, media.ErrBusy):
		return "busy", "publisher_busy"
	case errors.Is(err, errYouTubeHLSPublisherFirstSegmentTimeout):
		return "first_segment_timeout", "publisher_first_segment_timeout"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled", "publisher_cancelled"
	default:
		return "failed", "publisher_failed"
	}
}

// canPublishYouTubeHLS gates split-stream publishing on the current receiver's
// completed EVENT probe, native decoder policy and the requested quality's
// exact or Auto-compatible source dimensions.
func (s *Server) canPublishYouTubeHLS(device domain.Device, source domain.Source, metadata *domain.Metadata, quality string) bool {
	eligible, _ := s.youtubeHLSIneligibility(device, source, metadata, quality)
	return eligible
}

func (s *Server) youtubeHLSIneligibility(device domain.Device, source domain.Source, metadata *domain.Metadata, quality string) (bool, string) {
	if s.deps.RemoteHLSPublisher == nil || s.opt.YouTubeHLSPublishDir == "" {
		if s.deps.RemoteHLSPublisher == nil {
			return false, "publisher_unavailable"
		}
		return false, "publisher_directory_unavailable"
	}
	if source.Item.Provider != "youtube" {
		return false, "source_provider"
	}
	if source.Item.Kind != "video" {
		return false, "source_kind"
	}
	if source.Live {
		return false, "source_live"
	}
	if source.URL == "" {
		return false, "source_video_missing"
	}
	if source.AudioURL == "" {
		return false, "source_audio_missing"
	}
	if source.Item.DurationMS <= 0 || source.Item.DurationMS > int64(youtubeHLSPublisherMaxVideo/time.Millisecond) {
		return false, "source_duration"
	}
	if media.ManifestKind(source) != "" {
		return false, "source_manifest"
	}
	if !media.RemoteCandidate(source) {
		return false, "source_remote_ineligible"
	}
	if metadata == nil {
		return false, "metadata_missing"
	}
	caps := devices.CurrentCapabilities(device)
	if !playback.HasFreshLiveVideoHLSEvidence(caps) {
		return false, "fresh_event_probe"
	}

	autoQuality := quality == "" || quality == "auto"
	if !autoQuality {
		switch quality {
		case "480p", "720p", "1080p":
		default:
			return false, "quality_height_ineligible"
		}
	}

	videoCount, audioCount := 0, 0
	for _, stream := range metadata.Streams {
		switch stream.Type {
		case "video":
			if stream.Codec != "h264" {
				return false, "video_codec"
			}
			if stream.Width <= 0 || stream.Height <= 0 || stream.Width > 1920 || stream.Height > 1080 {
				return false, "video_dimensions_ineligible"
			}
			if !autoQuality && playback.ClassifyDimensionsTier(stream.Width, stream.Height) != quality {
				return false, "video_height"
			}
			videoCount++
		case "audio":
			if stream.Codec != "aac" {
				return false, "audio_codec"
			}
			audioCount++
		}
	}
	if videoCount != 1 || audioCount != 1 {
		return false, "stream_count"
	}
	localMode := playback.LocalMode(*metadata, source.MIME, caps, "")
	if localMode != "DIRECT_PLAY" && localMode != "REMUX" {
		return false, "native_decoder_incompatible"
	}
	selectedMode, _ := playback.SelectedQualityMode(*metadata, source, device, quality, 0, "REMUX")
	if selectedMode != "REMUX" {
		return false, "native_decoder_incompatible"
	}
	return true, "eligible"
}

// isExactProbedYouTubeSplitTier reports whether the already-probed source is a
// non-live YouTube split H.264/AAC source whose video stream matches the exact
// requested tier height.
func isExactProbedYouTubeSplitTier(source domain.Source, metadata *domain.Metadata, quality string) bool {
	if source.Item.Provider != "youtube" || source.Item.Kind != "video" || source.Live || source.Path != "" || source.URL == "" || source.AudioURL == "" || metadata == nil {
		return false
	}
	if media.ManifestKind(source) != "" || !media.RemoteCandidate(source) {
		return false
	}
	if !playback.ValidQuality(quality) || quality == "" || quality == "auto" || quality == "STANDARD" || quality == "LOW" {
		return false
	}
	wantTier := quality
	if !strings.HasSuffix(quality, "p") {
		return false
	}
	videoCount, audioCount := 0, 0
	for _, stream := range metadata.Streams {
		switch stream.Type {
		case "video":
			if stream.Codec != "h264" || playback.ClassifyDimensionsTier(stream.Width, stream.Height) != wantTier {
				return false
			}
			videoCount++
		case "audio":
			if stream.Codec != "aac" {
				return false
			}
			audioCount++
		}
	}
	return videoCount == 1 && audioCount == 1
}

// startYouTubeHLSPublication reserves the one gateway-owned output slot and
// waits only for the first committed segment. The rest of the EVENT playlist
// continues to publish in the background under the playback session context.
func (s *Server) startYouTubeHLSPublication(ctx context.Context, source domain.Source, selection domain.MediaSelection) (RemoteHLSPublication, error) {
	select {
	case s.youtubeHLSPublishers <- struct{}{}:
	default:
		return nil, media.ErrBusy
	}
	releaseSlot := sync.OnceFunc(func() { <-s.youtubeHLSPublishers })
	publication, err := s.deps.RemoteHLSPublisher.StartRemoteHLSPublisher(ctx, source, selection, s.opt.YouTubeHLSPublishDir)
	if err != nil {
		releaseSlot()
		return nil, err
	}
	if publication == nil {
		releaseSlot()
		return nil, errYouTubeHLSPublisherUnavailable
	}
	leased := &leasedRemoteHLSPublication{RemoteHLSPublication: publication, release: releaseSlot}
	if err := waitForYouTubeHLSPublication(ctx, leased); err != nil {
		leased.Close()
		return nil, err
	}
	go func() {
		<-ctx.Done()
		leased.Close()
	}()
	return leased, nil
}

type leasedRemoteHLSPublication struct {
	RemoteHLSPublication
	release func()
	once    sync.Once
}

func (p *leasedRemoteHLSPublication) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		p.RemoteHLSPublication.Close()
		p.release()
	})
}

func waitForYouTubeHLSPublication(ctx context.Context, publication RemoteHLSPublication) error {
	deadline := time.NewTimer(youtubeHLSPublisherStartup)
	defer deadline.Stop()
	ticker := time.NewTicker(youtubeHLSPublisherPoll)
	defer ticker.Stop()
	for {
		snapshot, err := publication.Snapshot()
		if err != nil {
			return errYouTubeHLSPublisherUnavailable
		}
		if len(snapshot.Segments) > 0 && len(snapshot.Playlist) > 0 {
			if _, err := rewriteYouTubeHLSEventPlaylist("session", "ticket", snapshot); err != nil {
				return errYouTubeHLSPublisherUnavailable
			}
			return nil
		}
		if snapshot.State == "failed" || snapshot.State == "cancelled" || snapshot.State == "closed" {
			if snapshot.Error == media.ErrBusy.Error() {
				return media.ErrBusy
			}
			return errYouTubeHLSPublisherUnavailable
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errYouTubeHLSPublisherFirstSegmentTimeout
		case <-ticker.C:
		}
	}
}

func (s *Server) serveYouTubeHLSPublication(w http.ResponseWriter, r *http.Request, sessionID, resource string, sess *session) {
	if sess == nil || sess.youtubeHLSPublisher == nil || sess.ctx.Err() != nil {
		fail(w, http.StatusGone, "media_unavailable")
		return
	}
	snapshot, err := sess.youtubeHLSPublisher.Snapshot()
	if err != nil {
		fail(w, http.StatusGone, "media_unavailable")
		return
	}
	switch snapshot.State {
	case "failed", "cancelled", "closed":
		s.retireYouTubeHLSSession(sessionID, sess)
		fail(w, http.StatusGone, "publisher_failed")
		return
	}
	if resource == "" {
		playlist, err := rewriteYouTubeHLSEventPlaylist(sessionID, sess.ticket, snapshot)
		if err != nil {
			if len(snapshot.Segments) == 0 {
				w.Header().Set("Retry-After", "1")
				fail(w, http.StatusServiceUnavailable, "media_starting")
				return
			}
			fail(w, http.StatusBadGateway, "invalid_playlist")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
		w.Header().Set("Cache-Control", "private, no-store")
		if r.Method != http.MethodHead {
			_, _ = w.Write(playlist)
		}
		return
	}
	if path.Base(resource) != resource || !strings.HasPrefix(resource, "segment-") || !strings.HasSuffix(resource, ".ts") {
		fail(w, http.StatusNotFound, "resource_not_found")
		return
	}
	var expected int64
	for _, segment := range snapshot.Segments {
		if segment.Name == resource {
			expected = segment.Bytes
			break
		}
	}
	if expected <= 0 {
		fail(w, http.StatusNotFound, "resource_not_found")
		return
	}
	file, size, err := sess.youtubeHLSPublisher.OpenSegment(resource)
	if err != nil || file == nil || size != expected {
		if file != nil {
			_ = file.Close()
		}
		fail(w, http.StatusNotFound, "resource_not_found")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, resource, time.Time{}, file)
}

func (s *Server) retireYouTubeHLSSession(sessionID string, sess *session) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	if s.sessions[sessionID] == sess {
		delete(s.sessions, sessionID)
		sess.cancel()
		s.events.publish(sess.device, "playback.stopped", map[string]string{"sessionId": sessionID})
	}
	s.mu.Unlock()
	if sess.youtubeHLSPublisher != nil {
		sess.youtubeHLSPublisher.Close()
	}
}

func rewriteYouTubeHLSEventPlaylist(sessionID, ticket string, snapshot RemoteHLSPublicationSnapshot) ([]byte, error) {
	if sessionID == "" || ticket == "" || len(snapshot.Segments) == 0 || len(snapshot.Playlist) == 0 || len(snapshot.Playlist) > 2<<20 {
		return nil, errYouTubeHLSPublisherUnavailable
	}
	segmentNames := make(map[string]struct{}, len(snapshot.Segments))
	for _, segment := range snapshot.Segments {
		if segment.Name == "" || path.Base(segment.Name) != segment.Name || !strings.HasPrefix(segment.Name, "segment-") || !strings.HasSuffix(segment.Name, ".ts") || segment.Bytes < 188 {
			return nil, errYouTubeHLSPublisherUnavailable
		}
		segmentNames[segment.Name] = struct{}{}
	}
	lines := strings.Split(strings.ReplaceAll(string(snapshot.Playlist), "\r\n", "\n"), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, errYouTubeHLSPublisherUnavailable
	}
	sawEvent, sawSegment := false, false
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:EVENT") {
				sawEvent = true
			}
			if strings.Contains(line, "URI=") || strings.Contains(line, "http://") || strings.Contains(line, "https://") || strings.Contains(line, "../") {
				return nil, errYouTubeHLSPublisherUnavailable
			}
			lines[i] = line
			continue
		}
		if _, ok := segmentNames[line]; !ok {
			return nil, errYouTubeHLSPublisherUnavailable
		}
		lines[i] = sessionID + "/" + line + "?ticket=" + url.QueryEscape(ticket)
		sawSegment = true
	}
	if !sawEvent || !sawSegment {
		return nil, errYouTubeHLSPublisherUnavailable
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// youtubeStreamTransportChoice describes how chooseYouTubeStreamTransport
// selected a transport for a split YouTube source.
type youtubeStreamTransportChoice struct {
	// Mode is the selected playback mode (one of youtubeHLSSessionMode,
	// "REMUX", or the original legacyMode passed in).
	Mode string
	// HLSEligible reports whether the HLS gate returned true. Callers may
	// record this in diagnostic logs without calling youtubeHLSIneligibility a
	// second time.
	HLSEligible bool
	// HLSReason is the gate reason string (always set when source is YouTube).
	HLSReason string
	// PreferredStreaming reports whether fresh chunked evidence made REMUX the
	// first choice (native_streaming_preferred).
	PreferredStreaming bool
}

// chooseYouTubeStreamTransport selects the best transport for a split YouTube
// source (separate video+audio URLs) given the device capabilities and a
// quality tier.  It is called for both playback creation and quality changes.
//
// Rules (intentionally conservative – never manufacture evidence):
//
//  1. If the source is not a YouTube split stream, return the legacyMode
//     unchanged; the caller handles non-YouTube and combined sources.
//  2. At positionMS == 0: when the device has a fresh, current-suite-bound
//     http-fmp4-chunked PASS with advancement, prefer the existing streaming
//     REMUX path ("native_streaming_preferred"). The HLS gate is still
//     evaluated and recorded but REMUX wins.
//  3. At positionMS == 0 without verified chunked PASS, and at any nonzero
//     positionMS: if the HLS gate passes, use youtubeHLSSessionMode so the
//     native tier is preserved without a forced full-file REMUX or transcode.
//  4. Otherwise return legacyMode unchanged.
//
// The caller is responsible for applying the result, persisting quality
// preferences, and recording diagnostic stages.
func (s *Server) chooseYouTubeStreamTransport(
	device domain.Device,
	source domain.Source,
	metadata *domain.Metadata,
	quality string,
	positionMS int64,
	legacyMode string,
	now time.Time,
) youtubeStreamTransportChoice {
	// Only applies to split YouTube streams with separate audio.
	if source.Item.Provider != "youtube" || source.AudioURL == "" || source.Path != "" {
		return youtubeStreamTransportChoice{Mode: legacyMode}
	}

	// Evaluate HLS eligibility once (used in multiple branches).
	hlsEligible, hlsReason := s.youtubeHLSIneligibility(device, source, metadata, quality)

	if positionMS == 0 {
		// Check for fresh chunked streaming evidence on the current probe suite.
		caps := devices.CurrentCapabilities(device)
		if caps.SuiteVersion == devices.ProbeSuiteVersion &&
			caps.DeviceID == device.ID &&
			caps.CacheKey != "" && caps.CacheKey == devices.ProbeCacheKey(device) {
			chunkedStatus, chunkedFresh := freshProbeOutcome(caps, "http-fmp4-chunked", now)
			if chunkedFresh && chunkedStatus == "PASS" {
				// Fresh chunked PASS: prefer streaming REMUX. HLS gate
				// information is still surfaced for diagnostics.
				return youtubeStreamTransportChoice{
					Mode:               legacyMode, // keep REMUX/TRANSCODE decision to caller
					HLSEligible:        hlsEligible,
					HLSReason:          "native_streaming_preferred",
					PreferredStreaming: true,
				}
			}
		}
		// No verified chunked path: if HLS is eligible, use it.
		if hlsEligible {
			return youtubeStreamTransportChoice{
				Mode:        youtubeHLSSessionMode,
				HLSEligible: true,
				HLSReason:   hlsReason,
			}
		}
		return youtubeStreamTransportChoice{Mode: legacyMode, HLSEligible: false, HLSReason: hlsReason}
	}

	// nonzero positionMS: prefer HLS to preserve native tier without forced
	// transcode or a known-length-only full-file spool.
	if hlsEligible {
		return youtubeStreamTransportChoice{
			Mode:        youtubeHLSSessionMode,
			HLSEligible: true,
			HLSReason:   hlsReason,
		}
	}
	return youtubeStreamTransportChoice{Mode: legacyMode, HLSEligible: false, HLSReason: hlsReason}
}
