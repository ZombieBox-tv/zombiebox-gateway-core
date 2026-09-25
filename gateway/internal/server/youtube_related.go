package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

var youtubeVideoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

type relatedCursor struct {
	VideoID string `json:"v"`
	Offset  int    `json:"o"`
	Query   string `json:"q,omitempty"`
}

type recentYouTubeContext struct {
	Query     string
	Title     string
	VideoID   string
	UpdatedAt time.Time
}

type relatedCacheEntry struct {
	Fetched      time.Time
	Items        []domain.Item
	CurrentVideo *domain.Item
	NextCursor   string
	HasMore      bool
	Seen         map[string]bool
}

func encodeRelatedCursor(c relatedCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeRelatedCursor(raw, videoID string) (relatedCursor, error) {
	if len(raw) > 512 {
		return relatedCursor{}, errors.New("cursor too long")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		data, err = base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return relatedCursor{}, errors.New("invalid base64")
		}
	}
	var c relatedCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return relatedCursor{}, errors.New("invalid json")
	}
	if c.VideoID != videoID {
		return relatedCursor{}, errors.New("cursor video mismatch")
	}
	if c.Offset < 0 || c.Offset > 360 {
		return relatedCursor{}, errors.New("offset out of bounds")
	}
	return c, nil
}

func (s *Server) recordRecentYouTubeContext(device, title, query, videoID string) {
	if device == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.youtubeContexts == nil {
		s.youtubeContexts = map[string]recentYouTubeContext{}
	}
	if len(s.youtubeContexts) >= 64 {
		var oldestDev string
		var oldestTime time.Time
		for dev, ctx := range s.youtubeContexts {
			if oldestDev == "" || ctx.UpdatedAt.Before(oldestTime) {
				oldestDev = dev
				oldestTime = ctx.UpdatedAt
			}
		}
		delete(s.youtubeContexts, oldestDev)
	}
	ctx := s.youtubeContexts[device]
	if query != "" {
		ctx.Query = query
	}
	if title != "" {
		ctx.Title = title
	}
	if videoID != "" {
		ctx.VideoID = videoID
	}
	ctx.UpdatedAt = time.Now()
	s.youtubeContexts[device] = ctx
	delete(s.youtubeHomeFeeds, device)
}

func (s *Server) getRecentYouTubeContext(ctx context.Context, device string) string {
	s.mu.Lock()
	if s.youtubeContexts != nil {
		if ctx, ok := s.youtubeContexts[device]; ok && time.Since(ctx.UpdatedAt) < 24*time.Hour {
			if ctx.Query != "" {
				s.mu.Unlock()
				return ctx.Query
			}
			if ctx.Title != "" {
				s.mu.Unlock()
				return ctx.Title
			}
			if ctx.VideoID != "" {
				s.mu.Unlock()
				return ctx.VideoID
			}
		}
	}
	if entry, ok := s.searchResults[device]; ok && entry.query != "" && time.Since(entry.fetched) < 24*time.Hour {
		s.mu.Unlock()
		return entry.query
	}
	s.mu.Unlock()

	// The in-memory context disappears when Full restarts. Reuse the device's
	// existing bounded watch history so a prior YouTube session can still seed
	// the feed without introducing a second personal-data store.
	stored, err := s.db.List(ctx, "progress:"+device)
	if err != nil {
		return ""
	}
	var latest domain.Progress
	for _, raw := range stored {
		var progress domain.Progress
		if json.Unmarshal(raw, &progress) != nil || progress.Item.Provider != "youtube" {
			continue
		}
		if progress.UpdatedAt > latest.UpdatedAt {
			latest = progress
		}
	}
	if latest.UpdatedAt <= 0 || time.Since(time.Unix(latest.UpdatedAt, 0)) > 30*24*time.Hour {
		return ""
	}
	title := strings.TrimSpace(latest.Item.Title)
	if title != "" && !strings.EqualFold(title, "YouTube") {
		if chars := []rune(title); len(chars) > 120 {
			title = string(chars[:120])
		}
		return title
	}
	videoID := strings.TrimPrefix(latest.Item.ID, "youtube-")
	if youtubeVideoIDPattern.MatchString(videoID) {
		return videoID
	}
	return ""
}

func (s *Server) findYouTubeVideo(ctx context.Context, device, videoID string) *domain.Item {
	fullID := "youtube-" + videoID
	var placeholder *domain.Item

	// 1. Check active playback sessions
	s.mu.Lock()
	for _, sess := range s.sessions {
		if sess.source.Item.ID == fullID || sess.source.Item.ID == videoID {
			itemCopy := sess.source.Item
			if itemCopy.Title != "" && itemCopy.Title != "YouTube" {
				s.mu.Unlock()
				return &itemCopy
			}
			if placeholder == nil {
				placeholder = &itemCopy
			}
		}
	}
	s.mu.Unlock()

	// 2. Check browse source
	if src := s.browseSource(ctx, device, fullID); src != nil {
		itemCopy := src.Item
		if itemCopy.Title != "" && itemCopy.Title != "YouTube" {
			return &itemCopy
		}
		if placeholder == nil {
			placeholder = &itemCopy
		}
	}

	// 3. Check search source
	if src := s.searchSource(ctx, device, fullID); src != nil {
		itemCopy := src.Item
		if itemCopy.Title != "" && itemCopy.Title != "YouTube" {
			return &itemCopy
		}
		if placeholder == nil {
			placeholder = &itemCopy
		}
	}

	// 4. Check YouTube receiver
	if s.youtubeReceiver != nil {
		if src := s.youtubeReceiver.Source(device, fullID); src != nil {
			itemCopy := src.Item
			if itemCopy.Title != "" && itemCopy.Title != "YouTube" {
				return &itemCopy
			}
			if placeholder == nil {
				placeholder = &itemCopy
			}
		}
	}

	// 5. Check recent context
	s.mu.Lock()
	if s.youtubeContexts != nil {
		if ctxVal, ok := s.youtubeContexts[device]; ok && ctxVal.VideoID == videoID && ctxVal.Title != "" && ctxVal.Title != "YouTube" {
			item := domain.Item{
				ID:       fullID,
				Provider: "youtube",
				Kind:     "video",
				Title:    ctxVal.Title,
				Playable: true,
			}
			s.mu.Unlock()
			return &item
		}
	}
	s.mu.Unlock()

	return placeholder
}

func (s *Server) youtubeRelated(w http.ResponseWriter, r *http.Request, d domain.Device) {
	rawVideo := strings.TrimSpace(r.URL.Query().Get("video"))
	if rawVideo == "" {
		rawVideo = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if rawVideo == "" {
		rawVideo = strings.TrimSpace(r.PathValue("video"))
	}
	videoID := strings.TrimPrefix(rawVideo, "youtube-")
	if !youtubeVideoIDPattern.MatchString(videoID) {
		fail(w, 400, "invalid_video_id")
		return
	}

	offset := 0
	query := ""
	rawCursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if rawCursor != "" {
		parsed, err := decodeRelatedCursor(rawCursor, videoID)
		if err != nil {
			fail(w, 400, "invalid_cursor")
			return
		}
		offset = parsed.Offset
		query = parsed.Query
	} else if rawOffset := strings.TrimSpace(r.URL.Query().Get("offset")); rawOffset != "" {
		n, err := strconv.Atoi(rawOffset)
		if err != nil || n < 0 || n > 360 {
			fail(w, 400, "invalid_offset")
			return
		}
		offset = n
	}

	limit := 20
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 1 || n > 40 {
			fail(w, 400, "invalid_limit")
			return
		}
		limit = n
	}

	s.mu.Lock()
	config, revision := s.config(r.Context(), "youtube"), s.configRevision["youtube"]
	s.mu.Unlock()
	if !config.Enabled {
		fail(w, 409, "provider_disabled")
		return
	}

	cacheKey := d.ID + ":" + videoID + ":" + strconv.Itoa(offset) + ":" + strconv.Itoa(limit)
	s.mu.Lock()
	if s.relatedCache != nil {
		if entry, ok := s.relatedCache[cacheKey]; ok && time.Since(entry.Fetched) < 2*time.Minute {
			s.mu.Unlock()
			respond(w, 200, domain.RelatedPage{
				APIVersion:   1,
				VideoID:      videoID,
				CurrentVideo: entry.CurrentVideo,
				Items:        entry.Items,
				NextCursor:   entry.NextCursor,
				HasMore:      entry.HasMore,
				Total:        len(entry.Items),
			})
			return
		}
	}
	s.mu.Unlock()

	reqCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	currentVideo := s.findYouTubeVideo(reqCtx, d.ID, videoID)
	if currentVideo == nil {
		s.mu.Lock()
		if s.relatedCache != nil {
			for k, v := range s.relatedCache {
				if strings.HasPrefix(k, d.ID+":"+videoID+":") && v.CurrentVideo != nil && v.CurrentVideo.Title != "" && v.CurrentVideo.Title != "YouTube" {
					copy := *v.CurrentVideo
					currentVideo = &copy
					break
				}
			}
		}
		s.mu.Unlock()
	}

	if query == "" {
		if currentVideo != nil && currentVideo.Title != "" && currentVideo.Title != "YouTube" {
			query = currentVideo.Title
		} else {
			query = videoID
		}
	}

	result, err := s.deps.Browse.Browse(reqCtx, "youtube", config, "", query, offset)
	if err != nil {
		if errors.Is(reqCtx.Err(), context.Canceled) {
			return
		}
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			fail(w, 504, "provider_timeout")
			return
		}
		// The optional YouTube worker accepts one operation at a time. A browse
		// racing playback resolution can fail transiently; returning a successful
		// empty page would make clients treat that failure as a real empty feed.
		fail(w, 503, "provider_unavailable")
		return
	}
	page := s.browse.Present(reqCtx, d.ID, "youtube", providers.Titles["youtube"], revision, config, "", query, offset, result)

	// Check if browse result contained the current video
	for _, item := range page.Items {
		cleanID := strings.TrimPrefix(item.ID, "youtube-")
		if cleanID == videoID {
			itemCopy := item
			currentVideo = &itemCopy
			s.recordRecentYouTubeContext(d.ID, item.Title, item.Title, videoID)
			break
		}
	}

	// Ensure seed video metadata is available for DIAL playback and not left blank
	// merely because a title query fails to return the exact seed.
	if currentVideo == nil || currentVideo.Title == "" || currentVideo.Title == "YouTube" || (currentVideo.Description == "" && currentVideo.Subtitle == "") {
		if query != videoID {
			seedResult, err := s.deps.Browse.Browse(reqCtx, "youtube", config, "", videoID, 0)
			if err == nil && len(seedResult.Sources) > 0 {
				seedPage := s.browse.Present(reqCtx, d.ID, "youtube", providers.Titles["youtube"], revision, config, "", videoID, 0, seedResult)
				for _, item := range seedPage.Items {
					if strings.TrimPrefix(item.ID, "youtube-") == videoID {
						itemCopy := item
						currentVideo = &itemCopy
						s.recordRecentYouTubeContext(d.ID, item.Title, item.Title, videoID)
						break
					}
				}
			}
		}
	}

	if currentVideo == nil {
		currentVideo = s.findYouTubeVideo(reqCtx, d.ID, videoID)
	}
	if currentVideo == nil {
		currentVideo = &domain.Item{
			ID:       "youtube-" + videoID,
			Provider: "youtube",
			Kind:     "video",
			Title:    videoID,
			Playable: true,
		}
	} else if currentVideo.Title != "" && currentVideo.Title != "YouTube" {
		s.recordRecentYouTubeContext(d.ID, currentVideo.Title, query, videoID)
	}

	seenKey := d.ID + ":" + videoID + ":seen"
	seen := map[string]bool{
		videoID:              true,
		"youtube-" + videoID: true,
	}

	if offset > 0 {
		s.mu.Lock()
		if s.relatedCache != nil {
			if entry, ok := s.relatedCache[seenKey]; ok && time.Since(entry.Fetched) < 10*time.Minute {
				for id := range entry.Seen {
					seen[id] = true
				}
			}
		}
		s.mu.Unlock()
	}

	// Filter out the current video itself and deduplicate earlier pages
	items := make([]domain.Item, 0, len(page.Items))
	for _, item := range page.Items {
		cleanID := strings.TrimPrefix(item.ID, "youtube-")
		if seen[cleanID] || seen[item.ID] {
			continue
		}
		if item.Kind != "video" || !item.Playable {
			continue
		}
		seen[cleanID] = true
		seen[item.ID] = true
		items = append(items, item)
		if len(items) >= limit {
			break
		}
	}

	nextCursor := ""
	hasMore := false
	if page.NextOffset > offset && page.NextOffset <= 360 && len(items) > 0 {
		nextCursor = encodeRelatedCursor(relatedCursor{
			VideoID: videoID,
			Offset:  page.NextOffset,
			Query:   query,
		})
		hasMore = true
	}

	// Store in cache
	s.mu.Lock()
	if s.relatedCache == nil {
		s.relatedCache = map[string]relatedCacheEntry{}
	}
	s.relatedCache[seenKey] = relatedCacheEntry{
		Fetched: time.Now(),
		Seen:    seen,
	}
	s.relatedCache[cacheKey] = relatedCacheEntry{
		Fetched:      time.Now(),
		Items:        items,
		CurrentVideo: currentVideo,
		NextCursor:   nextCursor,
		HasMore:      hasMore,
		Seen:         seen,
	}
	for len(s.relatedCache) > 256 {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range s.relatedCache {
			if k == seenKey || k == cacheKey {
				continue
			}
			if oldestKey == "" || v.Fetched.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.Fetched
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.relatedCache, oldestKey)
	}
	s.mu.Unlock()

	respond(w, 200, domain.RelatedPage{
		APIVersion:   1,
		VideoID:      videoID,
		CurrentVideo: currentVideo,
		Items:        items,
		NextCursor:   nextCursor,
		HasMore:      hasMore,
		Total:        len(items),
	})
}
