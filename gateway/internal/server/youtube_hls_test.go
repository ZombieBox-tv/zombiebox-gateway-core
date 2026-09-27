package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/providers"
)

type fakeRemoteHLSPublisher struct {
	mu            sync.Mutex
	startErr      error
	starts        int
	lastSource    domain.Source
	lastSelection domain.MediaSelection
	last          *fakeRemoteHLSPublication
}

func (p *fakeRemoteHLSPublisher) StartRemoteHLSPublisher(_ context.Context, source domain.Source, selection domain.MediaSelection, root string) (RemoteHLSPublication, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
	p.lastSource = source
	p.lastSelection = selection
	if p.startErr != nil {
		return nil, p.startErr
	}
	if root == "" {
		return nil, errors.New("expected a private publication directory")
	}
	directory, err := os.MkdirTemp(root, "youtube-hls-test-")
	if err != nil {
		return nil, err
	}
	segment := []byte(strings.Repeat("ts-packet-data-", 30))
	name := "segment-00000.ts"
	if err := os.WriteFile(filepath.Join(directory, name), segment, 0600); err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	publication := &fakeRemoteHLSPublication{
		directory: directory,
		segment:   name,
		bytes:     int64(len(segment)),
		playlist:  []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXTINF:2.000,\n" + name + "\n"),
		closed:    make(chan struct{}),
	}
	p.last = publication
	return publication, nil
}

type fakeRemoteHLSPublication struct {
	mu        sync.Mutex
	directory string
	segment   string
	bytes     int64
	playlist  []byte
	state     string
	errText   string
	closed    chan struct{}
	closeOnce sync.Once
}

type youtubeHLSQualityMedia struct {
	qualityTestMedia
}

func (m *youtubeHLSQualityMedia) ProbeRemote(_ context.Context, source domain.Source) (domain.Metadata, error) {
	height, width, profile := 720, 1280, "Main"
	if strings.Contains(source.URL, "video-1080") {
		height, width, profile = 1080, 1920, "High"
	}
	metadata := domain.Metadata{
		Streams: []domain.Stream{
			{Index: 0, Type: "video", Codec: "h264", Profile: profile, Width: width, Height: height},
			{Index: 1, Type: "audio", Codec: "aac"},
		},
	}
	metadata.Format.Name = "mp4"
	return metadata, nil
}

func (p *fakeRemoteHLSPublication) Snapshot() (RemoteHLSPublicationSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.closed:
		return RemoteHLSPublicationSnapshot{State: "closed"}, nil
	default:
	}
	return RemoteHLSPublicationSnapshot{
		State:    p.stateOrRunning(),
		Playlist: append([]byte(nil), p.playlist...),
		Segments: []RemoteHLSPublishedSegment{{Name: p.segment, Bytes: p.bytes}},
		Error:    p.errText,
	}, nil
}

func (p *fakeRemoteHLSPublication) stateOrRunning() string {
	if p.state == "" {
		return "running"
	}
	return p.state
}

func (p *fakeRemoteHLSPublication) OpenSegment(name string) (ReadSeekCloser, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if name != p.segment {
		return nil, 0, os.ErrNotExist
	}
	file, err := os.Open(filepath.Join(p.directory, name))
	if err != nil {
		return nil, 0, err
	}
	return file, p.bytes, nil
}

func (p *fakeRemoteHLSPublication) Close() {
	p.closeOnce.Do(func() {
		_ = os.RemoveAll(p.directory)
		close(p.closed)
	})
}

func setupYouTubeHLSServer(t *testing.T, publisher *fakeRemoteHLSPublisher, qualityTest bool) (*Server, string, domain.Source) {
	t.Helper()
	s := testServer(t, nil, t.TempDir())
	s.opt.YouTubeHLSPublishDir = t.TempDir()
	s.deps.RemoteHLSPublisher = publisher
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{
		"youtube": {Enabled: true, URL: "https://wrapper.invalid", Token: strings.Repeat("s", 32)},
	}); err != nil {
		t.Fatal(err)
	}
	const deviceID = "youtube-hls-device"
	token := pair(t, s, deviceID)
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", deviceID, &device); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	device.Capabilities = domain.Capabilities{
		Version:      1,
		DeviceID:     device.ID,
		SuiteVersion: devices.ProbeSuiteVersion,
		CacheKey:     devices.ProbeCacheKey(device),
		Probes: []domain.Probe{
			{ID: "hls-event-h264-aac", Status: "PASS", Completed: true, PositionMS: 25_000, TestedAt: now.Unix()},
			{ID: "http-fmp4", Status: "PASS", Completed: true, PositionMS: 1000, TestedAt: now.Unix()},
			{ID: "h264-720-main", Status: "PASS", Completed: true, PositionMS: 1000, TestedAt: now.Unix()},
			{ID: "h264-1080-high", Status: "PASS", Completed: true, PositionMS: 1000, TestedAt: now.Unix()},
			{ID: "aac", Status: "PASS", Completed: true, PositionMS: 1000, TestedAt: now.Unix()},
		},
	}
	if err := s.db.Put(t.Context(), "devices", device.ID, device); err != nil {
		t.Fatal(err)
	}
	source := domain.Source{
		Item:       domain.Item{ID: "youtube-hls-fixture", Provider: "youtube", Kind: "video", Playable: true, DurationMS: 60_000},
		URL:        "https://r1.googlevideo.com/video?signature=upstream-secret",
		AudioURL:   "https://r2.googlevideo.com/audio?signature=audio-secret",
		MIME:       "video/mp4",
		Variants:   []string{"720p", "1080p"},
		ResolveURL: "https://wrapper.invalid/resolve/youtube-hls-fixture",
	}
	s.mu.Lock()
	s.searchResults[deviceID] = searchResult{revision: s.configRevision["youtube"], fetched: now, sources: []providers.Source{source}}
	s.mu.Unlock()
	if qualityTest {
		s.deps.RemoteMedia = &qualityTestMedia{width: 1280, height: 720, codec: "h264", profile: "Main"}
	}
	s.deps.Resolver = &mockYouTubeResolver{resolveFn: func(_ context.Context, request domain.Source) (domain.Source, error) {
		request.URL = "https://r1.googlevideo.com/video?signature=upstream-secret"
		request.AudioURL = "https://r2.googlevideo.com/audio?signature=audio-secret"
		request.MIME = "video/mp4"
		request.Item.DurationMS = 60_000
		switch request.ResolveQuality {
		case "1080p":
			request.URL = "https://r1.googlevideo.com/video-1080?signature=upstream-secret"
		}
		return request, nil
	}}
	return s, token, source
}

func TestYouTubeHLSPublishesTicketedRelativeSegmentsWhilePublisherRuns(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)
	diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
	defer restoreLogs()
	resolver := &youtubeDiagnosticContextResolver{inner: s.deps.Resolver}
	s.deps.Resolver = resolver
	position := int64(17_000)
	body := fmt.Sprintf(`{"itemId":%q,"quality":"720p","positionMs":%d}`, source.Item.ID, position)
	request := httptest.NewRequest(http.MethodPost, "/v1/playback", strings.NewReader(body))
	request.Header.Set("X-Zombie-Device", "youtube-hls-device")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Zombie-Diagnostic-Id", "ffffffffffffffff")
	created := httptest.NewRecorder()
	s.ServeHTTP(created, request)
	if created.Code != http.StatusCreated {
		t.Fatalf("create playback: status=%d body=%s", created.Code, created.Body)
	}
	entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
	if len(entries) != 1 {
		t.Fatalf("terminal diagnostic count = %d, want 1; logs: %s", len(entries), diagnosticLogs.String())
	}
	var diagnostic youtubePlaybackDiagnosticEvent
	if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
		t.Fatalf("decode terminal diagnostic: %v", err)
	}
	if len(diagnostic.TraceID) != 16 || diagnostic.TraceID != strings.ToLower(diagnostic.TraceID) {
		t.Fatalf("trace ID is not 16 lowercase characters: %q", diagnostic.TraceID)
	}
	for _, character := range diagnostic.TraceID {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			t.Fatalf("trace ID contains non-hex character %q", character)
		}
	}
	if diagnostic.RequestedQuality != "720p" || diagnostic.ChosenQuality != "720p" || diagnostic.DeliveryRoute != "youtube_hls" || diagnostic.Stage != "playback" || diagnostic.Outcome != "success" {
		t.Fatalf("unexpected terminal playback result: %+v", diagnostic)
	}
	if diagnostic.Resolver != "passed" || diagnostic.StreamProbe != "passed" || diagnostic.QualityResolution != "passed" || diagnostic.HLSGate != "eligible" || diagnostic.HLSGateReason != "eligible" || diagnostic.Publisher != "ready_first_segment" {
		t.Fatalf("YouTube HLS stages were not recorded: %+v", diagnostic)
	}
	if len(resolver.ids) != 2 {
		t.Fatalf("resolver received %d diagnostic contexts, want initial and quality resolution", len(resolver.ids))
	}
	for _, id := range resolver.ids {
		if id != diagnostic.TraceID || id == "ffffffffffffffff" {
			t.Fatalf("resolver diagnostic context = %q, expected generated trace ID %q", id, diagnostic.TraceID)
		}
	}
	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "DIRECT_PLAY" || plan.MIME != "application/vnd.apple.mpegurl" || plan.ResumeMS != 0 || plan.TimelineOffsetMS != position {
		t.Fatalf("HLS plan lost delivery or position semantics: %+v", plan)
	}
	publisher.mu.Lock()
	publication := publisher.last
	selected := publisher.lastSelection
	sourceUsed := publisher.lastSource
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 1 || publication == nil || selected.Quality != "720p" || selected.PositionMS != position {
		t.Fatalf("publisher did not receive selected quality/position: starts=%d selection=%+v", starts, selected)
	}
	if sourceUsed.URL == "" || !strings.Contains(sourceUsed.URL, "googlevideo.com") {
		t.Fatalf("expected resolved upstream source at publisher boundary, got %+v", sourceUsed)
	}
	defer publication.Close()

	wrongURL, err := url.Parse(plan.URL)
	if err != nil {
		t.Fatal(err)
	}
	wrongQuery := wrongURL.Query()
	wrongQuery.Set("ticket", "wrong")
	wrongURL.RawQuery = wrongQuery.Encode()
	wrong := httptestStream(t, s, http.MethodGet, wrongURL.String())
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong ticket accepted: status=%d", wrong.Code)
	}
	playlist := httptestStream(t, s, http.MethodGet, plan.URL)
	if playlist.Code != http.StatusOK {
		t.Fatalf("playlist route: status=%d body=%s", playlist.Code, playlist.Body)
	}
	if playlist.Header().Get("Content-Length") != fmt.Sprint(playlist.Body.Len()) {
		t.Fatalf("playlist Content-Length=%q body bytes=%d", playlist.Header().Get("Content-Length"), playlist.Body.Len())
	}
	if strings.Contains(playlist.Body.String(), "googlevideo.com") || strings.Contains(playlist.Body.String(), "upstream-secret") || strings.Contains(playlist.Body.String(), "audio-secret") {
		t.Fatalf("playlist leaked upstream URLs or credentials: %s", playlist.Body.String())
	}
	if !strings.Contains(playlist.Body.String(), "#EXT-X-PLAYLIST-TYPE:EVENT") || strings.Contains(playlist.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatalf("expected a growing EVENT playlist while publisher is running: %s", playlist.Body.String())
	}
	var reference string
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if strings.HasPrefix(line, plan.SessionID+"/") {
			reference = line
			break
		}
	}
	if reference == "" {
		t.Fatalf("expected basename-relative session/segment URI: %s", playlist.Body.String())
	}
	base, err := url.Parse(plan.URL)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := url.Parse(reference)
	if err != nil {
		t.Fatal(err)
	}
	segmentURL := base.ResolveReference(ref).String()
	segmentPath, err := url.Parse(segmentURL)
	if err != nil {
		t.Fatal(err)
	}
	badSegment := httptestStream(t, s, http.MethodGet, segmentPath.Path+"?ticket=wrong")
	if badSegment.Code != http.StatusUnauthorized {
		t.Fatalf("segment accepted wrong ticket: status=%d", badSegment.Code)
	}
	segment := httptestStream(t, s, http.MethodGet, segmentURL)
	if segment.Code != http.StatusOK || segment.Header().Get("Content-Length") != fmt.Sprint(len(strings.Repeat("ts-packet-data-", 30))) {
		t.Fatalf("segment response status=%d length=%q body=%d", segment.Code, segment.Header().Get("Content-Length"), segment.Body.Len())
	}
	rangeRequest := httptestStreamWithHeader(t, s, http.MethodGet, segmentURL, "Range", "bytes=3-9")
	if rangeRequest.Code != http.StatusPartialContent || rangeRequest.Body.Len() != 7 || rangeRequest.Header().Get("Content-Length") != "7" {
		t.Fatalf("segment Range response status=%d length=%q body=%d", rangeRequest.Code, rangeRequest.Header().Get("Content-Length"), rangeRequest.Body.Len())
	}
	stopped := call(s, http.MethodDelete, "/v1/playback/"+plan.SessionID, "", "youtube-hls-device", token, "")
	if stopped.Code != http.StatusOK {
		t.Fatalf("stop playback: status=%d body=%s", stopped.Code, stopped.Body)
	}
	select {
	case <-publication.closed:
	case <-time.After(time.Second):
		t.Fatal("stopping playback did not close the HLS publisher")
	}
	if _, err := os.Stat(publication.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publisher output directory survived stop: err=%v", err)
	}
}

func TestYouTubeHLSStartupFailureDoesNotFallBackToHDWithoutAudio(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{startErr: errors.New("fixture conversion failure")}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)
	body := fmt.Sprintf(`{"itemId":%q,"quality":"720p"}`, source.Item.ID)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusBadGateway || !strings.Contains(created.Body.String(), "conversion_failed") {
		t.Fatalf("HD HLS failure silently fell back: status=%d body=%s", created.Code, created.Body)
	}
	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 1 {
		t.Fatalf("expected exactly one attempted HD publisher, got %d", starts)
	}
	s.mu.Lock()
	sessionCount := len(s.sessions)
	s.mu.Unlock()
	if sessionCount != 0 {
		t.Fatalf("failed HLS startup leaked %d playback sessions", sessionCount)
	}
}

func TestYouTubeHLSQualitySwitchRetiresOldPublisherAndKeepsPosition(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)
	s.deps.RemoteMedia = &youtubeHLSQualityMedia{}
	created := call(s, http.MethodPost, "/v1/playback", fmt.Sprintf(`{"itemId":%q,"quality":"720p"}`, source.Item.ID), "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create 720p playback: status=%d body=%s", created.Code, created.Body)
	}
	var firstPlan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &firstPlan); err != nil {
		t.Fatal(err)
	}
	publisher.mu.Lock()
	oldPublication := publisher.last
	publisher.mu.Unlock()
	if oldPublication == nil {
		t.Fatal("720p playback did not create an HLS publisher")
	}

	const positionMS = int64(23_000)
	request := fmt.Sprintf(`{"qualityId":"1080p","positionMs":%d}`, positionMS)
	switched := call(s, http.MethodPost, "/v1/playback/"+firstPlan.SessionID+"/quality", request, "youtube-hls-device", token, "")
	if switched.Code != http.StatusCreated {
		t.Fatalf("switch to 1080p: status=%d body=%s", switched.Code, switched.Body)
	}
	var secondPlan domain.Plan
	if err := json.Unmarshal(switched.Body.Bytes(), &secondPlan); err != nil {
		t.Fatal(err)
	}
	if secondPlan.Mode != "DIRECT_PLAY" || secondPlan.MIME != "application/vnd.apple.mpegurl" || secondPlan.TimelineOffsetMS != positionMS {
		t.Fatalf("1080p replacement lost HLS or playback position: %+v", secondPlan)
	}
	select {
	case <-oldPublication.closed:
	case <-time.After(time.Second):
		t.Fatal("quality switch did not close the superseded HLS publisher")
	}
	s.mu.Lock()
	newSession := s.sessions[secondPlan.SessionID]
	s.mu.Unlock()
	if newSession == nil || newSession.youtubeHLSPublisher == nil || newSession.selection.Quality != "1080p" || newSession.selection.PositionMS != positionMS {
		t.Fatalf("replacement session lost quality or position: %+v", newSession)
	}
	if got := s.getQualityPreference(t.Context(), "youtube-hls-device", "youtube", "video"); got != "1080p" {
		t.Fatalf("quality preference = %q, want 1080p", got)
	}
}

func TestYouTubeHLSRetiresTerminalPublisherInsteadOfServingStalePlaylist(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)
	created := call(s, http.MethodPost, "/v1/playback", fmt.Sprintf(`{"itemId":%q,"quality":"720p"}`, source.Item.ID), "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create HLS session: status=%d body=%s", created.Code, created.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	publisher.mu.Lock()
	publication := publisher.last
	publisher.mu.Unlock()
	if publication == nil {
		t.Fatal("playback did not create an HLS publisher")
	}
	publication.mu.Lock()
	publication.state = "failed"
	publication.errText = "fixture source transport failed"
	publication.mu.Unlock()

	response := httptestStream(t, s, http.MethodGet, plan.URL)
	if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), "publisher_failed") {
		t.Fatalf("terminal publisher served stale playlist or unclear response: status=%d body=%s", response.Code, response.Body)
	}
	select {
	case <-publication.closed:
	case <-time.After(time.Second):
		t.Fatal("terminal publisher failure did not close its output directory")
	}
	s.mu.Lock()
	remaining := s.sessions[plan.SessionID]
	s.mu.Unlock()
	if remaining != nil {
		t.Fatal("terminal publisher failure left its playback session active")
	}
}

func TestYouTubeHLSRequiresFreshCompletedEventEvidence(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, token, source := setupYouTubeHLSServer(t, publisher, true)
	diagnosticLogs, restoreLogs := captureYouTubePlaybackLog(t)
	defer restoreLogs()
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &device); err != nil {
		t.Fatal(err)
	}
	for index := range device.Capabilities.Probes {
		if device.Capabilities.Probes[index].ID == "hls-event-h264-aac" {
			device.Capabilities.Probes[index].Status = "UNKNOWN"
		}
	}
	if err := s.db.Put(t.Context(), "devices", device.ID, device); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"itemId":%q,"quality":"720p"}`, source.Item.ID)
	created := call(s, http.MethodPost, "/v1/playback", body, "youtube-hls-device", token, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("legacy playback fallback failed: status=%d body=%s", created.Code, created.Body)
	}
	entries := youtubePlaybackDiagnosticEntries(diagnosticLogs.String())
	if len(entries) != 1 {
		t.Fatalf("terminal diagnostic count = %d, want 1; logs: %s", len(entries), diagnosticLogs.String())
	}
	var diagnostic youtubePlaybackDiagnosticEvent
	if err := json.Unmarshal([]byte(entries[0]), &diagnostic); err != nil {
		t.Fatalf("decode terminal diagnostic: %v", err)
	}
	if diagnostic.Outcome != "success" || diagnostic.HLSGate != "ineligible" || diagnostic.HLSGateReason != "fresh_event_probe" || diagnostic.Publisher != "unreached" {
		t.Fatalf("fresh EVENT ineligibility was not recorded: %+v", diagnostic)
	}
	publisher.mu.Lock()
	starts := publisher.starts
	publisher.mu.Unlock()
	if starts != 0 {
		t.Fatalf("publisher used without fresh completed EVENT evidence: starts=%d", starts)
	}
	if strings.Contains(created.Body.String(), youtubeHLSSessionMode) || strings.Contains(created.Body.String(), "application/vnd.apple.mpegurl") {
		t.Fatalf("nonqualifying device received the new HLS route: %s", created.Body.String())
	}
}

func TestYouTubeHLSIneligibilityReportsCategoricalSourceAndStreamReasons(t *testing.T) {
	publisher := &fakeRemoteHLSPublisher{}
	s, _, source := setupYouTubeHLSServer(t, publisher, false)
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", "youtube-hls-device", &device); err != nil {
		t.Fatal(err)
	}
	metadata := &domain.Metadata{Streams: []domain.Stream{
		{Index: 0, Type: "video", Codec: "h264", Height: 720},
		{Index: 1, Type: "audio", Codec: "aac"},
	}}
	for _, test := range []struct {
		name     string
		mutate   func(*domain.Device, *domain.Source, *domain.Metadata)
		wantCode string
	}{
		{
			name: "event evidence",
			mutate: func(device *domain.Device, _ *domain.Source, _ *domain.Metadata) {
				for index := range device.Capabilities.Probes {
					if device.Capabilities.Probes[index].ID == "hls-event-h264-aac" {
						device.Capabilities.Probes[index].Status = "UNKNOWN"
					}
				}
			},
			wantCode: "fresh_event_probe",
		},
		{
			name: "video codec",
			mutate: func(_ *domain.Device, _ *domain.Source, metadata *domain.Metadata) {
				metadata.Streams[0].Codec = "vp9"
			},
			wantCode: "video_codec",
		},
		{
			name: "video height",
			mutate: func(_ *domain.Device, _ *domain.Source, metadata *domain.Metadata) {
				metadata.Streams[0].Height = 1080
			},
			wantCode: "video_height",
		},
		{
			name: "source audio missing",
			mutate: func(_ *domain.Device, source *domain.Source, _ *domain.Metadata) {
				source.AudioURL = ""
			},
			wantCode: "source_audio_missing",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidateDevice := device
			candidateDevice.Capabilities.Probes = append([]domain.Probe(nil), device.Capabilities.Probes...)
			candidateSource := source
			candidateMetadata := *metadata
			candidateMetadata.Streams = append([]domain.Stream(nil), metadata.Streams...)
			test.mutate(&candidateDevice, &candidateSource, &candidateMetadata)
			eligible, reason := s.youtubeHLSIneligibility(candidateDevice, candidateSource, &candidateMetadata, "720p")
			if eligible || reason != test.wantCode {
				t.Fatalf("eligibility = %t, reason = %q; want false, %q", eligible, reason, test.wantCode)
			}
			if safeYouTubeHLSGateReason(reason) != test.wantCode {
				t.Fatalf("diagnostic reason = %q, want categorical code %q", safeYouTubeHLSGateReason(reason), test.wantCode)
			}
		})
	}
}

func TestYouTubeHLSPublisherFailureDiagnosticIsCategorical(t *testing.T) {
	for _, test := range []struct {
		name          string
		err           error
		wantPublisher string
		wantOutcome   string
	}{
		{name: "first segment timeout", err: errYouTubeHLSPublisherFirstSegmentTimeout, wantPublisher: "first_segment_timeout", wantOutcome: "publisher_first_segment_timeout"},
		{name: "busy", err: media.ErrBusy, wantPublisher: "busy", wantOutcome: "publisher_busy"},
		{name: "cancelled", err: context.Canceled, wantPublisher: "cancelled", wantOutcome: "publisher_cancelled"},
		{name: "failure redaction", err: errors.New("private URL https://example.invalid/?signature=secret"), wantPublisher: "failed", wantOutcome: "publisher_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			publisher, outcome := youtubeHLSPublisherFailureDiagnostic(test.err)
			if publisher != test.wantPublisher || outcome != test.wantOutcome {
				t.Fatalf("failure diagnostic = (%q, %q), want (%q, %q)", publisher, outcome, test.wantPublisher, test.wantOutcome)
			}
			if strings.Contains(publisher, "secret") || strings.Contains(outcome, "secret") {
				t.Fatal("publisher diagnostic included private failure content")
			}
		})
	}
}

func httptestStream(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	return httptestStreamWithHeader(t, s, method, target, "", "")
}

func httptestStreamWithHeader(t *testing.T, s *Server, method, target, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	if header != "" {
		request.Header.Set(header, value)
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	return response
}
