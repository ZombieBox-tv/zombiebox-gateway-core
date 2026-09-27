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

// canPublishYouTubeHLS gates split HD publishing on both the current receiver's
// completed EVENT probe and the exact codecs/resolution selected upstream.
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
	if !playback.HasFreshLiveVideoHLSEvidence(devices.CurrentCapabilities(device)) {
		return false, "fresh_event_probe"
	}

	wantHeight := 0
	switch quality {
	case "720p":
		wantHeight = 720
	case "1080p":
		wantHeight = 1080
	default:
		return false, "quality_height_ineligible"
	}

	videoCount, audioCount := 0, 0
	for _, stream := range metadata.Streams {
		switch stream.Type {
		case "video":
			if stream.Codec != "h264" {
				return false, "video_codec"
			}
			if stream.Height != wantHeight {
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
	return true, "eligible"
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
