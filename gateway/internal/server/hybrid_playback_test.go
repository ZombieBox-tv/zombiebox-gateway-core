package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/providers"
)

type hybridRemoteStub struct {
	remoteMediaStub
	mu        sync.Mutex
	mode      string
	selection domain.MediaSelection
	err       error
	delay     time.Duration
}

func (stub *hybridRemoteStub) ConvertRemote(ctx context.Context, _ domain.Source, mode string, selection domain.MediaSelection, output io.Writer) error {
	stub.mu.Lock()
	stub.mode, stub.selection = mode, selection
	delay := stub.delay
	stubErr := stub.err
	stub.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if stubErr != nil {
		return stubErr
	}
	_, err := io.WriteString(output, "fragmented-mp4")
	return err
}

func (stub *hybridRemoteStub) getSelection() (string, domain.MediaSelection) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.mode, stub.selection
}

func TestHybridStreamUsesBoundedSpool(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	stub := &hybridRemoteStub{}
	s.deps.RemoteMedia = stub
	audioID := 2
	sessionCtx, sessionCancel := context.WithCancel(t.Context())
	defer sessionCancel()
	s.sessions["hybrid"] = &session{
		mode: "HYBRID", ticket: "private-ticket", expires: time.Now().Add(time.Minute),
		ctx: sessionCtx, cancel: sessionCancel,
		source:    domain.Source{URL: "https://media.test/movie", MIME: "video/mp4"},
		selection: domain.MediaSelection{AudioID: &audioID},
	}
	request := func(method, rangeValue string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/streams/hybrid?ticket=private-ticket", nil)
		if rangeValue != "" {
			r.Header.Set("Range", rangeValue)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	// 1. GET response provides non-chunked delivery with Content-Length and Accept-Ranges
	getResp := request(http.MethodGet, "")
	if getResp.Code != http.StatusOK {
		t.Fatalf("hybrid stream: %d", getResp.Code)
	}
	if getResp.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("unexpected Content-Type: %q", getResp.Header().Get("Content-Type"))
	}
	if getResp.Header().Get("Content-Length") != "14" {
		t.Fatalf("missing or wrong Content-Length: %q", getResp.Header().Get("Content-Length"))
	}
	if getResp.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("missing Accept-Ranges: %q", getResp.Header().Get("Accept-Ranges"))
	}
	if getResp.Header().Get("Transfer-Encoding") == "chunked" {
		t.Fatal("hybrid stream must not be chunked for legacy clients")
	}
	if getResp.Body.String() != "fragmented-mp4" {
		t.Fatalf("unexpected body: %q", getResp.Body.String())
	}
	mode, sel := stub.getSelection()
	if mode != "HYBRID" || sel.AudioID == nil || *sel.AudioID != audioID {
		t.Fatalf("conversion lost selected audio: %q %+v", mode, sel)
	}

	// 2. HEAD response returns matching headers without body
	headResp := request(http.MethodHead, "")
	if headResp.Code != http.StatusOK {
		t.Fatalf("hybrid HEAD failed: %d", headResp.Code)
	}
	if headResp.Header().Get("Content-Length") != "14" || headResp.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("head headers mismatch: len=%q type=%q", headResp.Header().Get("Content-Length"), headResp.Header().Get("Content-Type"))
	}
	if headResp.Body.Len() != 0 {
		t.Fatalf("HEAD request returned body of length %d", headResp.Body.Len())
	}

	// 3. Sub-range request is satisfied (seek support)
	rangeResp := request(http.MethodGet, "bytes=5-")
	if rangeResp.Code != http.StatusPartialContent {
		t.Fatalf("range request: expected 206, got %d", rangeResp.Code)
	}
	if rangeResp.Body.String() != "ented-mp4" {
		t.Fatalf("unexpected range body: %q", rangeResp.Body.String())
	}
	if rangeResp.Header().Get("Content-Range") != "bytes 5-13/14" {
		t.Fatalf("unexpected Content-Range: %q", rangeResp.Header().Get("Content-Range"))
	}

	// 4. Out-of-bounds range request returns 416
	outOfBounds := request(http.MethodGet, "bytes=100-")
	if outOfBounds.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416 for out-of-bounds range, got %d", outOfBounds.Code)
	}
}

func TestHybridStreamPrerequisitesAndFailures(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	stub := &hybridRemoteStub{}
	s.deps.RemoteMedia = stub

	makeSession := func(id string, stubErr error) {
		stub.err = stubErr
		s.sessions[id] = &session{
			mode: "HYBRID", ticket: "ticket-" + id, expires: time.Now().Add(time.Minute),
			ctx: t.Context(), cancel: func() {},
			source: domain.Source{URL: "https://media.test/" + id, MIME: "video/mp4"},
		}
	}
	callStream := func(id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/streams/"+id+"?ticket=ticket-"+id, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	// Missing adapter returns 502
	s.deps.RemoteMedia = nil
	makeSession("no-adapter", nil)
	if resp := callStream("no-adapter"); resp.Code != http.StatusBadGateway {
		t.Fatalf("missing adapter advertised a playable stream: %d", resp.Code)
	}

	// Conversion error returns 502
	s.deps.RemoteMedia = stub
	makeSession("fail-conv", errors.New("transcode failed"))
	if resp := callStream("fail-conv"); resp.Code != http.StatusBadGateway {
		t.Fatalf("failed conversion was not reported: %d", resp.Code)
	}

	// Media busy returns 429
	makeSession("busy-conv", media.ErrBusy)
	if resp := callStream("busy-conv"); resp.Code != http.StatusTooManyRequests {
		t.Fatalf("busy media returned %d, want 429", resp.Code)
	}
}

func TestHybridStreamCancellationAndBounds(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	stub := &hybridRemoteStub{}
	s.deps.RemoteMedia = stub

	// 1. Session cancellation removes spool file
	sessCtx, sessCancel := context.WithCancel(context.Background())
	s.sessions["cancel-test"] = &session{
		mode: "HYBRID", ticket: "ticket-cancel", expires: time.Now().Add(time.Minute),
		ctx: sessCtx, cancel: sessCancel,
		source: domain.Source{URL: "https://media.test/cancel", MIME: "video/mp4"},
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/streams/cancel-test?ticket=ticket-cancel", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("stream prepare: %d", w.Code)
	}
	spoolPath := s.sessions["cancel-test"].hybridSpool.filePath()
	if _, err := os.Stat(spoolPath); err != nil {
		t.Fatalf("spool file does not exist before cancel: %v", err)
	}
	sessCancel()
	// context.AfterFunc runs concurrently
	time.Sleep(10 * time.Millisecond)
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Fatalf("spool file was not cleaned up after session cancellation: %v", err)
	}

	// 2. Storage limit bounded writer
	var buf bytes.Buffer
	bounded := &boundedSpoolWriter{w: &buf, limit: 10}
	if _, err := bounded.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.Write([]byte("123456")); err == nil {
		t.Fatal("expected spool_limit_exceeded error")
	}
}

func TestHybridPlaybackPlanIsSeekable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Movie.mkv"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, nil, dir)
	s.deps.Media = &preferredAudioMedia{metadata: domain.Metadata{Streams: []domain.Stream{
		{Index: 0, Type: "video", Codec: "h264", Width: 640, Height: 360},
		{Index: 1, Type: "audio", Codec: "ac3"},
	}}}
	owner := pair(t, s, "hybrid-device")
	setDevicePassingProbes(t, s, "hybrid-device", "http-fmp4", "h264-baseline-360", "aac")
	sources, err := providers.Local(dir)
	if err != nil || len(sources) != 1 {
		t.Fatalf("local source: %v %+v", err, sources)
	}
	request, err := json.Marshal(map[string]string{"itemId": sources[0].Item.ID})
	if err != nil {
		t.Fatal(err)
	}
	response := call(s, http.MethodPost, "/v1/playback", string(request), "hybrid-device", owner, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("plan: %d %s", response.Code, response.Body.String())
	}
	var plan domain.Plan
	if err := json.Unmarshal(response.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "HYBRID" || !plan.PrepareBeforePlayback || plan.MIME != "video/mp4" || !plan.Seekable || plan.ResumeMS != 0 {
		t.Fatalf("invalid hybrid plan: %+v", plan)
	}
}

func TestHybridIsAutomaticResponseOnlyAndReceiverCanStartIt(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	owner := pair(t, s, "hybrid-receiver")
	setDevicePassingProbes(t, s, "hybrid-receiver", "http-fmp4", "h264-baseline-360", "aac")
	s.deps.RemoteMedia = &preferredAudioMedia{metadata: domain.Metadata{Streams: []domain.Stream{
		{Index: 0, Type: "video", Codec: "h264", Profile: "Baseline", Width: 640, Height: 360},
		{Index: 1, Type: "audio", Codec: "ac3"},
	}}}
	request := call(s, http.MethodPost, "/v1/playback", `{"itemId":"movie","mode":"HYBRID"}`, "hybrid-receiver", owner, "")
	if request.Code != http.StatusBadRequest {
		t.Fatalf("client forced internal mode: %d %s", request.Code, request.Body.String())
	}
	plan, err := (receiverAdapter{server: s}).Start(t.Context(), "hybrid-receiver", providers.Source{
		URL: "https://media.test/movie.mkv", MIME: "video/x-matroska",
		Item: domain.Item{ID: "movie", Kind: "video", Playable: true},
	})
	if err != nil || plan.Mode != "HYBRID" || !plan.PrepareBeforePlayback || plan.MIME != "video/mp4" || !plan.Live || plan.Seekable {
		t.Fatalf("receiver hybrid plan: %+v %v", plan, err)
	}
}

func TestRealFFmpegHybridStreamDelivery(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not available")
	}

	mediaDir := t.TempDir()
	fixtureFile := filepath.Join(mediaDir, "test-hybrid.mkv")

	// Use fixture if already available, otherwise create short synthetic H.264 High 720p + AC3 MKV
	qaFixture := "/home/diego/zombie-tv-project/.codex-slaves/zombie-hybrid-qa-20260925.mkv"
	if data, readErr := os.ReadFile(qaFixture); readErr == nil {
		if writeErr := os.WriteFile(fixtureFile, data, 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
	} else {
		cmd := exec.Command(ffmpeg, "-nostdin", "-v", "error",
			"-f", "lavfi", "-i", "color=c=green:s=1280x720:r=24",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100",
			"-t", "0.5", "-c:v", "libx264", "-profile:v", "high", "-threads", "1",
			"-c:a", "ac3", fixtureFile)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create fixture: %v %s", err, out)
		}
	}

	s := testServer(t, nil, mediaDir)
	s.deps.Media = media.New(ffmpeg, ffprobe)

	token := pair(t, s, "vizio-test-device")
	setDevicePassingProbes(t, s, "vizio-test-device", "http-fmp4", "h264-720-high", "aac")

	sources, err := providers.Local(mediaDir)
	if err != nil || len(sources) != 1 {
		t.Fatalf("load local source: %v %+v", err, sources)
	}

	// 1. POST /v1/playback planning selects HYBRID
	reqBody, _ := json.Marshal(map[string]string{"itemId": sources[0].Item.ID})
	resp := call(s, http.MethodPost, "/v1/playback", string(reqBody), "vizio-test-device", token, "")
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", resp.Code, resp.Body.String())
	}
	var plan domain.Plan
	if err := json.Unmarshal(resp.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "HYBRID" {
		t.Fatalf("expected HYBRID mode, got %s", plan.Mode)
	}
	if !plan.Seekable {
		t.Fatal("expected plan.Seekable to be true for non-live local movie")
	}

	// 2. GET /v1/streams/<sessionId>?ticket=<ticket>
	streamReq := httptest.NewRequest(http.MethodGet, plan.URL, nil)
	streamRec := httptest.NewRecorder()
	s.ServeHTTP(streamRec, streamReq)

	if streamRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from stream, got %d", streamRec.Code)
	}
	if streamRec.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("expected video/mp4, got %q", streamRec.Header().Get("Content-Type"))
	}
	contentLengthStr := streamRec.Header().Get("Content-Length")
	if contentLengthStr == "" {
		t.Fatal("expected Content-Length header for Vizio AVAPIMediaPlayer")
	}
	contentLength, err := strconv.ParseInt(contentLengthStr, 10, 64)
	if err != nil || contentLength <= 0 {
		t.Fatalf("invalid Content-Length: %q", contentLengthStr)
	}
	if int64(streamRec.Body.Len()) != contentLength {
		t.Fatalf("body length %d != Content-Length %d", streamRec.Body.Len(), contentLength)
	}
	if streamRec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("expected Accept-Ranges: bytes, got %q", streamRec.Header().Get("Accept-Ranges"))
	}
	if streamRec.Header().Get("Transfer-Encoding") != "" {
		t.Fatalf("unexpected Transfer-Encoding: %q", streamRec.Header().Get("Transfer-Encoding"))
	}

	// 3. Probe delivered media with ffprobe to verify H.264 video was copied and AC3 was transcoded to AAC
	deliveredPath := filepath.Join(t.TempDir(), "delivered.mp4")
	if err := os.WriteFile(deliveredPath, streamRec.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	probeMeta, err := s.deps.Media.Probe(t.Context(), deliveredPath)
	if err != nil {
		t.Fatalf("probe delivered media: %v", err)
	}
	if len(probeMeta.Streams) != 2 {
		t.Fatalf("expected 2 streams, got %d: %+v", len(probeMeta.Streams), probeMeta.Streams)
	}
	if probeMeta.Streams[0].Codec != "h264" || !strings.Contains(strings.ToLower(probeMeta.Streams[0].Profile), "high") {
		t.Fatalf("expected H.264 High video stream, got: %+v", probeMeta.Streams[0])
	}
	if probeMeta.Streams[1].Codec != "aac" {
		t.Fatalf("expected AAC audio stream, got: %+v", probeMeta.Streams[1])
	}

	// 4. Test Range request on the delivered stream
	rangeReq := httptest.NewRequest(http.MethodGet, plan.URL, nil)
	rangeReq.Header.Set("Range", "bytes=100-200")
	rangeRec := httptest.NewRecorder()
	s.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d", rangeRec.Code)
	}
	if rangeRec.Body.Len() != 101 {
		t.Fatalf("expected 101 bytes for range 100-200, got %d", rangeRec.Body.Len())
	}
	if rangeRec.Header().Get("Content-Range") != "bytes 100-200/"+contentLengthStr {
		t.Fatalf("unexpected Content-Range: %q", rangeRec.Header().Get("Content-Range"))
	}

	// 5. Cleanup: verify spool is removed after server is closed
	spoolFile := s.sessions[plan.SessionID].hybridSpool.filePath()
	if _, err := os.Stat(spoolFile); err != nil {
		t.Fatalf("spool file missing while session active: %v", err)
	}
	s.Close()
	time.Sleep(10 * time.Millisecond)
	if _, err := os.Stat(spoolFile); !os.IsNotExist(err) {
		t.Fatalf("spool file %s not removed after server.Close()", spoolFile)
	}
}

func TestHybridSpoolIsolationAndCrashCleanup(t *testing.T) {
	baseDir := t.TempDir()

	// 1. Instance 1 starts in baseDir
	s1 := testServer(t, nil, "")
	s1.opt.HybridSpoolDir = baseDir
	s1.hybridSpools.Close()
	s1.hybridSpools = newHybridSpoolManager(s1.opt)

	// Write a file in instance 1's private dir
	file1 := filepath.Join(s1.hybridSpools.instanceDir, "active1.mp4")
	if err := os.WriteFile(file1, []byte("data1"), 0600); err != nil {
		t.Fatal(err)
	}

	// 2. Instance 2 starts in the same baseDir
	s2 := testServer(t, nil, "")
	s2.opt.HybridSpoolDir = baseDir
	s2.hybridSpools.Close()
	s2.hybridSpools = newHybridSpoolManager(s2.opt)

	// Verify instance 2 did NOT touch instance 1's active file
	if _, err := os.Stat(file1); err != nil {
		t.Fatalf("instance 2 startup destroyed instance 1 spool file: %v", err)
	}

	// Write a file in instance 2's private dir
	file2 := filepath.Join(s2.hybridSpools.instanceDir, "active2.mp4")
	if err := os.WriteFile(file2, []byte("data2"), 0600); err != nil {
		t.Fatal(err)
	}

	// 3. Simulate Instance 1 crash: unlock lockFile without deleting instanceDir
	inst1Dir := s1.hybridSpools.instanceDir
	_ = syscall.Flock(int(s1.hybridSpools.lockFile.Fd()), syscall.LOCK_UN)
	_ = s1.hybridSpools.lockFile.Close()

	// 4. Instance 3 starts in baseDir: should detect instance 1 as stale/crashed and clean it,
	// while leaving instance 2 completely untouched!
	s3 := testServer(t, nil, "")
	s3.opt.HybridSpoolDir = baseDir
	s3.hybridSpools.Close()
	s3.hybridSpools = newHybridSpoolManager(s3.opt)

	// Verify instance 1's orphaned dir is now cleaned up
	if _, err := os.Stat(inst1Dir); !os.IsNotExist(err) {
		t.Fatalf("crashed instance 1 dir was not cleaned up by instance 3: %v", err)
	}

	// Verify instance 2's active file STILL exists!
	if _, err := os.Stat(file2); err != nil {
		t.Fatalf("instance 2 spool file missing after instance 3 startup: %v", err)
	}
}

func TestHybridAggregateQuotaAndRetention(t *testing.T) {
	baseDir := t.TempDir()
	s := testServer(t, nil, "")
	s.opt.HybridSpoolDir = baseDir
	s.opt.HybridMaxSpoolBytes = 1000
	s.opt.HybridAggregateQuotaBytes = 2500
	s.opt.HybridMaxConversions = 4
	s.hybridSpools.Close()
	s.hybridSpools = newHybridSpoolManager(s.opt)

	makeSession := func(id string) {
		s.sessions[id] = &session{
			mode: "HYBRID", ticket: "ticket-" + id, expires: time.Now().Add(time.Minute),
			ctx: t.Context(), cancel: func() {},
			source: domain.Source{URL: "https://media.test/" + id, MIME: "video/mp4"},
		}
	}
	getStream := func(id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/streams/"+id+"?ticket=ticket-"+id, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	// Session A: 800 bytes
	makeSession("sess-a")
	stubA := &hybridPayloadStub{payload: strings.Repeat("A", 800)}
	s.deps.RemoteMedia = stubA
	respA := getStream("sess-a")
	if respA.Code != http.StatusOK {
		t.Fatalf("sess-a failed: %d", respA.Code)
	}
	spoolA := s.sessions["sess-a"].hybridSpool
	if spoolA == nil || spoolA.size != 800 {
		t.Fatalf("sess-a spool wrong size: %+v", spoolA)
	}

	// Session B: 800 bytes
	makeSession("sess-b")
	stubB := &hybridPayloadStub{payload: strings.Repeat("B", 800)}
	s.deps.RemoteMedia = stubB
	respB := getStream("sess-b")
	if respB.Code != http.StatusOK {
		t.Fatalf("sess-b failed: %d", respB.Code)
	}
	spoolB := s.sessions["sess-b"].hybridSpool

	// Total used so far: 800 + 800 = 1600 bytes.
	// Next request needs 1000 reservation (1600 + 1000 = 2600 > 2500).
	// Neither A nor B is currently being read. A is older (LRU), so A should be evicted.
	makeSession("sess-c")
	stubC := &hybridPayloadStub{payload: strings.Repeat("C", 800)}
	s.deps.RemoteMedia = stubC
	respC := getStream("sess-c")
	if respC.Code != http.StatusOK {
		t.Fatalf("sess-c failed: %d %s", respC.Code, respC.Body.String())
	}
	// Verify spool A was evicted (path cleaned)
	spoolA.mu.Lock()
	cleanedA := spoolA.cleaned
	spoolA.mu.Unlock()
	if !cleanedA {
		t.Fatal("sess-a was not evicted by LRU retention when quota was needed")
	}

	// Now hold an active reader on spool B
	if err := spoolB.acquireReader(); err != nil {
		t.Fatal(err)
	}
	defer spoolB.releaseReader()

	// Total used: B (800) + C (800) = 1600.
	// Now allocate Session D (needs 1000 reservation).
	// B has active reader (cannot be evicted!).
	// C is idle (can be evicted!).
	makeSession("sess-d")
	stubD := &hybridPayloadStub{payload: strings.Repeat("D", 800)}
	s.deps.RemoteMedia = stubD
	respD := getStream("sess-d")
	if respD.Code != http.StatusOK {
		t.Fatalf("sess-d failed: %d %s", respD.Code, respD.Body.String())
	}
	spoolD := s.sessions["sess-d"].hybridSpool

	// Now hold active reader on spool D as well!
	if err := spoolD.acquireReader(); err != nil {
		t.Fatal(err)
	}
	defer spoolD.releaseReader()

	// Total used: B (800) + D (800) = 1600.
	// Try to start Session E (needs 1000 reservation -> 2600 > 2500).
	// BOTH B and D have active readers! Neither can be evicted!
	// Session E MUST fail with 507 spool_quota_exceeded!
	makeSession("sess-e")
	stubE := &hybridPayloadStub{payload: strings.Repeat("E", 800)}
	s.deps.RemoteMedia = stubE
	respE := getStream("sess-e")
	if respE.Code != 507 {
		t.Fatalf("expected 507 spool_quota_exceeded when all quota is locked by active readers, got: %d %s", respE.Code, respE.Body.String())
	}

	// Verify range reads on Session B and Session D still work!
	rangeB := httptest.NewRequest(http.MethodGet, "/v1/streams/sess-b?ticket=ticket-sess-b", nil)
	rangeB.Header.Set("Range", "bytes=10-20")
	recB := httptest.NewRecorder()
	s.ServeHTTP(recB, rangeB)
	if recB.Code != http.StatusPartialContent {
		t.Fatalf("range read on protected active session B failed: %d", recB.Code)
	}
}

type hybridPayloadStub struct {
	remoteMediaStub
	payload string
}

func (s *hybridPayloadStub) ConvertRemote(_ context.Context, _ domain.Source, _ string, _ domain.MediaSelection, output io.Writer) error {
	_, err := io.WriteString(output, s.payload)
	return err
}

type hybridBarrierRemoteStub struct {
	remoteMediaStub
	entered         chan struct{}
	release         chan struct{}
	cancelObserved  chan struct{}
	cancelAware     bool
	holdAfterCancel bool
	payload         string
}

func (stub *hybridBarrierRemoteStub) ConvertRemote(ctx context.Context, _ domain.Source, _ string, _ domain.MediaSelection, output io.Writer) error {
	if stub.payload != "" {
		if _, err := io.WriteString(output, stub.payload); err != nil {
			return err
		}
	}
	close(stub.entered)
	if stub.cancelAware {
		select {
		case <-ctx.Done():
			close(stub.cancelObserved)
			if stub.holdAfterCancel {
				<-stub.release
			}
			return ctx.Err()
		case <-stub.release:
			return nil
		}
	}
	<-stub.release
	return nil
}

func newHybridLifecycleFixture(t *testing.T, remote RemoteMedia) (*Server, *session, string) {
	t.Helper()
	baseDir := t.TempDir()
	s := testServer(t, nil, "")
	s.hybridSpools.Close()
	s.opt.HybridSpoolDir = baseDir
	s.hybridSpools = newHybridSpoolManager(s.opt)
	if s.hybridSpools.initErr != nil {
		t.Fatalf("initialize spool manager: %v", s.hybridSpools.initErr)
	}
	s.deps.RemoteMedia = remote
	sess := &session{
		mode: "HYBRID", ticket: "lifecycle-ticket", expires: time.Now().Add(time.Minute),
		ctx: context.Background(), cancel: func() {},
		source: domain.Source{URL: "https://media.test/lifecycle", MIME: "video/mp4"},
	}
	return s, sess, s.hybridSpools.instanceDir
}

func awaitHybridManagerClosed(t *testing.T, manager *hybridSpoolManager) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		manager.mu.Lock()
		closed := manager.closed
		manager.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("spool manager Close did not close worker admission")
		case <-ticker.C:
		}
	}
}

func assertHybridManagerDrained(t *testing.T, manager *hybridSpoolManager, instanceDir string, spool *hybridSpool) {
	t.Helper()
	if spool != nil {
		select {
		case <-spool.done:
		default:
			t.Fatal("spool worker is still running after Close returned")
		}
	}
	manager.mu.Lock()
	activeConvs := manager.activeConvs
	spoolCount := len(manager.spools)
	manager.mu.Unlock()
	if activeConvs != 0 || spoolCount != 0 {
		t.Fatalf("spool manager leaked state after Close: active conversions=%d spools=%d", activeConvs, spoolCount)
	}
	if _, err := os.Stat(instanceDir); !os.IsNotExist(err) {
		t.Fatalf("instance directory remains after Close: %v", err)
	}
}

func TestHybridCloseBeforeSpoolCreationJoinsAndRejectsAdmission(t *testing.T) {
	s, sess, instanceDir := newHybridLifecycleFixture(t, &hybridPayloadStub{payload: "never-created"})
	manager := s.hybridSpools
	workerReady := make(chan struct{})
	allowCreate := make(chan struct{})
	workerDone := make(chan struct{})
	spool, err := manager.acquireSpool(sess, func(ctx context.Context, sess *session) {
		close(workerReady)
		<-allowCreate
		s.runHybridSpool(ctx, sess)
		close(workerDone)
	})
	if err != nil {
		t.Fatal(err)
	}
	<-workerReady

	closeResults := make(chan struct{}, 4)
	for i := 0; i < cap(closeResults); i++ {
		go func() {
			manager.Close()
			closeResults <- struct{}{}
		}()
	}
	awaitHybridManagerClosed(t, manager)

	entries, err := os.ReadDir(instanceDir)
	if err != nil {
		t.Fatalf("read spool instance before releasing worker: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatalf("spool file was created before the worker barrier released: %+v", entries)
	}
	close(allowCreate)
	<-workerDone
	for i := 0; i < cap(closeResults); i++ {
		select {
		case <-closeResults:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Close call did not return")
		}
	}
	assertHybridManagerDrained(t, manager, instanceDir, spool)
	if path := spool.filePath(); path != "" {
		t.Fatalf("spool path remained after close-before-create: %q", path)
	}
}

func TestHybridCloseDuringConversionCancelsAndJoinsWorker(t *testing.T) {
	remote := &hybridBarrierRemoteStub{
		entered: make(chan struct{}), release: make(chan struct{}),
		cancelObserved: make(chan struct{}), cancelAware: true, holdAfterCancel: true, payload: "partial",
	}
	defer func() {
		select {
		case <-remote.release:
		default:
			close(remote.release)
		}
	}()
	s, sess, instanceDir := newHybridLifecycleFixture(t, remote)
	s.sessions["close-during-conversion"] = sess
	manager := s.hybridSpools
	requestDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, "/v1/streams/close-during-conversion?ticket=lifecycle-ticket", nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		requestDone <- w
	}()
	<-remote.entered

	spool := sess.hybridSpool
	spoolPath := spool.filePath()
	if spoolPath == "" {
		t.Fatal("conversion did not publish its spool path before starting")
	}
	if info, err := os.Stat(spoolPath); err != nil || info.Size() != int64(len(remote.payload)) {
		t.Fatalf("expected partial spool during conversion, info=%v err=%v", info, err)
	}

	closeDone := make(chan struct{})
	go func() {
		manager.Close()
		close(closeDone)
	}()
	awaitHybridManagerClosed(t, manager)
	select {
	case <-remote.cancelObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("manager Close did not cancel the conversion context")
	}
	select {
	case <-closeDone:
		t.Fatal("manager Close returned before the active converter terminated")
	default:
	}
	lockPath := filepath.Join(instanceDir, ".lock")
	lockFD, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatalf("instance lock disappeared before the worker terminated: %v", err)
	}
	probeLock := os.NewFile(uintptr(lockFD), lockPath)
	flockErr := syscall.Flock(int(probeLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if flockErr == nil {
		_ = syscall.Flock(int(probeLock.Fd()), syscall.LOCK_UN)
		_ = probeLock.Close()
		t.Fatal("manager released the instance lock before the worker terminated")
	}
	_ = probeLock.Close()
	close(remote.release)
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("manager Close did not join the canceled conversion")
	}
	response := <-requestDone
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("canceled conversion returned %d, want 504", response.Code)
	}
	assertHybridManagerDrained(t, manager, instanceDir, spool)
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Fatalf("partial spool remains after canceled conversion: %v", err)
	}
}

func TestHybridCloseAfterConversionCompletionIsConcurrentAndIdempotent(t *testing.T) {
	remote := &hybridBarrierRemoteStub{
		entered: make(chan struct{}), release: make(chan struct{}), payload: "completed-media",
	}
	s, sess, instanceDir := newHybridLifecycleFixture(t, remote)
	s.sessions["close-after-completion"] = sess
	manager := s.hybridSpools
	requestDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, "/v1/streams/close-after-completion?ticket=lifecycle-ticket", nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		requestDone <- w
	}()
	<-remote.entered
	close(remote.release)
	response := <-requestDone
	if response.Code != http.StatusOK || response.Body.String() != remote.payload {
		t.Fatalf("completed conversion response: status=%d body=%q", response.Code, response.Body.String())
	}

	spool := sess.hybridSpool
	spoolPath := spool.filePath()
	if _, err := os.Stat(spoolPath); err != nil {
		t.Fatalf("completed spool missing before Close: %v", err)
	}
	closeResults := make(chan struct{}, 4)
	for i := 0; i < cap(closeResults); i++ {
		go func() {
			manager.Close()
			closeResults <- struct{}{}
		}()
	}
	for i := 0; i < cap(closeResults); i++ {
		select {
		case <-closeResults:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent idempotent Close call did not return")
		}
	}
	assertHybridManagerDrained(t, manager, instanceDir, spool)
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Fatalf("completed spool remains after Close: %v", err)
	}
}

func TestHybridClosePreservesReplacedInstancePath(t *testing.T) {
	s, _, instanceDir := newHybridLifecycleFixture(t, &hybridPayloadStub{payload: "unused"})
	manager := s.hybridSpools
	movedDir := instanceDir + "-moved"
	if err := os.Rename(instanceDir, movedDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(instanceDir, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(instanceDir, "preserve.txt")
	if err := os.WriteFile(sentinel, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("Close removed or changed the replacement instance path: data=%q err=%v", data, err)
	}
}

func TestHybridClosePreservesReplacedBasePath(t *testing.T) {
	s, _, instanceDir := newHybridLifecycleFixture(t, &hybridPayloadStub{payload: "unused"})
	manager := s.hybridSpools
	baseDir := filepath.Dir(instanceDir)
	movedBase := filepath.Join(filepath.Dir(baseDir), "moved-"+filepath.Base(baseDir))
	t.Cleanup(func() { _ = os.RemoveAll(movedBase) })
	if err := os.Rename(baseDir, movedBase); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(baseDir, 0700); err != nil {
		t.Fatal(err)
	}
	replacementInstance := filepath.Join(baseDir, filepath.Base(instanceDir))
	if err := os.Mkdir(replacementInstance, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(replacementInstance, "preserve.txt")
	if err := os.WriteFile(sentinel, []byte("replacement parent"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "replacement parent" {
		t.Fatalf("Close removed or changed the replacement base path: data=%q err=%v", data, err)
	}
}

func TestHybridConcurrencyLimit(t *testing.T) {
	s := testServer(t, nil, "")
	s.opt.HybridMaxConversions = 1
	s.hybridSpools.Close()
	s.hybridSpools = newHybridSpoolManager(s.opt)

	stub := &hybridRemoteStub{delay: 100 * time.Millisecond}
	s.deps.RemoteMedia = stub

	s.sessions["conv1"] = &session{
		mode: "HYBRID", ticket: "t1", expires: time.Now().Add(time.Minute),
		ctx: t.Context(), cancel: func() {},
		source: domain.Source{URL: "https://media.test/1", MIME: "video/mp4"},
	}
	s.sessions["conv2"] = &session{
		mode: "HYBRID", ticket: "t2", expires: time.Now().Add(time.Minute),
		ctx: t.Context(), cancel: func() {},
		source: domain.Source{URL: "https://media.test/2", MIME: "video/mp4"},
	}

	done := make(chan struct{})
	go func() {
		r1 := httptest.NewRequest(http.MethodGet, "/v1/streams/conv1?ticket=t1", nil)
		w1 := httptest.NewRecorder()
		s.ServeHTTP(w1, r1)
		close(done)
	}()

	// Small pause so conv1 starts and acquires the single conversion slot
	time.Sleep(10 * time.Millisecond)

	r2 := httptest.NewRequest(http.MethodGet, "/v1/streams/conv2?ticket=t2", nil)
	w2 := httptest.NewRecorder()
	s.ServeHTTP(w2, r2)

	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 media_busy when max concurrent conversions reached, got: %d %s", w2.Code, w2.Body.String())
	}
	<-done
}

type lazyTestMedia struct {
	preferredAudioMedia
}

func (m *lazyTestMedia) ConvertSelected(_ context.Context, _ string, _ string, _ domain.MediaSelection, output io.Writer) error {
	_, err := io.WriteString(output, "hybrid-content")
	return err
}

func TestHybridLazyConversion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Movie.mkv"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, nil, dir)
	lazyMedia := &lazyTestMedia{
		preferredAudioMedia: preferredAudioMedia{metadata: domain.Metadata{Streams: []domain.Stream{
			{Index: 0, Type: "video", Codec: "h264", Width: 640, Height: 360},
			{Index: 1, Type: "audio", Codec: "ac3"},
		}}},
	}
	s.deps.Media = lazyMedia
	owner := pair(t, s, "lazy-device")
	setDevicePassingProbes(t, s, "lazy-device", "http-fmp4", "h264-baseline-360", "aac")
	sources, err := providers.Local(dir)
	if err != nil || len(sources) != 1 {
		t.Fatalf("local source: %v %+v", err, sources)
	}

	// 1. POST /v1/playback plans HYBRID
	reqBody, _ := json.Marshal(map[string]string{"itemId": sources[0].Item.ID})
	resp := call(s, http.MethodPost, "/v1/playback", string(reqBody), "lazy-device", owner, "")
	if resp.Code != http.StatusCreated {
		t.Fatalf("plan: %d %s", resp.Code, resp.Body.String())
	}
	var plan domain.Plan
	if err := json.Unmarshal(resp.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "HYBRID" {
		t.Fatalf("expected HYBRID mode, got: %s", plan.Mode)
	}

	// Verify that NO conversion was eagerly started!
	sess := s.sessions[plan.SessionID]
	if sess == nil {
		t.Fatal("session not found")
	}
	if sess.hybridSpool != nil {
		t.Fatal("expected hybridSpool to be nil before stream is fetched (must be lazy)")
	}

	// 2. Now fetch the stream URL: should start conversion lazily
	streamReq := httptest.NewRequest(http.MethodGet, plan.URL, nil)
	streamRec := httptest.NewRecorder()
	s.ServeHTTP(streamRec, streamReq)

	if streamRec.Code != http.StatusOK {
		t.Fatalf("stream failed: %d %s", streamRec.Code, streamRec.Body.String())
	}
	if sess.hybridSpool == nil {
		t.Fatal("expected hybridSpool to be initialized after stream request")
	}
}

func TestHybridCleanupOnSessionExpiry(t *testing.T) {
	s := testServer(t, nil, "")
	stub := &hybridPayloadStub{payload: "temp-data"}
	s.deps.RemoteMedia = stub

	// Create a session whose deadline expires in 50 milliseconds
	expiry := time.Now().Add(50 * time.Millisecond)
	sessCtx, cancel := context.WithDeadline(context.Background(), expiry)
	defer cancel()

	s.sessions["expiring"] = &session{
		mode: "HYBRID", ticket: "t-exp", expires: expiry,
		ctx: sessCtx, cancel: cancel,
		source: domain.Source{URL: "https://media.test/exp", MIME: "video/mp4"},
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/streams/expiring?ticket=t-exp", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("stream failed: %d", w.Code)
	}
	spoolPath := s.sessions["expiring"].hybridSpool.filePath()
	if _, err := os.Stat(spoolPath); err != nil {
		t.Fatalf("spool file does not exist: %v", err)
	}

	// Wait for context deadline to fire
	time.Sleep(100 * time.Millisecond)

	// Spool file should have been cleaned up automatically by context.AfterFunc(sess.ctx)
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Fatalf("spool file was not cleaned up on session expiry: %v", err)
	}
}

func TestHybridSpoolBaseDirSecurityAndFailClosed(t *testing.T) {
	// 1. Never chmod an existing configured base directory
	dir := t.TempDir()
	customDir := filepath.Join(dir, "custom-spool-dir")
	if err := os.Mkdir(customDir, 0755); err != nil {
		t.Fatal(err)
	}
	fiBefore, err := os.Lstat(customDir)
	if err != nil {
		t.Fatal(err)
	}

	s := testServer(t, nil, "")
	s.opt.HybridSpoolDir = customDir
	s.hybridSpools.Close()
	s.hybridSpools = newHybridSpoolManager(s.opt)
	if s.hybridSpools.initErr != nil {
		t.Fatalf("unexpected init error: %v", s.hybridSpools.initErr)
	}

	fiAfter, err := os.Lstat(customDir)
	if err != nil {
		t.Fatal(err)
	}
	if fiAfter.Mode().Perm() != fiBefore.Mode().Perm() {
		t.Fatalf("base dir permissions were mutated: before=%o after=%o", fiBefore.Mode().Perm(), fiAfter.Mode().Perm())
	}

	// 2. Reject symlink base directories and fail closed
	linkDir := filepath.Join(t.TempDir(), "symlink-spools")
	if err := os.Symlink(customDir, linkDir); err != nil {
		t.Fatal(err)
	}

	sSym := testServer(t, nil, "")
	sSym.deps.RemoteMedia = &hybridRemoteStub{}
	sSym.opt.HybridSpoolDir = linkDir
	sSym.hybridSpools.Close()
	sSym.hybridSpools = newHybridSpoolManager(sSym.opt)
	if sSym.hybridSpools.initErr == nil {
		t.Fatal("expected initErr on symlink base dir, got nil")
	}

	sessCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sSym.sessions["sym-sess"] = &session{
		mode: "HYBRID", ticket: "t-sym", expires: time.Now().Add(time.Minute),
		ctx: sessCtx, cancel: cancel,
		source: domain.Source{URL: "https://media.test/sym", MIME: "video/mp4"},
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/streams/sym-sess?ticket=t-sym", nil)
	w := httptest.NewRecorder()
	sSym.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "spool_storage_unavailable") {
		t.Fatalf("expected 500 spool_storage_unavailable, got %d %s", w.Code, w.Body.String())
	}
}

func TestHybridCrashCleanupPreservesMissingOrUnreadableLock(t *testing.T) {
	baseDir := t.TempDir()

	// 1. Directory with missing .lock must NOT be removed
	noLockDir := filepath.Join(baseDir, "instance-nolock")
	if err := os.Mkdir(noLockDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noLockDir, "data.txt"), []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}

	// 2. Directory with unreadable .lock must NOT be removed
	unreadableLockDir := filepath.Join(baseDir, "instance-unreadable")
	if err := os.Mkdir(unreadableLockDir, 0700); err != nil {
		t.Fatal(err)
	}
	unreadableLockFile := filepath.Join(unreadableLockDir, ".lock")
	if err := os.WriteFile(unreadableLockFile, []byte("locked"), 0000); err != nil {
		t.Fatal(err)
	}

	// 3. Directory with valid .lock whose flock can be acquired (abandoned) SHOULD be cleaned up
	crashedDir := filepath.Join(baseDir, "instance-crashed")
	if err := os.Mkdir(crashedDir, 0700); err != nil {
		t.Fatal(err)
	}
	crashedLock := filepath.Join(crashedDir, ".lock")
	if err := os.WriteFile(crashedLock, []byte("crashed"), 0600); err != nil {
		t.Fatal(err)
	}

	// Run cleanup
	cleanStaleHybridSpools(baseDir)

	// Verify missing .lock was PRESERVED
	if _, err := os.Stat(noLockDir); os.IsNotExist(err) {
		t.Fatal("instance directory without .lock was erroneously deleted")
	}

	// Verify unreadable .lock was PRESERVED
	if _, err := os.Stat(unreadableLockDir); os.IsNotExist(err) {
		t.Fatal("instance directory with unreadable .lock was erroneously deleted")
	}

	// Verify crashed directory with valid unlocked .lock WAS removed
	if _, err := os.Stat(crashedDir); !os.IsNotExist(err) {
		t.Fatal("crashed directory was not cleaned up")
	}
}

func TestHybridConcurrentConversionsAndCancellationsRace(t *testing.T) {
	baseDir := t.TempDir()
	s := testServer(t, nil, "")
	s.opt.HybridSpoolDir = baseDir
	s.opt.HybridMaxConversions = 4
	s.opt.HybridMaxSpoolBytes = 2000
	s.opt.HybridAggregateQuotaBytes = 8000
	s.hybridSpools.Close()
	s.hybridSpools = newHybridSpoolManager(s.opt)

	stub := &hybridRemoteStub{delay: 5 * time.Millisecond}
	s.deps.RemoteMedia = stub

	const numSessions = 12
	var wg sync.WaitGroup

	for i := 0; i < numSessions; i++ {
		id := fmt.Sprintf("race-sess-%d", i)
		sessCtx, sessCancel := context.WithCancel(context.Background())
		s.sessions[id] = &session{
			mode: "HYBRID", ticket: "t-" + id, expires: time.Now().Add(time.Minute),
			ctx: sessCtx, cancel: sessCancel,
			source: domain.Source{URL: "https://media.test/" + id, MIME: "video/mp4"},
		}
	}

	for i := 0; i < numSessions; i++ {
		wg.Add(1)
		id := fmt.Sprintf("race-sess-%d", i)
		cancelFn := s.sessions[id].cancel
		go func(sessID string, cancelFn context.CancelFunc, idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/v1/streams/"+sessID+"?ticket=t-"+sessID, nil)
			rec := httptest.NewRecorder()

			// Concurrently cancel half of the sessions after a slight jitter
			if idx%2 == 1 {
				time.AfterFunc(time.Duration(idx*2)*time.Millisecond, cancelFn)
			}

			s.ServeHTTP(rec, req)

			// Concurrently try range requests on successful responses
			if rec.Code == http.StatusOK {
				rangeReq := httptest.NewRequest(http.MethodGet, "/v1/streams/"+sessID+"?ticket=t-"+sessID, nil)
				rangeReq.Header.Set("Range", "bytes=0-4")
				rangeRec := httptest.NewRecorder()
				s.ServeHTTP(rangeRec, rangeReq)
			}
		}(id, cancelFn, i)
	}

	wg.Wait()

	// Clean up all sessions
	for i := 0; i < numSessions; i++ {
		id := fmt.Sprintf("race-sess-%d", i)
		s.sessions[id].cancel()
	}
	time.Sleep(20 * time.Millisecond)

	s.hybridSpools.mu.Lock()
	activeConvs := s.hybridSpools.activeConvs
	s.hybridSpools.mu.Unlock()

	if activeConvs != 0 {
		t.Fatalf("active conversion slots leaked: got %d, want 0", activeConvs)
	}
}

func TestHybridTransientAllocationRetry(t *testing.T) {
	baseDir := t.TempDir()
	s := testServer(t, nil, "")
	s.opt.HybridSpoolDir = baseDir
	s.opt.HybridMaxConversions = 1
	s.opt.HybridMaxSpoolBytes = 500
	s.opt.HybridAggregateQuotaBytes = 1000
	s.hybridSpools.Close()
	s.hybridSpools = newHybridSpoolManager(s.opt)

	stub := &hybridRemoteStub{delay: 40 * time.Millisecond}
	s.deps.RemoteMedia = stub

	// 1. Test Concurrency Limit (429) Retry
	s.sessions["conv-a"] = &session{
		mode: "HYBRID", ticket: "ta", expires: time.Now().Add(time.Minute),
		ctx: t.Context(), cancel: func() {},
		source: domain.Source{URL: "https://media.test/a", MIME: "video/mp4"},
	}
	s.sessions["conv-b"] = &session{
		mode: "HYBRID", ticket: "tb", expires: time.Now().Add(time.Minute),
		ctx: t.Context(), cancel: func() {},
		source: domain.Source{URL: "https://media.test/b", MIME: "video/mp4"},
	}

	doneA := make(chan struct{})
	go func() {
		rA := httptest.NewRequest(http.MethodGet, "/v1/streams/conv-a?ticket=ta", nil)
		wA := httptest.NewRecorder()
		s.ServeHTTP(wA, rA)
		close(doneA)
	}()

	// Wait briefly for conv-a to acquire conversion slot
	time.Sleep(10 * time.Millisecond)

	// conv-b attempts stream while conv-a is active: should fail with 429
	rB1 := httptest.NewRequest(http.MethodGet, "/v1/streams/conv-b?ticket=tb", nil)
	wB1 := httptest.NewRecorder()
	s.ServeHTTP(wB1, rB1)

	if wB1.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 media_busy on first attempt, got %d %s", wB1.Code, wB1.Body.String())
	}

	// Verify conv-b session was NOT permanently poisoned
	s.mu.Lock()
	spoolB := s.sessions["conv-b"].hybridSpool
	s.mu.Unlock()
	if spoolB != nil {
		t.Fatal("expected hybridSpool to remain nil on conv-b after 429 allocation error")
	}

	// Wait for conv-a to finish
	<-doneA

	// Retry conv-b: should now SUCCEED with 200 OK!
	rB2 := httptest.NewRequest(http.MethodGet, "/v1/streams/conv-b?ticket=tb", nil)
	wB2 := httptest.NewRecorder()
	s.ServeHTTP(wB2, rB2)

	if wB2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on retry after concurrency freed, got %d %s", wB2.Code, wB2.Body.String())
	}

	// 2. Test Quota Exhaustion (507) Retry
	// Hold reader on conv-b so it cannot be evicted
	spoolBFinal := s.sessions["conv-b"].hybridSpool
	if err := spoolBFinal.acquireReader(); err != nil {
		t.Fatal(err)
	}
	defer spoolBFinal.releaseReader()

	// Update quota configuration for part 2: MaxSpool = 400, AggregateQuota = 700
	s.hybridSpools.mu.Lock()
	s.hybridSpools.maxSpoolBytes = 400
	s.hybridSpools.aggregateQuota = 700
	s.hybridSpools.mu.Unlock()

	// conv-b is 14 bytes. Add conv-c with 350 bytes (<= 400 maxSpool).
	s.deps.RemoteMedia = &hybridPayloadStub{payload: strings.Repeat("C", 350)}
	s.sessions["conv-c"] = &session{
		mode: "HYBRID", ticket: "tc", expires: time.Now().Add(time.Minute),
		ctx: t.Context(), cancel: func() {},
		source: domain.Source{URL: "https://media.test/c", MIME: "video/mp4"},
	}
	rC := httptest.NewRequest(http.MethodGet, "/v1/streams/conv-c?ticket=tc", nil)
	wC := httptest.NewRecorder()
	s.ServeHTTP(wC, rC)
	if wC.Code != http.StatusOK {
		t.Fatalf("conv-c stream failed: %d %s", wC.Code, wC.Body.String())
	}
	spoolC := s.sessions["conv-c"].hybridSpool
	if err := spoolC.acquireReader(); err != nil {
		t.Fatal(err)
	}

	// Now try conv-d: quota exhausted because B and C have active readers!
	s.sessions["conv-d"] = &session{
		mode: "HYBRID", ticket: "td", expires: time.Now().Add(time.Minute),
		ctx: t.Context(), cancel: func() {},
		source: domain.Source{URL: "https://media.test/d", MIME: "video/mp4"},
	}
	rD1 := httptest.NewRequest(http.MethodGet, "/v1/streams/conv-d?ticket=td", nil)
	wD1 := httptest.NewRecorder()
	s.ServeHTTP(wD1, rD1)

	if wD1.Code != 507 {
		t.Fatalf("expected 507 spool_quota_exceeded, got %d %s", wD1.Code, wD1.Body.String())
	}

	// Verify conv-d session was NOT permanently poisoned
	s.mu.Lock()
	spoolD := s.sessions["conv-d"].hybridSpool
	s.mu.Unlock()
	if spoolD != nil {
		t.Fatal("expected hybridSpool to remain nil on conv-d after 507 allocation error")
	}

	// Release reader on C so it can be evicted
	spoolC.releaseReader()

	// Retry conv-d: C is evicted and conv-d SUCCEEDS with 200 OK!
	rD2 := httptest.NewRequest(http.MethodGet, "/v1/streams/conv-d?ticket=td", nil)
	wD2 := httptest.NewRecorder()
	s.ServeHTTP(wD2, rD2)

	if wD2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on retry after quota freed, got %d %s", wD2.Code, wD2.Body.String())
	}
}
