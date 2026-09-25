package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type progressiveYouTubeHTTP struct {
	content []byte
	ranges  []string
}

func (client *progressiveYouTubeHTTP) Do(request *http.Request) (*http.Response, error) {
	var start, end int
	if _, err := fmt.Sscanf(request.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end-start >= 256<<10 {
		return nil, errors.New("unbounded upstream range")
	}
	client.ranges = append(client.ranges, request.Header.Get("Range"))
	if end >= len(client.content) {
		end = len(client.content) - 1
	}
	return &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{
		"Content-Range": []string{fmt.Sprintf("bytes %d-%d/%d", start, end, len(client.content))},
		"Content-Type":  []string{"video/mp4"},
	}, Body: io.NopCloser(bytes.NewReader(client.content[start : end+1])), Request: request}, nil
}

type remoteMediaStub struct{ err error }

func (stub remoteMediaStub) ProbeRemote(context.Context, domain.Source) (domain.Metadata, error) {
	return domain.Metadata{Streams: []domain.Stream{{Type: "video", Codec: "h264", Width: 640, Height: 360}, {Type: "audio", Codec: "aac"}}}, stub.err
}
func (remoteMediaStub) ConvertRemote(context.Context, domain.Source, string, domain.MediaSelection, io.Writer) error {
	return nil
}

func TestAdaptivePlaybackNeverReturnsVideoOnlyOrIgnoresFailedFragmentProbe(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	s.deps.RemoteMedia = remoteMediaStub{}
	source := domain.Source{URL: "https://video.test/clip", AudioURL: "https://audio.test/clip", MIME: "video/mp4"}
	device := domain.Device{}
	if mode, err := s.playbackMode(context.Background(), source, device, ""); err != nil || mode.mode != "REMUX" {
		t.Fatal(mode, err)
	}
	for _, requested := range []string{"DIRECT_PLAY", "EXTERNAL_PLAYER"} {
		if _, err := s.playbackMode(context.Background(), source, device, requested); err == nil {
			t.Fatal("accepted video-only mode", requested)
		}
	}
	device.Capabilities.Probes = []domain.Probe{{ID: "http-fmp4", Status: "FAIL", TestedAt: time.Now().Unix()}}
	if _, err := s.playbackMode(context.Background(), source, device, ""); err == nil {
		t.Fatal("ignored failed fragment probe")
	}
	s.deps.RemoteMedia = remoteMediaStub{err: errors.New("network unavailable")}
	if _, err := s.playbackMode(context.Background(), source, domain.Device{}, ""); err == nil {
		t.Fatal("missing mux silently fell back")
	}
	source.AudioURL = ""
	if mode, err := s.playbackMode(context.Background(), source, domain.Device{}, ""); err != nil || mode.mode != "DIRECT_PLAY" {
		t.Fatal("network failure treated as decoder failure", mode, err)
	}
}

func TestYouTubeCombinedStreamUsesBoundedGatewayProgressiveRelay(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	s.deps.RemoteMedia = remoteMediaStub{}
	source := domain.Source{Item: domain.Item{Provider: "youtube"}, URL: "https://r1.googlevideo.com/clip", MIME: "video/mp4"}
	device := domain.Device{Capabilities: domain.Capabilities{Probes: []domain.Probe{
		{ID: "http-progressive", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		{ID: "h264-baseline-360", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
		{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: time.Now().Unix()},
	}}}
	if mode, err := s.playbackMode(t.Context(), source, device, "AUTO"); err != nil || mode.mode != "DIRECT_PLAY" {
		t.Fatal(mode, err)
	}
	for _, requested := range []string{"DIRECT_PLAY", "EXTERNAL_PLAYER"} {
		if _, err := s.playbackMode(t.Context(), source, domain.Device{}, requested); err == nil {
			t.Fatal("YouTube bypassed finite range relay", requested)
		}
	}
	device.Capabilities.Probes = append(device.Capabilities.Probes, domain.Probe{ID: "http-fmp4", Status: "FAIL", TestedAt: time.Now().Unix()})
	if mode, err := s.playbackMode(t.Context(), source, device, "AUTO"); err != nil || mode.mode != "DIRECT_PLAY" {
		t.Fatal("fragment probe incorrectly blocked progressive MP4", mode, err)
	}
	s.deps.RemoteMedia = remoteMediaStub{err: errors.New("upstream unavailable")}
	if _, err := s.playbackMode(t.Context(), source, domain.Device{}, "AUTO"); err == nil {
		t.Fatal("failed YouTube probe silently became direct play")
	}
}

func TestYouTubeCombinedStreamServesLengthAndRangesWithoutOriginURL(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	client := &progressiveYouTubeHTTP{content: bytes.Repeat([]byte("mp4-content"), 30000)}
	s.deps.StreamHTTP = client
	s.sessions["youtube-test"] = &session{
		mode: "DIRECT_PLAY", ticket: "private-ticket", expires: time.Now().Add(time.Minute), ctx: t.Context(), cancel: func() {},
		source: domain.Source{Item: domain.Item{Provider: "youtube"}, URL: "https://r1.googlevideo.com/video?signature=private", MIME: "video/mp4"},
	}
	for _, tc := range []struct {
		name, rangeValue string
		start            int
		status           int
	}{
		{name: "full", start: 0, status: http.StatusOK},
		{name: "seek", rangeValue: "bytes=17-", start: 17, status: http.StatusPartialContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/streams/youtube-test?ticket=private-ticket", nil)
			if tc.rangeValue != "" {
				request.Header.Set("Range", tc.rangeValue)
			}
			response := httptest.NewRecorder()
			s.ServeHTTP(response, request)
			if response.Code != tc.status || !bytes.Equal(response.Body.Bytes(), client.content[tc.start:]) {
				t.Fatalf("progressive relay: status=%d bytes=%d", response.Code, response.Body.Len())
			}
			if response.Header().Get("Content-Length") != strconv.Itoa(len(client.content)-tc.start) || bytes.Contains(response.Body.Bytes(), []byte("private")) {
				t.Fatal("length missing or origin leaked")
			}
		})
	}
	if len(client.ranges) < 4 {
		t.Fatal("gateway did not request finite upstream chunks")
	}
}

func TestLiveTransportConvertsOnlyOnExplicitRetry(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	s.deps.RemoteMedia = remoteMediaStub{err: errors.New("must not probe Auto")}
	source := domain.Source{URL: "https://channel.test/live", MIME: "video/mp2t", Live: true}
	if mode, err := s.playbackMode(t.Context(), source, domain.Device{}, "AUTO"); err != nil || mode.mode != "DIRECT_PLAY" {
		t.Fatal(mode, err)
	}
	s.deps.RemoteMedia = remoteMediaStub{}
	for _, requested := range []string{"REMUX", "TRANSCODE"} {
		if mode, err := s.playbackMode(t.Context(), source, domain.Device{}, requested); err != nil || mode.mode != requested {
			t.Fatal(mode, err)
		}
	}
}

func TestManifestAutoConvertsBothLiveAndVod(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	s.deps.RemoteMedia = remoteMediaStub{}
	for _, mime := range []string{"application/vnd.apple.mpegurl", "application/dash+xml"} {
		for _, live := range []bool{false, true} {
			source := domain.Source{URL: "https://source.test/manifest", MIME: mime, Live: live}
			if mode, err := s.playbackMode(t.Context(), source, domain.Device{}, "AUTO"); err != nil || mode.mode != "REMUX" {
				t.Fatal(mime, live, mode, err)
			}
			device := domain.Device{Capabilities: domain.Capabilities{Probes: []domain.Probe{{ID: "http-fmp4", Status: "FAIL", TestedAt: time.Now().Unix()}}}}
			if mode, err := s.playbackMode(t.Context(), source, device, "AUTO"); err != nil || mode.mode != "EXTERNAL_PLAYER" {
				t.Fatal(mode, err)
			}
		}
	}
}
