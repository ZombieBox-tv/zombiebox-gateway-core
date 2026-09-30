package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/playback"
	"zombiebox.local/gateway/internal/providers"
)

type customStreamsRemoteMedia struct {
	qualityTestMedia
	metadata domain.Metadata
}

func (m *customStreamsRemoteMedia) Probe(context.Context, string) (domain.Metadata, error) {
	return m.metadata, nil
}

func (m *customStreamsRemoteMedia) ProbeRemote(context.Context, domain.Source) (domain.Metadata, error) {
	return m.metadata, nil
}

type countingYouTubeResolver struct {
	inner Resolver
	calls int
}

func (r *countingYouTubeResolver) Resolve(ctx context.Context, source domain.Source) (domain.Source, error) {
	r.calls++
	return r.inner.Resolve(ctx, source)
}

func addYouTubeSearchVariant(s *Server, source *domain.Source, variant string) {
	for _, existing := range source.Variants {
		if existing == variant {
			return
		}
	}
	source.Variants = append(source.Variants, variant)
	s.mu.Lock()
	result := s.searchResults["youtube-hls-device"]
	for index := range result.sources {
		if result.sources[index].Item.ID == source.Item.ID {
			result.sources[index].Variants = append(result.sources[index].Variants, variant)
		}
	}
	s.searchResults["youtube-hls-device"] = result
	s.mu.Unlock()
}

const (
	youtube804VideoURL = "https://r1.googlevideo.com/video-804?signature=upstream-secret"
	youtube804AudioURL = "https://r2.googlevideo.com/audio-804?signature=audio-secret"
)

func setupYouTube804HLSServer(t *testing.T, publisher *fakeRemoteHLSPublisher) (*Server, string, domain.Source) {
	t.Helper()
	s, token, source := setupYouTubeHLSServer(t, publisher, false)
	s.deps.RemoteMedia = &youtubeHLSQualityMedia{}
	s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
		request.URL = youtube804VideoURL
		request.AudioURL = youtube804AudioURL
		request.MIME = "video/mp4"
		request.Item.DurationMS = 60_000
		return request, nil
	}}
	return s, token, source
}

func TestYouTubeAutoHLSEligibleAuto720And1080SelectsHLSWithoutExtraResolve(t *testing.T) {
	t.Run("auto 720p", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, true)
		countingResolver := &countingYouTubeResolver{inner: s.deps.Resolver}
		s.deps.Resolver = countingResolver

		diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
		defer restoreLogs()

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		request := httptest.NewRequest(http.MethodPost, "/v1/playback", strings.NewReader(body))
		request.Header.Set("X-Zombie-Device", "youtube-hls-device")
		request.Header.Set("Authorization", "Bearer "+token)
		created := httptest.NewRecorder()
		s.ServeHTTP(created, request)
		if created.Code != http.StatusCreated {
			t.Fatalf("create playback status=%d body=%s", created.Code, created.Body)
		}

		if countingResolver.calls != 1 {
			t.Fatalf("resolver called %d times, want exactly 1 (no extra resolve)", countingResolver.calls)
		}

		var plan domain.Plan
		if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" || plan.ResumeMS != 0 || plan.TimelineOffsetMS != 0 {
			t.Fatalf("unexpected plan semantics for Auto 720p HLS: %+v", plan)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		selected := publisher.lastSelection
		publisher.mu.Unlock()
		if starts != 1 {
			t.Fatalf("publisher starts = %d, want 1", starts)
		}
		if selected.PositionMS != 0 {
			t.Fatalf("publisher PositionMS = %d, want 0", selected.PositionMS)
		}

		entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
		if len(entries) != 1 {
			t.Fatalf("terminal diagnostic count = %d, want 1; logs: %s", len(entries), diagnosticLogs.String())
		}
		var diagnostic youtubePlaybackDiagnosticEvent
		if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
			t.Fatalf("decode terminal diagnostic: %v", err)
		}
		if diagnostic.RequestedQuality != "auto" || diagnostic.ChosenQuality != "auto" || diagnostic.DeliveryRoute != "youtube_hls" || diagnostic.Stage != "playback" || diagnostic.Outcome != "success" {
			t.Fatalf("unexpected terminal diagnostic: %+v", diagnostic)
		}
		if diagnostic.HLSGate != "eligible" || diagnostic.HLSGateReason != "eligible" || diagnostic.Publisher != "ready_first_segment" {
			t.Fatalf("HLS stages not recorded: %+v", diagnostic)
		}

		// Ensure user preference was not silently changed.
		if pref := s.getQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video"); pref != "" && pref != "auto" {
			t.Fatalf("quality preference was silently persisted as %q", pref)
		}
	})

	t.Run("auto 1080p", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, false)
		s.deps.RemoteMedia = &qualityTestMedia{width: 1920, height: 1080, codec: "h264", profile: "High"}
		s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
			request.URL = "https://r1.googlevideo.com/video-1080?signature=upstream-secret"
			request.AudioURL = "https://r2.googlevideo.com/audio?signature=audio-secret"
			request.MIME = "video/mp4"
			request.Item.DurationMS = 60_000
			return request, nil
		}}
		countingResolver := &countingYouTubeResolver{inner: s.deps.Resolver}
		s.deps.Resolver = countingResolver

		diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
		defer restoreLogs()

		body := fmt.Sprintf(`{"itemId":%q,"mode":"AUTO","quality":"auto"}`, source.Item.ID)
		request := httptest.NewRequest(http.MethodPost, "/v1/playback", strings.NewReader(body))
		request.Header.Set("X-Zombie-Device", "youtube-hls-device")
		request.Header.Set("Authorization", "Bearer "+token)
		created := httptest.NewRecorder()
		s.ServeHTTP(created, request)
		if created.Code != http.StatusCreated {
			t.Fatalf("create playback status=%d body=%s", created.Code, created.Body)
		}

		if countingResolver.calls != 1 {
			t.Fatalf("resolver called %d times, want exactly 1 (no extra resolve)", countingResolver.calls)
		}

		var plan domain.Plan
		if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" {
			t.Fatalf("unexpected plan for Auto 1080p HLS: %+v", plan)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		publisher.mu.Unlock()
		if starts != 1 {
			t.Fatalf("publisher starts = %d, want 1", starts)
		}

		entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
		if len(entries) != 1 {
			t.Fatalf("terminal diagnostic count = %d, want 1; logs: %s", len(entries), diagnosticLogs.String())
		}
		var diagnostic youtubePlaybackDiagnosticEvent
		if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
			t.Fatalf("decode terminal diagnostic: %v", err)
		}
		if diagnostic.ChosenQuality != "auto" || diagnostic.DeliveryRoute != "youtube_hls" || diagnostic.HLSGate != "eligible" {
			t.Fatalf("unexpected diagnostic for Auto 1080p: %+v", diagnostic)
		}
	})
}

func TestYouTubeAutoHLSResume31064ReachesPublisherOffset(t *testing.T) {
	const resumePosition = int64(31064)

	t.Run("explicit position in request body", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, true)

		body := fmt.Sprintf(`{"itemId":%q,"positionMs":%d}`, source.Item.ID, resumePosition)
		request := httptest.NewRequest(http.MethodPost, "/v1/playback", strings.NewReader(body))
		request.Header.Set("X-Zombie-Device", "youtube-hls-device")
		request.Header.Set("Authorization", "Bearer "+token)
		created := httptest.NewRecorder()
		s.ServeHTTP(created, request)
		if created.Code != http.StatusCreated {
			t.Fatalf("create playback status=%d body=%s", created.Code, created.Body)
		}

		var plan domain.Plan
		if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" || plan.ResumeMS != 0 || plan.TimelineOffsetMS != resumePosition {
			t.Fatalf("plan timeline offset lost: %+v", plan)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		selected := publisher.lastSelection
		publisher.mu.Unlock()
		if starts != 1 {
			t.Fatalf("publisher starts = %d, want 1", starts)
		}
		if selected.PositionMS != resumePosition {
			t.Fatalf("publisher selection position = %d, want %d", selected.PositionMS, resumePosition)
		}
	})

	t.Run("resume position from stored progress", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, true)

		if err := s.db.Put(t.Context(), "progress:youtube-hls-device", source.Item.ID, domain.Progress{
			Item:       source.Item,
			PositionMS: resumePosition,
			DurationMS: 60_000,
			State:      "PAUSED",
		}); err != nil {
			t.Fatal(err)
		}

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		request := httptest.NewRequest(http.MethodPost, "/v1/playback", strings.NewReader(body))
		request.Header.Set("X-Zombie-Device", "youtube-hls-device")
		request.Header.Set("Authorization", "Bearer "+token)
		created := httptest.NewRecorder()
		s.ServeHTTP(created, request)
		if created.Code != http.StatusCreated {
			t.Fatalf("create playback status=%d body=%s", created.Code, created.Body)
		}

		var plan domain.Plan
		if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" || plan.ResumeMS != 0 || plan.TimelineOffsetMS != resumePosition {
			t.Fatalf("plan timeline offset lost: %+v", plan)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		selected := publisher.lastSelection
		publisher.mu.Unlock()
		if starts != 1 {
			t.Fatalf("publisher starts = %d, want 1", starts)
		}
		if selected.PositionMS != resumePosition {
			t.Fatalf("publisher selection position = %d, want %d", selected.PositionMS, resumePosition)
		}
	})
}

func TestYouTubeAutoHLSRejectsStaleOrMissingHLSEvidence(t *testing.T) {
	t.Run("missing probe", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, true)

		var dev domain.Device
		if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &dev); err != nil {
			t.Fatal(err)
		}
		filtered := make([]domain.Probe, 0, len(dev.Capabilities.Probes))
		for _, probe := range dev.Capabilities.Probes {
			if probe.ID != "hls-event-h264-aac" {
				filtered = append(filtered, probe)
			}
		}
		dev.Capabilities.Probes = filtered
		if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
			t.Fatal(err)
		}

		diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
		defer restoreLogs()

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("playback failed: %d %s", created.Code, created.Body)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		publisher.mu.Unlock()
		if starts != 0 {
			t.Fatalf("publisher started without HLS probe evidence: starts=%d", starts)
		}

		entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
		if len(entries) != 1 {
			t.Fatalf("terminal diagnostic count = %d, want 1", len(entries))
		}
		var diagnostic youtubePlaybackDiagnosticEvent
		if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
			t.Fatal(err)
		}
		if diagnostic.HLSGate != "ineligible" || diagnostic.HLSGateReason != "fresh_event_probe" || diagnostic.Publisher != "unreached" {
			t.Fatalf("unexpected diagnostic for missing HLS probe: %+v", diagnostic)
		}
	})

	t.Run("stale probe", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, true)

		var dev domain.Device
		if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &dev); err != nil {
			t.Fatal(err)
		}
		for i := range dev.Capabilities.Probes {
			if dev.Capabilities.Probes[i].ID == "hls-event-h264-aac" {
				// 10 days old -> stale (> 7 days)
				dev.Capabilities.Probes[i].TestedAt = time.Now().Unix() - 10*24*3600
			}
		}
		if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
			t.Fatal(err)
		}

		diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
		defer restoreLogs()

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("playback failed: %d %s", created.Code, created.Body)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		publisher.mu.Unlock()
		if starts != 0 {
			t.Fatalf("publisher started with stale HLS probe evidence: starts=%d", starts)
		}

		entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
		if len(entries) != 1 {
			t.Fatalf("terminal diagnostic count = %d, want 1", len(entries))
		}
		var diagnostic youtubePlaybackDiagnosticEvent
		if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
			t.Fatal(err)
		}
		if diagnostic.HLSGate != "ineligible" || diagnostic.HLSGateReason != "fresh_event_probe" {
			t.Fatalf("unexpected diagnostic for stale probe: %+v", diagnostic)
		}
	})
}

func TestYouTubeAutoHLSRejectsIncompatibleNativeMetadata(t *testing.T) {
	t.Run("incompatible video codec vp9", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, false)
		s.deps.RemoteMedia = &qualityTestMedia{width: 1280, height: 720, codec: "vp9", profile: "Profile 0"}

		diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
		defer restoreLogs()

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("playback failed: %d %s", created.Code, created.Body)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		publisher.mu.Unlock()
		if starts != 0 {
			t.Fatalf("publisher started with VP9 codec: starts=%d", starts)
		}

		entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
		if len(entries) != 1 {
			t.Fatalf("terminal diagnostic count = %d, want 1", len(entries))
		}
		var diagnostic youtubePlaybackDiagnosticEvent
		if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
			t.Fatal(err)
		}
		if diagnostic.HLSGate != "ineligible" || diagnostic.HLSGateReason != "video_codec" {
			t.Fatalf("unexpected diagnostic for VP9: %+v", diagnostic)
		}
	})

	t.Run("native 360p Auto uses existing decoder evidence", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, false)
		s.deps.RemoteMedia = &qualityTestMedia{width: 640, height: 360, codec: "h264", profile: "Baseline"}

		diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
		defer restoreLogs()

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("playback failed: %d %s", created.Code, created.Body)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		publisher.mu.Unlock()
		if starts != 1 {
			t.Fatalf("publisher starts = %d, want 1 for native 360p with passing decoder evidence", starts)
		}
		var plan domain.Plan
		if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" {
			t.Fatalf("native 360p Auto should use compatibility HLS: %+v", plan)
		}

		entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
		if len(entries) != 1 {
			t.Fatalf("terminal diagnostic count = %d, want 1", len(entries))
		}
		var diagnostic youtubePlaybackDiagnosticEvent
		if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
			t.Fatal(err)
		}
		if diagnostic.HLSGate != "eligible" || diagnostic.HLSGateReason != "eligible" {
			t.Fatalf("native 360p Auto did not use existing decoder policy: %+v", diagnostic)
		}
	})

	t.Run("incompatible audio codec opus", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, false)
		s.deps.RemoteMedia = &customStreamsRemoteMedia{metadata: domain.Metadata{
			Streams: []domain.Stream{
				{Index: 0, Type: "video", Codec: "h264", Profile: "Main", Width: 1280, Height: 720},
				{Index: 1, Type: "audio", Codec: "opus"},
			},
		}}

		diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
		defer restoreLogs()

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("playback failed: %d %s", created.Code, created.Body)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		publisher.mu.Unlock()
		if starts != 0 {
			t.Fatalf("publisher started with opus audio: starts=%d", starts)
		}

		entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
		if len(entries) != 1 {
			t.Fatalf("terminal diagnostic count = %d, want 1", len(entries))
		}
		var diagnostic youtubePlaybackDiagnosticEvent
		if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
			t.Fatal(err)
		}
		if diagnostic.HLSGate != "ineligible" || diagnostic.HLSGateReason != "audio_codec" {
			t.Fatalf("unexpected diagnostic for opus audio: %+v", diagnostic)
		}
	})

	t.Run("incompatible device decoder capability for 1080p", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, false)
		s.deps.RemoteMedia = &qualityTestMedia{width: 1920, height: 1080, codec: "h264", profile: "High"}
		s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
			request.URL = "https://r1.googlevideo.com/video-1080?signature=upstream-secret"
			request.AudioURL = "https://r2.googlevideo.com/audio?signature=audio-secret"
			request.MIME = "video/mp4"
			request.Item.DurationMS = 60_000
			return request, nil
		}}

		// Mark h264-1080-high as FAIL on device so SelectedQualityMode rejects REMUX for 1080p.
		var dev domain.Device
		if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &dev); err != nil {
			t.Fatal(err)
		}
		for i := range dev.Capabilities.Probes {
			if dev.Capabilities.Probes[i].ID == "h264-1080-high" {
				dev.Capabilities.Probes[i].Status = "FAIL"
			}
		}
		if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
			t.Fatal(err)
		}

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("playback failed: %d %s", created.Code, created.Body)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		publisher.mu.Unlock()
		if starts != 0 {
			t.Fatalf("publisher started when device cannot decode 1080p high profile: starts=%d", starts)
		}
	})
}

func TestYouTubeAutoHLSRejectsExplicitTranscode(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)

	diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
	defer restoreLogs()

	body := fmt.Sprintf(`{"itemId":%q,"mode":"TRANSCODE"}`, source.Item.ID)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("playback failed: %d %s", created.Code, created.Body)
	}

	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 0 {
		t.Fatalf("publisher started for explicit TRANSCODE mode: starts=%d", starts)
	}

	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "TRANSCODE" {
		t.Fatalf("plan mode = %q, want TRANSCODE", plan.Mode)
	}

	entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
	if len(entries) != 1 {
		t.Fatalf("terminal diagnostic count = %d, want 1", len(entries))
	}
	var diagnostic youtubePlaybackDiagnosticEvent
	if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
		t.Fatal(err)
	}
	if diagnostic.DeliveryRoute != "TRANSCODE" || diagnostic.HLSGate != "unreached" {
		t.Fatalf("unexpected diagnostic for explicit TRANSCODE: %+v", diagnostic)
	}
}

func TestYouTubeAutoHLSRejectsNetworkDownscale(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)

	// Inject fresh low-bandwidth sample (400 kbps triggers LOW/STANDARD transcode).
	now := time.Now()
	s.mu.Lock()
	s.networkSamples["youtube-hls-device"] = networkSample{
		id:       "sample-low",
		started:  now.Add(-10 * time.Second),
		measured: now,
		kbps:     400,
	}
	s.mu.Unlock()

	// Ensure device has chunked probe passing so network quality does not bypass conversion.
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &dev); err != nil {
		t.Fatal(err)
	}
	dev.Capabilities.Probes = append(dev.Capabilities.Probes, domain.Probe{
		ID:         "http-fmp4-chunked",
		Status:     "PASS",
		PositionMS: 1000,
		Completed:  true,
		TestedAt:   now.Unix(),
	})
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("playback failed: %d %s", created.Code, created.Body)
	}

	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 0 {
		t.Fatalf("publisher started despite network downscale: starts=%d", starts)
	}

	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "TRANSCODE" {
		t.Fatalf("plan mode = %q, want TRANSCODE", plan.Mode)
	}
}

func TestYouTubeAutoHLSRejectsNonYouTube(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, _ := setupYouTubeHLSServer(t, publisher, true)

	// Add an IPTV source to search results
	iptvSource := domain.Source{
		Item: domain.Item{
			ID:       "iptv-fixture",
			Provider: "iptv",
			Kind:     "video",
			Playable: true,
		},
		URL:  "https://iptv.invalid/stream.m3u8",
		MIME: "application/vnd.apple.mpegurl",
	}
	s.mu.Lock()
	s.searchResults["youtube-hls-device"] = searchResult{
		revision: 1,
		fetched:  time.Now(),
		sources:  []providers.Source{iptvSource},
	}
	s.mu.Unlock()

	body := fmt.Sprintf(`{"itemId":%q}`, iptvSource.Item.ID)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("IPTV playback failed: %d %s", created.Code, created.Body)
	}

	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 0 {
		t.Fatalf("YouTube HLS publisher started for IPTV source: starts=%d", starts)
	}
}

func TestYouTubeStoredQualityReusesExactProbedSplitTierWithoutExtraResolve(t *testing.T) {
	t.Run("matches exact probed 720p tier", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, true)
		countingResolver := &countingYouTubeResolver{inner: s.deps.Resolver}
		s.deps.Resolver = countingResolver

		if err := s.setQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video", "720p"); err != nil {
			t.Fatal(err)
		}

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("create playback status=%d body=%s", created.Code, created.Body)
		}

		if countingResolver.calls != 1 {
			t.Fatalf("resolver called %d times, want exactly 1 (reused probed split tier)", countingResolver.calls)
		}

		publisher.mu.Lock()
		starts := publisher.starts
		selected := publisher.lastSelection
		publisher.mu.Unlock()
		if starts != 1 {
			t.Fatalf("publisher starts = %d, want 1", starts)
		}
		if selected.Quality != "720p" {
			t.Fatalf("publisher selection quality = %q, want 720p", selected.Quality)
		}
	})

	t.Run("different requested tier does not reuse metadata", func(t *testing.T) {
		publisher := &fakeRemoteHLSPublisher{}
		s, token, source := setupYouTubeHLSServer(t, publisher, true)
		countingResolver := &countingYouTubeResolver{inner: s.deps.Resolver}
		s.deps.Resolver = countingResolver

		// Stored preference is 1080p, while initial source was 720p
		if err := s.setQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video", "1080p"); err != nil {
			t.Fatal(err)
		}

		body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
		created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
		if created.Code != http.StatusCreated {
			t.Fatalf("create playback status=%d body=%s", created.Code, created.Body)
		}

		// Because initial source was 720p, resolving 1080p required a 2nd resolve call!
		if countingResolver.calls != 2 {
			t.Fatalf("resolver called %d times, want 2 (extra resolve needed for different tier)", countingResolver.calls)
		}
	})
}

// TestYouTubeStoredQualityRequiresREMUXForExactTierReuse verifies that the
// exact-probed-tier shortcut (no extra resolve) is only allowed when the
// initial native decision mode is REMUX. A stored preference paired with a
// source that requires transcoding must trigger a fresh resolution.
func TestYouTubeStoredQualityRequiresREMUXForExactTierReuse(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, false)
	countingResolver := &countingYouTubeResolver{inner: s.deps.Resolver}
	s.deps.Resolver = countingResolver

	// The source profile is unsupported, so LocalMode needs transcoding even
	// though the 720p decoder tier itself has positive PASS evidence.
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &dev); err != nil {
		t.Fatal(err)
	}
	for i := range dev.Capabilities.Probes {
		if dev.Capabilities.Probes[i].ID == "h264-1080-high" {
			dev.Capabilities.Probes[i].Status = "FAIL"
		}
	}
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}
	// RemoteMedia returns a complete 720p split source with a profile that the
	// generic remux planner does not recognize. The tier remains selectable
	// because h264-720-main still has fresh PASS evidence.
	s.deps.RemoteMedia = &qualityTestMedia{width: 1280, height: 720, codec: "h264", profile: "UnknownUnsupportedProfile"}
	initialMetadata, err := s.deps.RemoteMedia.ProbeRemote(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	caps := devices.CurrentCapabilities(dev)
	if got := playback.LocalMode(initialMetadata, source.MIME, caps, ""); got != "TRANSCODE" {
		t.Fatalf("initial LocalMode = %q, want TRANSCODE for unsupported profile", got)
	}
	if !playback.HasQuality(playback.Qualities(initialMetadata, source, dev, ""), "720p") {
		t.Fatal("stored 720p tier is not selectable despite its passing decoder evidence")
	}
	if got, _ := playback.SelectedQualityMode(initialMetadata, source, dev, "720p", 0, "TRANSCODE"); got != "REMUX" {
		t.Fatalf("SelectedQualityMode for the passing 720p tier = %q, want REMUX", got)
	}

	// Store a 720p preference (which matches the probed tier).
	if err := s.setQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video", "720p"); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create playback status=%d body=%s", created.Code, created.Body)
	}

	// The initial decision mode is TRANSCODE (unsupported profile), so the
	// exact-tier reuse path must NOT be taken; a fresh resolve is required.
	// That means exactly 2 resolver calls: initial + quality re-resolve.
	if countingResolver.calls != 2 {
		t.Fatalf("resolver called %d times, want 2 (stored quality with TRANSCODE must trigger fresh resolve)", countingResolver.calls)
	}
	if got := s.getQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video"); got != "720p" {
		t.Fatalf("stored preference = %q, want 720p to remain selectable", got)
	}
	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 0 {
		t.Fatalf("HLS publisher started for unsupported profile: starts=%d", starts)
	}
}

// TestYouTubeHLS480pExplicitPublishesWithPosition verifies that an explicit
// 480p quality request is admitted by the HLS gate and that the publisher
// receives the correct position offset.
func TestYouTubeHLS480pExplicitPublishesWithPosition(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, false)
	addYouTubeSearchVariant(s, &source, "480p")
	s.deps.RemoteMedia = &qualityTestMedia{width: 854, height: 480, codec: "h264", profile: "Main"}
	s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
		request.URL = "https://r1.googlevideo.com/video-480?signature=upstream-secret"
		request.AudioURL = "https://r2.googlevideo.com/audio?signature=audio-secret"
		request.MIME = "video/mp4"
		request.Item.DurationMS = 60_000
		return request, nil
	}}

	const position = int64(31064)
	body := fmt.Sprintf(`{"itemId":%q,"quality":"480p","positionMs":%d}`, source.Item.ID, position)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create 480p HLS: status=%d body=%s", created.Code, created.Body)
	}

	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" || plan.TimelineOffsetMS != position {
		t.Fatalf("480p HLS plan lost delivery or position: %+v", plan)
	}

	publisher.mu.Lock()
	starts := publisher.starts
	selected := publisher.lastSelection
	usedSource := publisher.lastSource
	publisher.mu.Unlock()
	if starts != 1 {
		t.Fatalf("publisher starts = %d, want 1", starts)
	}
	if selected.Quality != "480p" || selected.PositionMS != position {
		t.Fatalf("publisher selection = %+v, want quality=480p positionMs=%d", selected, position)
	}
	if usedSource.AudioURL == "" {
		t.Fatal("480p publisher source lost its separate audio URL")
	}
}

func TestYouTubeHLSQualitySwitchTo480pPreservesOffsetWithoutSeekEvidence(t *testing.T) {
	const positionMS = int64(31064)
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)
	addYouTubeSearchVariant(s, &source, "480p")
	s.deps.RemoteMedia = &youtubeHLSQualityMedia{}
	s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
		request.URL = "https://r1.googlevideo.com/video?signature=upstream-secret"
		if request.ResolveQuality == "480p" {
			request.URL = "https://r1.googlevideo.com/video-480?signature=upstream-secret"
		}
		request.AudioURL = "https://r2.googlevideo.com/audio?signature=audio-secret"
		request.MIME = "video/mp4"
		request.Item.DurationMS = 60_000
		return request, nil
	}}

	// The device has known-length fMP4 PASS and a current chunked FAIL, but no
	// seek PASS. HLS must preserve the replacement offset without a seek claim.
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &device); err != nil {
		t.Fatal(err)
	}
	device.Capabilities.Probes = append(device.Capabilities.Probes, domain.Probe{
		ID:       "http-fmp4-chunked",
		Status:   "FAIL",
		TestedAt: time.Now().Unix(),
	})
	if err := s.db.Put(t.Context(), "devices", device.ID, device); err != nil {
		t.Fatal(err)
	}
	caps := devices.CurrentCapabilities(device)
	if status, fresh := freshProbeOutcome(caps, "http-fmp4", time.Now()); !fresh || status != "PASS" {
		t.Fatalf("fixture lacks fresh known-length fMP4 PASS: status=%q fresh=%t", status, fresh)
	}
	if status, fresh := freshProbeOutcome(caps, "http-fmp4-chunked", time.Now()); !fresh || status != "FAIL" {
		t.Fatalf("fixture lacks fresh chunked fMP4 FAIL: status=%q fresh=%t", status, fresh)
	}
	if status, fresh := freshProbeOutcome(caps, "http-fmp4-seek", time.Now()); fresh && status == "PASS" {
		t.Fatal("fixture unexpectedly includes seek PASS evidence")
	}

	created := call(s, http.MethodPost, "/v1/playback",
		fmt.Sprintf(`{"itemId":%q,"quality":"720p"}`, source.Item.ID),
		"youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create initial 720p playback: status=%d body=%s", created.Code, created.Body)
	}
	var firstPlan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &firstPlan); err != nil {
		t.Fatal(err)
	}

	switched := call(s, http.MethodPost, "/v1/playback/"+firstPlan.SessionID+"/quality",
		fmt.Sprintf(`{"qualityId":"480p","positionMs":%d}`, positionMS),
		"youtube-hls-device", token, "")
	if switched.Code != http.StatusCreated {
		t.Fatalf("switch to 480p: status=%d body=%s", switched.Code, switched.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(switched.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" || plan.ResumeMS != 0 || plan.TimelineOffsetMS != positionMS {
		t.Fatalf("480p replacement lost HLS offset semantics: %+v", plan)
	}

	publisher.mu.Lock()
	starts := publisher.starts
	selected := publisher.lastSelection
	usedSource := publisher.lastSource
	publisher.mu.Unlock()
	if starts != 2 || selected.Quality != "480p" || selected.PositionMS != positionMS {
		t.Fatalf("publisher replacement = starts:%d selection:%+v, want 2 starts and 480p at %dms", starts, selected, positionMS)
	}
	if usedSource.AudioURL == "" {
		t.Fatal("480p quality-switch publisher source lost its separate audio URL")
	}
	if got := s.getQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video"); got != "480p" {
		t.Fatalf("quality preference = %q, want 480p", got)
	}
}

// TestYouTubeAutoHLS480pEligible verifies that an Auto request where the
// native source is 480p h264/aac is admitted through the HLS gate.
func TestYouTubeAutoHLS480pEligible(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, false)
	addYouTubeSearchVariant(s, &source, "480p")
	s.deps.RemoteMedia = &qualityTestMedia{width: 854, height: 480, codec: "h264", profile: "Main"}
	s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
		request.URL = "https://r1.googlevideo.com/video-480?signature=upstream-secret"
		request.AudioURL = "https://r2.googlevideo.com/audio?signature=audio-secret"
		request.MIME = "video/mp4"
		request.Item.DurationMS = 60_000
		return request, nil
	}}

	diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
	defer restoreLogs()

	body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("Auto 480p playback failed: %d %s", created.Code, created.Body)
	}

	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" {
		t.Fatalf("Auto 480p should use HLS: %+v", plan)
	}

	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 1 {
		t.Fatalf("publisher starts = %d, want 1 for Auto 480p", starts)
	}

	entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
	if len(entries) != 1 {
		t.Fatalf("terminal diagnostic count = %d, want 1; logs: %s", len(entries), diagnosticLogs.String())
	}
	var diagnostic youtubePlaybackDiagnosticEvent
	if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
		t.Fatal(err)
	}
	if diagnostic.HLSGate != "eligible" || diagnostic.DeliveryRoute != "youtube_hls" {
		t.Fatalf("Auto 480p HLS gate not eligible: %+v", diagnostic)
	}
}

// TestYouTubeHLS480pRejectsIncompatibleDecoderEvidence verifies that 480p is
// refused when the device does not have passing decoder evidence for any h264
// tier that covers 480p.
func TestYouTubeHLS480pRejectsIncompatibleDecoderEvidence(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, false)
	addYouTubeSearchVariant(s, &source, "480p")
	s.deps.RemoteMedia = &qualityTestMedia{width: 854, height: 480, codec: "h264", profile: "Main"}
	s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
		request.URL = "https://r1.googlevideo.com/video-480?signature=upstream-secret"
		request.AudioURL = "https://r2.googlevideo.com/audio?signature=audio-secret"
		request.MIME = "video/mp4"
		request.Item.DurationMS = 60_000
		return request, nil
	}}

	// Mark all h264 decoder probes as FAIL so SelectedQualityMode returns TRANSCODE/EXTERNAL_PLAYER.
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &dev); err != nil {
		t.Fatal(err)
	}
	for i := range dev.Capabilities.Probes {
		switch dev.Capabilities.Probes[i].ID {
		case "h264-720-main", "h264-1080-high":
			dev.Capabilities.Probes[i].Status = "FAIL"
		}
	}
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"itemId":%q,"quality":"480p"}`, source.Item.ID)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusConflict || !strings.Contains(created.Body.String(), "quality_unavailable") {
		t.Fatalf("missing 480p decoder should refuse the unavailable quality: status=%d body=%s", created.Code, created.Body)
	}

	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 0 {
		t.Fatalf("publisher started when device cannot decode 480p: starts=%d", starts)
	}

}

// TestYouTubeFreshChunkedPassPrefersStreamingREMUX verifies the exact legacy
// REMUX route for Auto and manual 480p/720p requests when fresh chunked evidence
// exists, even though each source also satisfies the HLS gate.
func TestYouTubeFreshChunkedPassPrefersStreamingREMUX(t *testing.T) {
	for _, test := range []struct {
		name    string
		quality string
	}{
		{name: "Auto", quality: "auto"},
		{name: "manual 480p", quality: "480p"},
		{name: "manual 720p", quality: "720p"},
	} {
		t.Run(test.name, func(t *testing.T) {
			publisher := &fakeRemoteHLSPublisher{}
			s, token, source := setupYouTubeHLSServer(t, publisher, test.quality != "480p")
			if test.quality == "480p" {
				addYouTubeSearchVariant(s, &source, "480p")
				s.deps.RemoteMedia = &qualityTestMedia{width: 854, height: 480, codec: "h264", profile: "Main"}
				s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
					request.URL = "https://r1.googlevideo.com/video-480?signature=upstream-secret"
					request.AudioURL = "https://r2.googlevideo.com/audio?signature=audio-secret"
					request.MIME = "video/mp4"
					request.Item.DurationMS = 60_000
					return request, nil
				}}
			}

			now := time.Now()
			var dev domain.Device
			if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &dev); err != nil {
				t.Fatal(err)
			}
			dev.Capabilities.Probes = append(dev.Capabilities.Probes, domain.Probe{
				ID:         "http-fmp4-chunked",
				Status:     "PASS",
				PositionMS: 1000,
				Completed:  true,
				TestedAt:   now.Unix(),
			})
			if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
				t.Fatal(err)
			}

			body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
			if test.quality != "auto" {
				body = fmt.Sprintf(`{"itemId":%q,"quality":%q}`, source.Item.ID, test.quality)
			}
			created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
			if created.Code != http.StatusCreated {
				t.Fatalf("playback failed: %d %s", created.Code, created.Body)
			}

			publisher.mu.Lock()
			starts := publisher.starts
			publisher.mu.Unlock()
			if starts != 0 {
				t.Fatalf("publisher started despite fresh chunked PASS: starts=%d", starts)
			}

			var plan domain.Plan
			if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			if plan.Mode != "REMUX" || plan.MIME == "application/vnd.apple.mpegurl" {
				t.Fatalf("fresh chunked evidence must select exact streaming REMUX: %+v", plan)
			}
		})
	}
}

// TestYouTubeStaleFailedOrMissingChunkedEvidenceUsesEligibleHLS verifies that
// only a fresh passing chunked probe suppresses HLS at position zero.
func TestYouTubeStaleFailedOrMissingChunkedEvidenceUsesEligibleHLS(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    string
		testedAgo time.Duration
	}{
		{name: "missing"},
		{name: "stale", status: "PASS", testedAgo: 10 * 24 * time.Hour},
		{name: "failed", status: "FAIL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			publisher := &fakeRemoteHLSPublisher{}
			s, token, source := setupYouTubeHLSServer(t, publisher, true)
			if test.status != "" {
				var dev domain.Device
				if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &dev); err != nil {
					t.Fatal(err)
				}
				dev.Capabilities.Probes = append(dev.Capabilities.Probes, domain.Probe{
					ID:         "http-fmp4-chunked",
					Status:     test.status,
					PositionMS: 1000,
					Completed:  true,
					TestedAt:   time.Now().Add(-test.testedAgo).Unix(),
				})
				if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
					t.Fatal(err)
				}
			}

			body := fmt.Sprintf(`{"itemId":%q}`, source.Item.ID)
			created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
			if created.Code != http.StatusCreated {
				t.Fatalf("playback failed: %d %s", created.Code, created.Body)
			}
			publisher.mu.Lock()
			starts := publisher.starts
			publisher.mu.Unlock()
			if starts != 1 {
				t.Fatalf("publisher starts = %d, want 1 for %s chunked evidence", starts, test.name)
			}
			var plan domain.Plan
			if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" || plan.ResumeMS != 0 || plan.TimelineOffsetMS != 0 {
				t.Fatalf("expected HLS with no resume offset for %s chunked evidence: %+v", test.name, plan)
			}
		})
	}
}

// TestYouTubeNonzeroPositionHLSPreservedOnKnownLengthOnlyDevice verifies that
// a nonzero resume position is preserved as a HLS timeline offset when the
// device only has known-length fMP4 evidence (no seek PASS) and HLS is
// eligible. The old bug zeroed position before HLS selection.
func TestYouTubeNonzeroPositionHLSPreservedOnKnownLengthOnlyDevice(t *testing.T) {
	const resumePosition = int64(31064)

	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)

	now := time.Now()
	var dev domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &dev); err != nil {
		t.Fatal(err)
	}
	// Add http-fmp4-chunked FAIL: this makes requiresKnownLengthYouTubeRemux
	// return true, which is the "known-length-only" condition that used to zero
	// positionMS before HLS could be selected.
	dev.Capabilities.Probes = append(dev.Capabilities.Probes, domain.Probe{
		ID:       "http-fmp4-chunked",
		Status:   "FAIL",
		TestedAt: now.Unix(),
	})
	if err := s.db.Put(t.Context(), "devices", dev.ID, dev); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"itemId":%q,"positionMs":%d}`, source.Item.ID, resumePosition)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("playback failed: %d %s", created.Code, created.Body)
	}

	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	// HLS is eligible for 720p h264/aac and was selected; position must be preserved.
	if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" {
		t.Fatalf("expected HLS for known-length-only device at nonzero position: %+v", plan)
	}
	if plan.TimelineOffsetMS != resumePosition {
		t.Fatalf("position lost: TimelineOffsetMS = %d, want %d; plan = %+v", plan.TimelineOffsetMS, resumePosition, plan)
	}

	publisher.mu.Lock()
	starts := publisher.starts
	selected := publisher.lastSelection
	publisher.mu.Unlock()
	if starts != 1 {
		t.Fatalf("publisher starts = %d, want 1", starts)
	}
	if selected.PositionMS != resumePosition {
		t.Fatalf("publisher selection position = %d, want %d", selected.PositionMS, resumePosition)
	}
}

// TestYouTubeQualitySwitchToAutoAtNonzeroPositionUsesEligibleHLS verifies
// that switching quality to Auto at a nonzero position selects HLS when the
// inferred native tier is eligible, keeping the Auto preference and offset.
func TestYouTubeQualitySwitchToAutoAtNonzeroPositionUsesEligibleHLS(t *testing.T) {
	const positionMS = int64(31064)

	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)
	s.deps.RemoteMedia = &youtubeHLSQualityMedia{}

	// Create an initial manual 720p HLS session.
	created := call(s, http.MethodPost, "/v1/playback",
		fmt.Sprintf(`{"itemId":%q,"quality":"720p"}`, source.Item.ID),
		"youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create 720p HLS: status=%d body=%s", created.Code, created.Body)
	}
	var firstPlan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &firstPlan); err != nil {
		t.Fatal(err)
	}

	// Switch to Auto at a nonzero position.
	switched := call(s, http.MethodPost,
		"/v1/playback/"+firstPlan.SessionID+"/quality",
		fmt.Sprintf(`{"qualityId":"auto","positionMs":%d}`, positionMS),
		"youtube-hls-device", token, "")
	if switched.Code != http.StatusCreated {
		t.Fatalf("switch to auto: status=%d body=%s", switched.Code, switched.Body)
	}
	var secondPlan domain.Plan
	if err := json.Unmarshal(switched.Body.Bytes(), &secondPlan); err != nil {
		t.Fatal(err)
	}

	// Auto switch at nonzero position should select HLS (inferred 720p tier is eligible)
	// and preserve the timeline offset.
	if secondPlan.Mode != "DIRECT_PLAY" || secondPlan.MIME != "application/vnd.apple.mpegurl" {
		t.Fatalf("Auto quality switch at nonzero position should use eligible HLS: %+v", secondPlan)
	}
	if secondPlan.TimelineOffsetMS != positionMS {
		t.Fatalf("Auto quality switch lost position: TimelineOffsetMS = %d, want %d", secondPlan.TimelineOffsetMS, positionMS)
	}

	// Verify Auto preference is preserved (not a manual tier).
	if got := s.getQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video"); got != "auto" && got != "" {
		t.Fatalf("quality preference = %q after Auto switch, want auto or empty", got)
	}

	s.mu.Lock()
	newSession := s.sessions[secondPlan.SessionID]
	s.mu.Unlock()
	if newSession == nil || newSession.youtubeHLSPublisher == nil {
		t.Fatalf("Auto-switch HLS session not created or has no publisher")
	}
	if newSession.selection.PositionMS != positionMS {
		t.Fatalf("Auto-switch session position = %d, want %d", newSession.selection.PositionMS, positionMS)
	}
}

func TestYouTubeAuto804HLSPreservesNativeSourceAndResume(t *testing.T) {
	const resumePosition = int64(31064)

	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTube804HLSServer(t, publisher)
	if err := s.setQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video", "auto"); err != nil {
		t.Fatal(err)
	}

	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &device); err != nil {
		t.Fatal(err)
	}
	caps := devices.CurrentCapabilities(device)
	for _, probeID := range []string{"hls-event-h264-aac", "http-fmp4", "h264-1080-high", "aac"} {
		status, fresh := freshProbeOutcome(caps, probeID, time.Now())
		if !fresh || status != "PASS" {
			t.Fatalf("fixture probe %q = %q fresh=%t, want fresh PASS", probeID, status, fresh)
		}
	}
	if status, fresh := freshProbeOutcome(caps, "http-fmp4-chunked", time.Now()); fresh || status != "" {
		t.Fatalf("fixture chunked probe = %q fresh=%t, want UNKNOWN", status, fresh)
	}

	diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
	defer restoreLogs()
	body := fmt.Sprintf(`{"itemId":%q,"quality":"auto","positionMs":%d}`, source.Item.ID, resumePosition)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("Auto 804p playback failed: %d %s", created.Code, created.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" || plan.ResumeMS != 0 || plan.TimelineOffsetMS != resumePosition {
		t.Fatalf("Auto 804p plan lost native HLS resume semantics: %+v", plan)
	}

	publisher.mu.Lock()
	starts := publisher.starts
	usedSource := publisher.lastSource
	selection := publisher.lastSelection
	publisher.mu.Unlock()
	if starts != 1 || selection.PositionMS != resumePosition {
		t.Fatalf("publisher starts=%d selection=%+v, want one HLS publication at %dms", starts, selection, resumePosition)
	}
	if usedSource.URL != youtube804VideoURL || usedSource.AudioURL != youtube804AudioURL {
		t.Fatalf("publisher did not receive the original split source: video=%q audio=%q", usedSource.URL, usedSource.AudioURL)
	}

	s.mu.Lock()
	sess := s.sessions[plan.SessionID]
	s.mu.Unlock()
	if sess == nil || sess.metadata == nil || len(sess.metadata.Streams) < 1 {
		t.Fatal("created Auto session lost its probed source metadata")
	}
	var actualVideo *domain.Stream
	for index := range sess.metadata.Streams {
		if sess.metadata.Streams[index].Type == "video" {
			actualVideo = &sess.metadata.Streams[index]
			break
		}
	}
	if actualVideo == nil || actualVideo.Width != 1920 || actualVideo.Height != 804 {
		t.Fatalf("session metadata dimensions were relabeled: %+v", sess.metadata.Streams)
	}

	entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
	if len(entries) != 1 {
		t.Fatalf("terminal diagnostic count = %d, want 1; logs: %s", len(entries), diagnosticLogs.String())
	}
	var diagnostic youtubePlaybackDiagnosticEvent
	if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
		t.Fatal(err)
	}
	if diagnostic.RequestedQuality != "auto" || diagnostic.ChosenQuality != "auto" || diagnostic.DeliveryRoute != "youtube_hls" ||
		diagnostic.ActualVideoWidth != 1920 || diagnostic.ActualVideoHeight != 804 || diagnostic.HLSGate != "eligible" {
		t.Fatalf("unexpected 804p Auto diagnostic: %+v", diagnostic)
	}
	if got := s.getQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video"); got != "auto" {
		t.Fatalf("Auto preference changed to %q", got)
	}
	for _, private := range []string{"googlevideo.com", "signature=upstream-secret", "signature=audio-secret"} {
		if strings.Contains(diagnosticLogs.String(), private) {
			t.Fatalf("diagnostic leaked %q: %s", private, diagnosticLogs.String())
		}
	}
}

func TestYouTubeAuto804FreshChunkedAtPositionZeroKeepsREMUX(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTube804HLSServer(t, publisher)
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &device); err != nil {
		t.Fatal(err)
	}
	device.Capabilities.Probes = append(device.Capabilities.Probes, domain.Probe{
		ID: "http-fmp4-chunked", Status: "PASS", PositionMS: 1000, Completed: true, TestedAt: time.Now().Unix(),
	})
	if err := s.db.Put(t.Context(), "devices", device.ID, device); err != nil {
		t.Fatal(err)
	}

	created := call(s, http.MethodPost, "/v1/playback",
		fmt.Sprintf(`{"itemId":%q,"quality":"auto"}`, source.Item.ID),
		"youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("Auto 804p playback failed: %d %s", created.Code, created.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "REMUX" || plan.MIME != "video/mp4" {
		t.Fatalf("fresh chunked PASS should keep native 804p REMUX: %+v", plan)
	}
	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 0 {
		t.Fatalf("publisher starts = %d, want no HLS publication with fresh chunked PASS", starts)
	}
}

func TestYouTubeManualHLSTiersMatchNominal804ClassForAcceptedPanoramicSource(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, _, source := setupYouTube804HLSServer(t, publisher)
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &device); err != nil {
		t.Fatal(err)
	}
	metadata := domain.Metadata{Streams: []domain.Stream{
		{Index: 0, Type: "video", Codec: "h264", Profile: "High", Width: 1920, Height: 804},
		{Index: 1, Type: "audio", Codec: "aac"},
	}}
	if eligible, reason := s.youtubeHLSIneligibility(device, source, &metadata, "auto"); !eligible || reason != "eligible" {
		t.Fatalf("native Auto gate = %t, %q; want eligible", eligible, reason)
	}
	for _, quality := range []string{"1080p", "720p"} {
		t.Run(quality, func(t *testing.T) {
			eligible, reason := s.youtubeHLSIneligibility(device, source, &metadata, quality)
			wantEligible := quality == "1080p"
			wantReason := "eligible"
			if !wantEligible {
				wantReason = "video_height"
			}
			if eligible != wantEligible || reason != wantReason {
				t.Fatalf("manual %s gate = %t, %q; want %t, %q", quality, eligible, reason, wantEligible, wantReason)
			}
		})
	}
}

func TestYouTubeAutoHLSRejectsMissingZeroOrOutOfBoundsDimensions(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, _, source := setupYouTube804HLSServer(t, publisher)
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &device); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		width  int
		height int
	}{
		{name: "missing width", width: 0, height: 804},
		{name: "missing height", width: 1920, height: 0},
		{name: "width above 1920", width: 1921, height: 804},
		{name: "height above 1080", width: 1920, height: 1081},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := domain.Metadata{Streams: []domain.Stream{
				{Index: 0, Type: "video", Codec: "h264", Profile: "High", Width: test.width, Height: test.height},
				{Index: 1, Type: "audio", Codec: "aac"},
			}}
			eligible, reason := s.youtubeHLSIneligibility(device, source, &metadata, "auto")
			if eligible || reason != "video_dimensions_ineligible" {
				t.Fatalf("Auto HLS gate = %t, %q; want false, video_dimensions_ineligible", eligible, reason)
			}
		})
	}
}

func TestYouTubeManual720Rejects804PanosAnd1080AcceptsWhenValid(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTube804HLSServer(t, publisher)
	diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
	defer restoreLogs()

	created := call(s, http.MethodPost, "/v1/playback",
		fmt.Sprintf(`{"itemId":%q,"quality":"720p"}`, source.Item.ID),
		"youtube-hls-device", token, "")
	if created.Code != http.StatusBadGateway || !strings.Contains(created.Body.String(), "quality_resolution_failed") {
		t.Fatalf("manual 720p should reject the 804p panorama: status=%d body=%s", created.Code, created.Body)
	}
	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 0 {
		t.Fatalf("publisher started for mismatched manual 720p: starts=%d", starts)
	}
	entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
	if len(entries) != 1 {
		t.Fatalf("terminal diagnostic count = %d, want 1; logs: %s", len(entries), diagnosticLogs.String())
	}
	var diagnostic youtubePlaybackDiagnosticEvent
	if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
		t.Fatal(err)
	}
	if diagnostic.RequestedQuality != "720p" || diagnostic.ChosenQuality != "unavailable" || diagnostic.DeliveryRoute != "unselected" {
		t.Fatalf("manual 720p was relabeled after mismatch: %+v", diagnostic)
	}

	publisher = &fakeRemoteHLSPublisher{}
	s, token, source = setupYouTube804HLSServer(t, publisher)
	diagnosticLogs, restoreLogs = captureYouTubePlaybackLog(t)
	defer restoreLogs()
	created = call(s, http.MethodPost, "/v1/playback",
		fmt.Sprintf(`{"itemId":%q,"quality":"1080p"}`, source.Item.ID),
		"youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("manual 1080p should accept valid 804p panorama under nominal 1080 class: status=%d body=%s", created.Code, created.Body)
	}
	publisher.mu.Lock()
	starts = publisher.starts
	publisher.mu.Unlock()
	if starts != 1 {
		t.Fatalf("publisher starts = %d, want 1 for valid 1080p panorama", starts)
	}
}

func TestYouTubeManualToAutoSwitchUses804NativeHLS(t *testing.T) {
	const positionMS = int64(31064)

	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, false)
	s.deps.RemoteMedia = &youtubeHLSQualityMedia{}
	s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
		request.MIME = "video/mp4"
		request.Item.DurationMS = 60_000
		if request.ResolveQuality == "720p" {
			request.URL = "https://r1.googlevideo.com/video-720?signature=upstream-secret"
			request.AudioURL = "https://r2.googlevideo.com/audio-720?signature=audio-secret"
		} else {
			request.URL = youtube804VideoURL
			request.AudioURL = youtube804AudioURL
		}
		return request, nil
	}}

	created := call(s, http.MethodPost, "/v1/playback",
		fmt.Sprintf(`{"itemId":%q,"quality":"720p"}`, source.Item.ID),
		"youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create initial manual 720p session: status=%d body=%s", created.Code, created.Body)
	}
	var firstPlan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &firstPlan); err != nil {
		t.Fatal(err)
	}

	switched := call(s, http.MethodPost, "/v1/playback/"+firstPlan.SessionID+"/quality",
		fmt.Sprintf(`{"qualityId":"auto","positionMs":%d}`, positionMS),
		"youtube-hls-device", token, "")
	if switched.Code != http.StatusCreated {
		t.Fatalf("switch 720p to Auto: status=%d body=%s", switched.Code, switched.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(switched.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" || plan.ResumeMS != 0 || plan.TimelineOffsetMS != positionMS {
		t.Fatalf("manual-to-Auto 804p switch lost HLS offset semantics: %+v", plan)
	}
	publisher.mu.Lock()
	starts := publisher.starts
	usedSource := publisher.lastSource
	selection := publisher.lastSelection
	publisher.mu.Unlock()
	if starts != 2 || selection.PositionMS != positionMS {
		t.Fatalf("publisher starts=%d selection=%+v, want replacement at %dms", starts, selection, positionMS)
	}
	if usedSource.URL != youtube804VideoURL || usedSource.AudioURL != youtube804AudioURL {
		t.Fatalf("Auto replacement did not preserve native split source: %+v", usedSource)
	}
	if got := s.getQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video"); got != "auto" {
		t.Fatalf("quality preference = %q after Auto switch, want auto", got)
	}
}

func TestYouTubeManual1080KnownLength804PanoramaKeepsREMUXAtZeroPosition(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTube804HLSServer(t, publisher)
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &device); err != nil {
		t.Fatal(err)
	}
	foundChunked := false
	for index := range device.Capabilities.Probes {
		if device.Capabilities.Probes[index].ID == "hls-event-h264-aac" {
			device.Capabilities.Probes[index].Status = "UNKNOWN"
		}
		if device.Capabilities.Probes[index].ID == "http-fmp4-chunked" {
			device.Capabilities.Probes[index].Status = "FAIL"
			device.Capabilities.Probes[index].Completed = true
			device.Capabilities.Probes[index].PositionMS = 1000
			device.Capabilities.Probes[index].Detail = "what=0,extra=0@prepare http=200,video/mp4"
			device.Capabilities.Probes[index].TestedAt = time.Now().Unix()
			foundChunked = true
		}
	}
	// The base fixture never records an http-fmp4-chunked probe, so append
	// the fresh FAIL evidence this test requires for the known-length REMUX
	// fallback instead of relying on a mutation that would otherwise be a
	// no-op.
	if !foundChunked {
		device.Capabilities.Probes = append(device.Capabilities.Probes, domain.Probe{
			ID:         "http-fmp4-chunked",
			Status:     "FAIL",
			Completed:  true,
			PositionMS: 1000,
			Detail:     "what=0,extra=0@prepare http=200,video/mp4",
			TestedAt:   time.Now().Unix(),
		})
	}
	if err := s.db.Put(t.Context(), "devices", device.ID, device); err != nil {
		t.Fatal(err)
	}

	created := call(s, http.MethodPost, "/v1/playback",
		fmt.Sprintf(`{"itemId":%q,"quality":"1080p","positionMs":31064}`, source.Item.ID),
		"youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("manual 1080p 804p panorama failed at nonzero position: status=%d body=%s", created.Code, created.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "REMUX" || plan.MIME != "video/mp4" || plan.ResumeMS != 0 || plan.TimelineOffsetMS != 0 {
		t.Fatalf("known-length 804p fallback did not stay REMUX at zero position: %+v", plan)
	}
	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 0 {
		t.Fatalf("publisher starts = %d, want 0 for known-length REMUX fallback", starts)
	}
}

func TestYouTubeKnownLengthFallbackRequiresFreshTransportDecoderAndAACEvidence(t *testing.T) {
	cases := []struct {
		name           string
		mutate         func(domain.Device) domain.Device
		wantStatus     int
		wantBodyString string
	}{
		{
			name: "missing http-fmp4 pass",
			mutate: func(device domain.Device) domain.Device {
				for index := range device.Capabilities.Probes {
					if device.Capabilities.Probes[index].ID == "http-fmp4" {
						device.Capabilities.Probes[index].Status = "FAIL"
						device.Capabilities.Probes[index].Completed = true
						device.Capabilities.Probes[index].PositionMS = 1000
						device.Capabilities.Probes[index].TestedAt = time.Now().Unix()
					}
				}
				return device
			},
			wantStatus:     http.StatusBadGateway,
			wantBodyString: "media_probe_failed",
		},
		{
			name: "missing h264 decoder evidence",
			mutate: func(device domain.Device) domain.Device {
				for index := range device.Capabilities.Probes {
					if device.Capabilities.Probes[index].ID == "h264-1080-high" {
						device.Capabilities.Probes[index].Status = "FAIL"
						device.Capabilities.Probes[index].TestedAt = time.Now().Unix()
					}
				}
				return device
			},
			wantStatus:     http.StatusConflict,
			wantBodyString: "quality_unavailable",
		},
		{
			name: "missing aac evidence",
			mutate: func(device domain.Device) domain.Device {
				for index := range device.Capabilities.Probes {
					if device.Capabilities.Probes[index].ID == "aac" {
						device.Capabilities.Probes[index].Status = "FAIL"
						device.Capabilities.Probes[index].TestedAt = time.Now().Unix()
					}
				}
				return device
			},
			wantStatus:     http.StatusBadGateway,
			wantBodyString: "media_probe_failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			publisher := &fakeRemoteHLSPublisher{}
			s, token, source := setupYouTube804HLSServer(t, publisher)

			var device domain.Device
			if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &device); err != nil {
				t.Fatal(err)
			}
			device = tc.mutate(device)
			foundChunked := false
			for index := range device.Capabilities.Probes {
				if device.Capabilities.Probes[index].ID == "hls-event-h264-aac" {
					device.Capabilities.Probes[index].Status = "UNKNOWN"
				}
				if device.Capabilities.Probes[index].ID == "http-fmp4-chunked" {
					device.Capabilities.Probes[index].Status = "FAIL"
					device.Capabilities.Probes[index].Completed = true
					device.Capabilities.Probes[index].PositionMS = 1000
					device.Capabilities.Probes[index].Detail = "what=0,extra=0@prepare http=200,video/mp4"
					device.Capabilities.Probes[index].TestedAt = time.Now().Unix()
					foundChunked = true
				}
			}
			if !foundChunked {
				device.Capabilities.Probes = append(device.Capabilities.Probes, domain.Probe{
					ID:         "http-fmp4-chunked",
					Status:     "FAIL",
					Completed:  true,
					PositionMS: 1000,
					Detail:     "what=0,extra=0@prepare http=200,video/mp4",
					TestedAt:   time.Now().Unix(),
				})
			}
			if err := s.db.Put(t.Context(), "devices", device.ID, device); err != nil {
				t.Fatal(err)
			}

			created := call(s, http.MethodPost, "/v1/playback",
				fmt.Sprintf(`{"itemId":%q,"quality":"1080p","positionMs":31064}`, source.Item.ID),
				"youtube-hls-device", token, "")
			if created.Code != tc.wantStatus || !strings.Contains(created.Body.String(), tc.wantBodyString) {
				t.Fatalf("manual 1080p known-length evidence guard=%s: status=%d body=%s; want %d %s", tc.name, created.Code, created.Body, tc.wantStatus, tc.wantBodyString)
			}
		})
	}
}
