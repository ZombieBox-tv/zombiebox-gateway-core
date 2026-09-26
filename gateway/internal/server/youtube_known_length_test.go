package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

type knownLengthYouTubeRemote struct {
	mu        sync.Mutex
	mode      string
	selection domain.MediaSelection
	height    int
	entered   chan struct{}
	proceed   chan struct{}
	block     bool
	calls     int
	startOne  sync.Once
}

func (stub *knownLengthYouTubeRemote) ProbeRemote(context.Context, domain.Source) (domain.Metadata, error) {
	height := stub.height
	if height == 0 {
		height = 720
	}
	width, profile := 1280, "Main"
	if height > 720 {
		width, profile = 1920, "High"
	}
	return domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "h264", Profile: profile, Width: width, Height: height},
		{Type: "audio", Codec: "aac"},
	}}, nil
}

func (stub *knownLengthYouTubeRemote) ConvertRemote(ctx context.Context, _ domain.Source, mode string, selection domain.MediaSelection, output io.Writer) error {
	stub.mu.Lock()
	stub.mode = mode
	stub.selection = selection
	stub.calls++
	block := stub.block
	proceed := stub.proceed
	stub.mu.Unlock()
	if stub.entered != nil {
		stub.startOne.Do(func() { close(stub.entered) })
	}
	if block {
		if proceed == nil {
			<-ctx.Done()
			return ctx.Err()
		}
		select {
		case <-proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err := io.WriteString(output, "fragmented-mp4")
	return err
}

func (stub *knownLengthYouTubeRemote) lastMode() string {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.mode
}

func (stub *knownLengthYouTubeRemote) lastSelectionPosition() int64 {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.selection.PositionMS
}

func (stub *knownLengthYouTubeRemote) callCount() int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.calls
}

func knownLengthEvidenceDevice(id string, now time.Time) domain.Device {
	device := domain.Device{ID: id, Registration: domain.Registration{ClientVersion: "test", Platform: domain.Platform{AndroidAPI: 13}}}
	device.Capabilities = domain.Capabilities{
		Version:      1,
		DeviceID:     device.ID,
		SuiteVersion: devices.ProbeSuiteVersion,
		CacheKey:     devices.ProbeCacheKey(device),
		Probes: []domain.Probe{
			{ID: "http-fmp4", Status: "PASS", PositionMS: 2946, Completed: true, TestedAt: now.Unix()},
			{ID: "http-fmp4-seek", Status: "PASS", PositionMS: 1500, Completed: true, TestedAt: now.Unix()},
			{ID: "http-fmp4-chunked", Status: "UNKNOWN", Detail: "what=0,extra=0@prepare http=200,video/mp4", TestedAt: now.Unix()},
			{ID: "aac", Status: "PASS", PositionMS: 1000, Completed: true, TestedAt: now.Unix()},
			{ID: "h264-720-main", Status: "PASS", PositionMS: 1000, Completed: true, TestedAt: now.Unix()},
		},
	}
	return device
}

func knownLengthYouTubeSource() domain.Source {
	return domain.Source{
		Item:       domain.Item{ID: "youtube-known-length", Provider: "youtube", Kind: "video", Playable: true},
		URL:        "https://r1.googlevideo.com/video?id=fixture",
		AudioURL:   "https://r1.googlevideo.com/audio?id=fixture",
		MIME:       "video/mp4",
		Variants:   []string{"720p"},
		ResolveURL: "https://wrapper.invalid/resolve/fixture",
	}
}

func setupKnownLengthYouTubeServer(t *testing.T, deviceID string) (*Server, string, domain.Source, *knownLengthYouTubeRemote) {
	t.Helper()
	s := testServer(t, nil, t.TempDir())
	if err := s.SeedProviders(context.Background(), map[string]providers.Config{"youtube": {Enabled: true, URL: "https://wrapper.invalid", Token: strings.Repeat("s", 32)}}); err != nil {
		t.Fatal(err)
	}
	owner := pair(t, s, deviceID)
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", deviceID, &device); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	device.Capabilities = knownLengthEvidenceDevice(deviceID, now).Capabilities
	device.Capabilities.CacheKey = devices.ProbeCacheKey(device)
	if err := s.db.Put(t.Context(), "devices", deviceID, device); err != nil {
		t.Fatal(err)
	}
	source := knownLengthYouTubeSource()
	s.mu.Lock()
	s.searchResults[device.ID] = searchResult{revision: s.configRevision["youtube"], fetched: now, sources: []providers.Source{source}}
	s.mu.Unlock()
	stub := &knownLengthYouTubeRemote{}
	s.deps.RemoteMedia = stub
	s.deps.Resolver = &mockYouTubeResolver{}
	return s, owner, source, stub
}

func TestRequiresKnownLengthYouTubeRemuxUsesFreshPairedEvidence(t *testing.T) {
	now := time.Now()
	source := knownLengthYouTubeSource()
	metadata := &domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "h264"},
		{Type: "audio", Codec: "aac"},
	}}
	base := knownLengthEvidenceDevice("probe-device", now)
	if !requiresKnownLengthYouTubeRemux(base, source, metadata, "REMUX", now) {
		t.Fatal("fresh suite-2 PASS plus fresh chunked UNKNOWN did not select known-length REMUX")
	}
	explicitFailure := knownLengthEvidenceDevice("probe-device", now)
	for index := range explicitFailure.Capabilities.Probes {
		if explicitFailure.Capabilities.Probes[index].ID == "http-fmp4-chunked" {
			explicitFailure.Capabilities.Probes[index].Status = "FAIL"
			explicitFailure.Capabilities.Probes[index].Detail = "decoder rejected stream"
		}
	}
	if !requiresKnownLengthYouTubeRemux(explicitFailure, source, metadata, "REMUX", now) {
		t.Fatal("fresh chunked FAIL did not select known-length REMUX")
	}

	tests := []struct {
		name   string
		change func(*domain.Device, *domain.Source, *domain.Metadata)
	}{
		{name: "wrong mode", change: func(_ *domain.Device, _ *domain.Source, _ *domain.Metadata) {}},
		{name: "missing current suite", change: func(device *domain.Device, _ *domain.Source, _ *domain.Metadata) {
			device.Capabilities.SuiteVersion = 0
		}},
		{name: "wrong cache key", change: func(device *domain.Device, _ *domain.Source, _ *domain.Metadata) {
			device.Capabilities.CacheKey = "stale"
		}},
		{name: "wrong device identity", change: func(device *domain.Device, _ *domain.Source, _ *domain.Metadata) {
			device.Capabilities.DeviceID = "other-device"
		}},
		{name: "missing chunked comparison", change: func(device *domain.Device, _ *domain.Source, _ *domain.Metadata) {
			device.Capabilities.Probes = []domain.Probe{device.Capabilities.Probes[0], device.Capabilities.Probes[1]}
		}},
		{name: "stale chunked comparison", change: func(device *domain.Device, _ *domain.Source, _ *domain.Metadata) {
			for index := range device.Capabilities.Probes {
				if device.Capabilities.Probes[index].ID == "http-fmp4-chunked" {
					device.Capabilities.Probes[index].TestedAt = now.Add(-8 * 24 * time.Hour).Unix()
				}
			}
		}},
		{name: "unknown without recorded prepare error", change: func(device *domain.Device, _ *domain.Source, _ *domain.Metadata) {
			for index := range device.Capabilities.Probes {
				if device.Capabilities.Probes[index].ID == "http-fmp4-chunked" {
					device.Capabilities.Probes[index].Detail = "network timeout"
				}
			}
		}},
		{name: "chunked path passed", change: func(device *domain.Device, _ *domain.Source, _ *domain.Metadata) {
			for index := range device.Capabilities.Probes {
				if device.Capabilities.Probes[index].ID == "http-fmp4-chunked" {
					device.Capabilities.Probes[index].Status = "PASS"
					device.Capabilities.Probes[index].PositionMS = 1000
				}
			}
		}},
		{name: "known-length did not advance", change: func(device *domain.Device, _ *domain.Source, _ *domain.Metadata) {
			device.Capabilities.Probes[0].Completed = false
			device.Capabilities.Probes[0].PositionMS = 0
		}},
		{name: "live source", change: func(_ *domain.Device, source *domain.Source, _ *domain.Metadata) { source.Live = true }},
		{name: "combined source", change: func(_ *domain.Device, source *domain.Source, _ *domain.Metadata) { source.AudioURL = "" }},
		{name: "not youtube", change: func(_ *domain.Device, source *domain.Source, _ *domain.Metadata) { source.Item.Provider = "plex" }},
		{name: "not h264 and aac", change: func(_ *domain.Device, _ *domain.Source, metadata *domain.Metadata) {
			metadata.Streams[0].Codec = "hevc"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := knownLengthEvidenceDevice("probe-device", now)
			candidateSource := knownLengthYouTubeSource()
			candidateMetadata := &domain.Metadata{Streams: append([]domain.Stream(nil), metadata.Streams...)}
			test.change(&device, &candidateSource, candidateMetadata)
			mode := "REMUX"
			if test.name == "wrong mode" {
				mode = "TRANSCODE"
			}
			if requiresKnownLengthYouTubeRemux(device, candidateSource, candidateMetadata, mode, now) {
				t.Fatal("nonqualifying source or evidence selected known-length REMUX")
			}
		})
	}
}

func TestKnownLengthYouTubeSeekRequiresFreshPass(t *testing.T) {
	now := time.Now()
	metadata := &domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "h264"},
		{Type: "audio", Codec: "aac"},
	}}
	tests := []struct {
		name   string
		change func(*domain.Device)
	}{
		{name: "missing", change: func(device *domain.Device) {
			filtered := device.Capabilities.Probes[:0]
			for _, probe := range device.Capabilities.Probes {
				if probe.ID != "http-fmp4-seek" {
					filtered = append(filtered, probe)
				}
			}
			device.Capabilities.Probes = filtered
		}},
		{name: "failed", change: func(device *domain.Device) {
			for index := range device.Capabilities.Probes {
				if device.Capabilities.Probes[index].ID == "http-fmp4-seek" {
					device.Capabilities.Probes[index].Status = "FAIL"
				}
			}
		}},
		{name: "stale", change: func(device *domain.Device) {
			for index := range device.Capabilities.Probes {
				if device.Capabilities.Probes[index].ID == "http-fmp4-seek" {
					device.Capabilities.Probes[index].TestedAt = now.Add(-8 * 24 * time.Hour).Unix()
				}
			}
		}},
		{name: "did not advance", change: func(device *domain.Device) {
			for index := range device.Capabilities.Probes {
				if device.Capabilities.Probes[index].ID == "http-fmp4-seek" {
					device.Capabilities.Probes[index].PositionMS = 0
					device.Capabilities.Probes[index].Completed = false
				}
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := knownLengthEvidenceDevice("seek-device", now)
			test.change(&device)
			if !requiresKnownLengthYouTubeRemux(device, knownLengthYouTubeSource(), metadata, "REMUX", now) {
				t.Fatal("missing seek evidence unexpectedly disabled the existing position-zero known-length spool")
			}
			if supportsKnownLengthYouTubeSeek(device, knownLengthYouTubeSource(), metadata, "REMUX", now) {
				t.Fatal("missing, failed, stale, or non-advancing seek probe enabled resumed REMUX")
			}
		})
	}
}

func TestYouTubePlaybackKeepsRemuxModeAndSpoolsOnlyWithFreshEvidence(t *testing.T) {
	s, owner, _, stub := setupKnownLengthYouTubeServer(t, "known-length-device")

	created := call(s, http.MethodPost, "/v1/playback", `{"itemId":"youtube-known-length","positionMs":10000}`, "known-length-device", owner, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create playback: %d %s", created.Code, created.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "REMUX" || !plan.Seekable || plan.ResumeMS != 10000 || plan.TimelineOffsetMS != 0 {
		t.Fatalf("transport workaround changed selected playback mode: %q", plan.Mode)
	}
	s.mu.Lock()
	sess := s.sessions[plan.SessionID]
	if sess == nil || !sess.knownLengthRemux || sess.mode != "REMUX" || sess.selection.PositionMS != 0 {
		s.mu.Unlock()
		t.Fatal("fresh transport comparison did not opt the REMUX session into known-length delivery")
	}
	s.mu.Unlock()
	qualities := call(s, http.MethodGet, "/v1/playback/"+plan.SessionID+"/qualities", "", "known-length-device", owner, "")
	if qualities.Code != http.StatusOK || !strings.Contains(qualities.Body.String(), `"id":"720p"`) {
		t.Fatalf("manual 720p quality missing from inventory: %d %s", qualities.Code, qualities.Body)
	}
	selected := call(s, http.MethodPost, "/v1/playback/"+plan.SessionID+"/quality", `{"qualityId":"720p","positionMs":15000}`, "known-length-device", owner, "")
	if selected.Code != http.StatusCreated {
		t.Fatalf("select manual 720p: %d %s", selected.Code, selected.Body)
	}
	var selectedPlan domain.Plan
	if err := json.Unmarshal(selected.Body.Bytes(), &selectedPlan); err != nil {
		t.Fatal(err)
	}
	if selectedPlan.Mode != "REMUX" || !selectedPlan.Seekable || selectedPlan.ResumeMS != 15000 || selectedPlan.TimelineOffsetMS != 0 {
		t.Fatalf("manual rendition changed stream-copy mode: %q", selectedPlan.Mode)
	}
	s.mu.Lock()
	selectedSession := s.sessions[selectedPlan.SessionID]
	if selectedSession == nil || !selectedSession.knownLengthRemux || selectedSession.mode != "REMUX" || selectedSession.selection.PositionMS != 0 {
		s.mu.Unlock()
		t.Fatal("manual quality replacement lost known-length REMUX eligibility")
	}
	s.mu.Unlock()

	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, selectedPlan.URL, nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "video/mp4" || response.Header().Get("Content-Length") != "14" {
		t.Fatalf("known-length REMUX response: status=%d content-type=%q content-length=%q", response.Code, response.Header().Get("Content-Type"), response.Header().Get("Content-Length"))
	}
	if response.Header().Get("Transfer-Encoding") == "chunked" || response.Body.String() != "fragmented-mp4" {
		t.Fatalf("spooled response changed bytes or used chunked framing: transfer-encoding=%q body=%q", response.Header().Get("Transfer-Encoding"), response.Body.String())
	}
	if mode := stub.lastMode(); mode != "REMUX" {
		t.Fatalf("known-length conversion changed stream-copy mode: %q", mode)
	}
	if position := stub.lastSelectionPosition(); position != 0 {
		t.Fatalf("REMUX conversion received nonzero FFmpeg position: %d", position)
	}
	ranged := httptest.NewRecorder()
	rangeRequest := httptest.NewRequest(http.MethodGet, selectedPlan.URL, nil)
	rangeRequest.Header.Set("Range", "bytes=2-")
	s.ServeHTTP(ranged, rangeRequest)
	if ranged.Code != http.StatusPartialContent || ranged.Body.String() != "agmented-mp4" || ranged.Header().Get("Content-Range") != "bytes 2-13/14" {
		t.Fatalf("known-length REMUX range: status=%d content-range=%q body=%q", ranged.Code, ranged.Header().Get("Content-Range"), ranged.Body.String())
	}
}

func TestYouTubeSavedProgressUsesKnownLengthYouTubeRemux(t *testing.T) {
	s, owner, source, _ := setupKnownLengthYouTubeServer(t, "known-length-saved-progress")
	if err := s.db.Put(t.Context(), "progress:"+"known-length-saved-progress", source.Item.ID, domain.Progress{
		Item: source.Item, PositionMS: 12000, DurationMS: 90000, State: "PAUSED",
	}); err != nil {
		t.Fatal(err)
	}

	created := call(s, http.MethodPost, "/v1/playback", `{"itemId":"youtube-known-length"}`, "known-length-saved-progress", owner, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create playback from saved progress: %d %s", created.Code, created.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "REMUX" || !plan.Seekable || plan.ResumeMS != 12000 || plan.TimelineOffsetMS != 0 {
		t.Fatalf("saved progress did not use seekable known-length REMUX: %+v", plan)
	}
	s.mu.Lock()
	sess := s.sessions[plan.SessionID]
	s.mu.Unlock()
	if sess == nil || !sess.knownLengthRemux || sess.selection.PositionMS != 0 {
		t.Fatalf("saved resume position leaked into REMUX conversion selection: %+v", sess)
	}
}

func TestInitialYouTubeSplitQualityRestartsAtZeroWithoutSeekEvidence(t *testing.T) {
	for _, quality := range []string{"720p", "1080p"} {
		for _, selection := range []string{"explicit", "preference"} {
			t.Run(quality+"/"+selection, func(t *testing.T) {
				deviceID := "known-length-initial-" + quality + "-" + selection
				s, owner, _, stub := setupKnownLengthYouTubeServer(t, deviceID)
				stub.height = 720
				if quality == "1080p" {
					stub.height = 1080
					s.mu.Lock()
					result := s.searchResults[deviceID]
					source := result.sources[0]
					source.Variants = []string{"1080p", "720p"}
					result.sources[0] = source
					s.searchResults[deviceID] = result
					s.mu.Unlock()
				}

				var device domain.Device
				if err := s.db.Get(t.Context(), "devices", deviceID, &device); err != nil {
					t.Fatal(err)
				}
				for index := range device.Capabilities.Probes {
					if device.Capabilities.Probes[index].ID == "http-fmp4-seek" {
						device.Capabilities.Probes[index].Status = "FAIL"
					}
				}
				if quality == "1080p" {
					device.Capabilities.Probes = append(device.Capabilities.Probes, domain.Probe{
						ID: "h264-1080-high", Status: "PASS", PositionMS: 1000, Completed: true, TestedAt: time.Now().Unix(),
					})
				}
				if err := s.db.Put(t.Context(), "devices", deviceID, device); err != nil {
					t.Fatal(err)
				}

				if selection == "preference" {
					if err := s.setQualityPreference(t.Context(), deviceID, "youtube", "video", quality); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.db.Put(t.Context(), "progress:"+deviceID, "youtube-known-length", domain.Progress{
					Item:       domain.Item{ID: "youtube-known-length", Provider: "youtube", Kind: "video", Playable: true},
					PositionMS: 15000, DurationMS: 90000, State: "PAUSED",
				}); err != nil {
					t.Fatal(err)
				}

				body := `{"itemId":"youtube-known-length","networkAdaptation":false}`
				if selection == "explicit" {
					body = `{"itemId":"youtube-known-length","quality":"` + quality + `","networkAdaptation":false}`
				}
				created := call(s, http.MethodPost, "/v1/playback", body, deviceID, owner, "")
				if created.Code != http.StatusCreated {
					t.Fatalf("create playback: %d %s", created.Code, created.Body)
				}
				var plan domain.Plan
				if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
					t.Fatal(err)
				}
				if plan.Mode != "REMUX" || plan.Seekable || plan.ResumeMS != 0 || plan.TimelineOffsetMS != 0 {
					t.Fatalf("native selected quality did not restart at zero via known-length REMUX: %+v", plan)
				}

				s.mu.Lock()
				sess := s.sessions[plan.SessionID]
				s.mu.Unlock()
				if sess == nil || !sess.knownLengthRemux || sess.mode != "REMUX" || sess.selection.Quality != quality || sess.selection.PositionMS != 0 || sess.source.ResolveQuality != quality {
					t.Fatalf("initial selected quality or restart position was lost: %+v", sess)
				}

				response := httptest.NewRecorder()
				s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, plan.URL, nil))
				if response.Code != http.StatusOK || response.Header().Get("Content-Length") != "14" || response.Header().Get("Transfer-Encoding") == "chunked" {
					t.Fatalf("initial selected quality did not use known-length delivery: status=%d length=%q transfer-encoding=%q", response.Code, response.Header().Get("Content-Length"), response.Header().Get("Transfer-Encoding"))
				}
				if stub.lastMode() != "REMUX" || stub.lastSelectionPosition() != 0 {
					t.Fatalf("initial quality conversion changed mode or position: mode=%q position=%d", stub.lastMode(), stub.lastSelectionPosition())
				}
			})
		}
	}
}

func TestYouTubeManualQualityRestartsAtZeroWithoutFreshSeekEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*domain.Device)
	}{
		{name: "missing", change: func(device *domain.Device) {
			filtered := device.Capabilities.Probes[:0]
			for _, probe := range device.Capabilities.Probes {
				if probe.ID != "http-fmp4-seek" {
					filtered = append(filtered, probe)
				}
			}
			device.Capabilities.Probes = filtered
		}},
		{name: "failed", change: func(device *domain.Device) {
			for index := range device.Capabilities.Probes {
				if device.Capabilities.Probes[index].ID == "http-fmp4-seek" {
					device.Capabilities.Probes[index].Status = "FAIL"
				}
			}
		}},
		{name: "stale", change: func(device *domain.Device) {
			for index := range device.Capabilities.Probes {
				if device.Capabilities.Probes[index].ID == "http-fmp4-seek" {
					device.Capabilities.Probes[index].TestedAt = time.Now().Add(-8 * 24 * time.Hour).Unix()
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			deviceID := "known-length-resume-" + test.name
			s, owner, _, _ := setupKnownLengthYouTubeServer(t, deviceID)
			var device domain.Device
			if err := s.db.Get(t.Context(), "devices", deviceID, &device); err != nil {
				t.Fatal(err)
			}
			test.change(&device)
			if err := s.db.Put(t.Context(), "devices", deviceID, device); err != nil {
				t.Fatal(err)
			}

			created := call(s, http.MethodPost, "/v1/playback", `{"itemId":"youtube-known-length","positionMs":15000}`, deviceID, owner, "")
			if created.Code != http.StatusCreated {
				t.Fatalf("create playback without fresh seek evidence: %d %s", created.Code, created.Body)
			}
			var plan domain.Plan
			if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			if plan.Mode != "TRANSCODE" || plan.Seekable || plan.ResumeMS != 0 || plan.TimelineOffsetMS != 15000 {
				t.Fatalf("missing fresh seek evidence did not preserve the chunked transcode fallback: %+v", plan)
			}
			s.mu.Lock()
			sess := s.sessions[plan.SessionID]
			s.mu.Unlock()
			if sess == nil || sess.knownLengthRemux || sess.selection.PositionMS != 15000 {
				t.Fatalf("fallback session has incorrect seek state: %+v", sess)
			}

			selected := call(s, http.MethodPost, "/v1/playback/"+plan.SessionID+"/quality", `{"qualityId":"720p","positionMs":19000}`, deviceID, owner, "")
			if selected.Code != http.StatusCreated {
				t.Fatalf("select quality without fresh seek evidence: %d %s", selected.Code, selected.Body)
			}
			var selectedPlan domain.Plan
			if err := json.Unmarshal(selected.Body.Bytes(), &selectedPlan); err != nil {
				t.Fatal(err)
			}
			if selectedPlan.Mode != "REMUX" || selectedPlan.Seekable || selectedPlan.ResumeMS != 0 || selectedPlan.TimelineOffsetMS != 0 {
				t.Fatalf("manual native quality did not restart from zero without seek evidence: %+v", selectedPlan)
			}
			s.mu.Lock()
			selectedSession := s.sessions[selectedPlan.SessionID]
			s.mu.Unlock()
			if selectedSession == nil || !selectedSession.knownLengthRemux || selectedSession.selection.PositionMS != 0 {
				t.Fatalf("known-length REMUX fallback has incorrect seek state: %+v", selectedSession)
			}
			if preference := s.getQualityPreference(t.Context(), deviceID, "youtube", "video"); preference != "720p" {
				t.Fatalf("accepted manual selection did not persist its quality preference: %q", preference)
			}
		})
	}
}

func TestYouTubeManualQualityKeepsPositionWithoutKnownLengthEvidence(t *testing.T) {
	deviceID := "known-length-resume-no-known-length"
	s, owner, _, _ := setupKnownLengthYouTubeServer(t, deviceID)
	var device domain.Device
	if err := s.db.Get(t.Context(), "devices", deviceID, &device); err != nil {
		t.Fatal(err)
	}
	filtered := device.Capabilities.Probes[:0]
	for _, probe := range device.Capabilities.Probes {
		if probe.ID != "http-fmp4-chunked" {
			filtered = append(filtered, probe)
		}
	}
	device.Capabilities.Probes = filtered
	if err := s.db.Put(t.Context(), "devices", deviceID, device); err != nil {
		t.Fatal(err)
	}

	created := call(s, http.MethodPost, "/v1/playback", `{"itemId":"youtube-known-length","positionMs":15000}`, deviceID, owner, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create playback without known-length comparison: %d %s", created.Code, created.Body)
	}
	var plan domain.Plan
	if err := json.Unmarshal(created.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}

	selected := call(s, http.MethodPost, "/v1/playback/"+plan.SessionID+"/quality", `{"qualityId":"720p","positionMs":19000}`, deviceID, owner, "")
	if selected.Code != http.StatusCreated {
		t.Fatalf("select quality without known-length comparison: %d %s", selected.Code, selected.Body)
	}
	var selectedPlan domain.Plan
	if err := json.Unmarshal(selected.Body.Bytes(), &selectedPlan); err != nil {
		t.Fatal(err)
	}
	if selectedPlan.Mode != "TRANSCODE" || selectedPlan.Seekable || selectedPlan.ResumeMS != 0 || selectedPlan.TimelineOffsetMS != 19000 {
		t.Fatalf("missing known-length evidence changed the requested timeline: %+v", selectedPlan)
	}
	s.mu.Lock()
	selectedSession := s.sessions[selectedPlan.SessionID]
	s.mu.Unlock()
	if selectedSession == nil || selectedSession.knownLengthRemux || selectedSession.selection.PositionMS != 19000 {
		t.Fatalf("missing known-length evidence changed the session position: %+v", selectedSession)
	}
}

func TestYouTubeREMUXWithoutFreshComparisonStaysStreaming(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	stub := &knownLengthYouTubeRemote{}
	s.deps.RemoteMedia = stub
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.sessions["unknown-probe"] = &session{
		mode: "REMUX", ticket: "ticket", expires: time.Now().Add(time.Minute), ctx: ctx, cancel: cancel,
		source: knownLengthYouTubeSource(), metadata: &domain.Metadata{Streams: []domain.Stream{{Type: "video", Codec: "h264"}, {Type: "audio", Codec: "aac"}}},
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/streams/unknown-probe?ticket=ticket", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Length") != "" || response.Body.String() != "fragmented-mp4" {
		t.Fatalf("unproven device path did not keep the streaming REMUX: status=%d content-length=%q body=%q", response.Code, response.Header().Get("Content-Length"), response.Body.String())
	}
	if mode := stub.lastMode(); mode != "REMUX" {
		t.Fatalf("unproven device path changed stream-copy mode: %q", mode)
	}
}

func TestKnownLengthYouTubeREMUXSpoolCancelsWithSession(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	stub := &knownLengthYouTubeRemote{entered: make(chan struct{}), block: true}
	s.deps.RemoteMedia = stub
	sessionCtx, cancelSession := context.WithCancel(t.Context())
	sess := &session{
		mode: "REMUX", knownLengthRemux: true, ticket: "ticket", expires: time.Now().Add(time.Minute),
		ctx: sessionCtx, cancel: cancelSession, source: knownLengthYouTubeSource(),
		metadata: &domain.Metadata{Streams: []domain.Stream{{Type: "video", Codec: "h264"}, {Type: "audio", Codec: "aac"}}},
	}
	s.sessions["cancel-known-length"] = sess
	request := httptest.NewRequest(http.MethodGet, "/v1/streams/cancel-known-length?ticket=ticket", nil)
	response := httptest.NewRecorder()
	requestDone := make(chan struct{})
	go func() {
		s.ServeHTTP(response, request)
		close(requestDone)
	}()
	select {
	case <-stub.entered:
	case <-time.After(time.Second):
		t.Fatal("known-length REMUX conversion did not start")
	}
	spool := sess.hybridSpool
	if spool == nil {
		t.Fatal("known-length REMUX did not allocate a bounded spool")
	}
	spoolPath := spool.filePath()
	if spoolPath == "" {
		t.Fatal("known-length REMUX spool did not create its private file")
	}
	cancelSession()
	select {
	case <-spool.done:
	case <-time.After(time.Second):
		t.Fatal("session cancellation did not stop the spool conversion")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("stream request did not exit after session cancellation")
	}
	if _, err := os.Stat(spoolPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled spool file was not removed: %v", err)
	}
}

func TestKnownLengthYouTubeREMUXTimeoutIsRetryable(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	s.hybridStartupWait = 20 * time.Millisecond
	stub := &knownLengthYouTubeRemote{entered: make(chan struct{}), proceed: make(chan struct{}), block: true}
	s.deps.RemoteMedia = stub
	sessionCtx, cancelSession := context.WithCancel(t.Context())
	defer cancelSession()
	s.sessions["retry-known-length"] = &session{
		mode: "REMUX", knownLengthRemux: true, ticket: "ticket", expires: time.Now().Add(time.Minute),
		ctx: sessionCtx, cancel: cancelSession, source: knownLengthYouTubeSource(),
		metadata: &domain.Metadata{Streams: []domain.Stream{{Type: "video", Codec: "h264"}, {Type: "audio", Codec: "aac"}}},
	}
	path := "/v1/streams/retry-known-length?ticket=ticket"
	first := httptest.NewRecorder()
	s.ServeHTTP(first, httptest.NewRequest(http.MethodGet, path, nil))
	if first.Code != http.StatusGatewayTimeout || first.Header().Get("Retry-After") != "2" || !strings.Contains(first.Body.String(), "conversion_timeout") {
		t.Fatalf("spool startup timeout was not explicitly retryable: status=%d retry-after=%q body=%q", first.Code, first.Header().Get("Retry-After"), first.Body.String())
	}
	select {
	case <-stub.entered:
	case <-time.After(time.Second):
		t.Fatal("spool conversion did not continue after the first response timed out")
	}
	sess := s.sessions["retry-known-length"]
	spool := sess.hybridSpool
	if spool == nil {
		t.Fatal("startup timeout discarded the active spool")
	}
	select {
	case <-spool.done:
		t.Fatal("spool completed before the retry test released conversion")
	default:
	}
	close(stub.proceed)
	select {
	case <-spool.done:
	case <-time.After(time.Second):
		t.Fatal("spool did not complete after conversion resumed")
	}
	second := httptest.NewRecorder()
	s.ServeHTTP(second, httptest.NewRequest(http.MethodGet, path, nil))
	if second.Code != http.StatusOK || second.Header().Get("Content-Length") != "14" || second.Body.String() != "fragmented-mp4" {
		t.Fatalf("retry after spool timeout failed: status=%d length=%q body=%q", second.Code, second.Header().Get("Content-Length"), second.Body.String())
	}
	if stub.callCount() != 1 {
		t.Fatalf("retry reran the same REMUX conversion %d times", stub.callCount())
	}
}

func TestKnownLengthYouTubeREMUXUsesSharedSpoolQuota(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	s.hybridSpools.Close()
	s.opt.HybridMaxSpoolBytes = 2 << 20
	s.opt.HybridAggregateQuotaBytes = 1 << 20
	s.hybridSpools = newHybridSpoolManager(s.opt)
	stub := &knownLengthYouTubeRemote{}
	s.deps.RemoteMedia = stub
	sessionCtx, cancelSession := context.WithCancel(t.Context())
	defer cancelSession()
	s.sessions["quota-known-length"] = &session{
		mode: "REMUX", knownLengthRemux: true, ticket: "ticket", expires: time.Now().Add(time.Minute),
		ctx: sessionCtx, cancel: cancelSession, source: knownLengthYouTubeSource(),
		metadata: &domain.Metadata{Streams: []domain.Stream{{Type: "video", Codec: "h264"}, {Type: "audio", Codec: "aac"}}},
	}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/streams/quota-known-length?ticket=ticket", nil))
	if response.Code != http.StatusInsufficientStorage || !strings.Contains(response.Body.String(), "spool_quota_exceeded") {
		t.Fatalf("bounded spool quota not surfaced: status=%d body=%q", response.Code, response.Body.String())
	}
	if stub.callCount() != 0 {
		t.Fatal("conversion started despite insufficient spool reservation")
	}
}
