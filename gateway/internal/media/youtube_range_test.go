package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

type youtubeRangeClient struct{}

func (youtubeRangeClient) Do(r *http.Request) (*http.Response, error) {
	response := &http.Response{Header: make(http.Header), Request: r}
	if r.Header.Get("Range") != "bytes=0-262143" {
		response.StatusCode = http.StatusForbidden
		response.Body = io.NopCloser(bytes.NewReader(nil))
		return response, nil
	}
	response.StatusCode = http.StatusPartialContent
	response.Header.Set("Content-Range", "bytes 0-262143/524288")
	response.Header.Set("Content-Type", "video/mp4")
	response.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("v"), int(youtubeRangeChunk))))
	return response, nil
}

type ignoreTruncatedYouTubeInput struct{}

func (ignoreTruncatedYouTubeInput) Run(_ context.Context, _ string, args []string, _ io.Writer) error {
	for i, arg := range args {
		if arg == "-i" && i+1 < len(args) {
			response, err := http.Get(args[i+1])
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			return nil
		}
	}
	return fmt.Errorf("missing input")
}

func TestYouTubeOpenRangeRelaysFiniteChunks(t *testing.T) {
	content := bytes.Repeat([]byte("zombie-video"), 60000)
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end-start+1 > int(youtubeRangeChunk) || start >= len(content) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if end >= len(content) {
			end = len(content) - 1
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
	}))
	defer upstream.Close()
	for _, tc := range []struct {
		name, rangeValue string
		start            int
		status           int
	}{
		{"entire", "", 0, http.StatusOK},
		{"seek", "bytes=19-", 19, http.StatusPartialContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://bridge/video", nil)
			if tc.rangeValue != "" {
				request.Header.Set("Range", tc.rangeValue)
			}
			response := httptest.NewRecorder()
			started, err := relayYouTubeOpenRange(response, request, upstream.Client(), upstream.URL, nil)
			if err != nil || !started || response.Code != tc.status || !bytes.Equal(response.Body.Bytes(), content[tc.start:]) {
				t.Fatalf("relay: started=%t err=%v status=%d bytes=%d", started, err, response.Code, response.Body.Len())
			}
			if response.Header().Get("Content-Length") != strconv.Itoa(len(content)-tc.start) {
				t.Fatal("wrong length")
			}
			if tc.rangeValue != "" && response.Header().Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", tc.start, len(content)-1, len(content)) {
				t.Fatal("wrong downstream range")
			}
		})
	}
	if requests < 6 {
		t.Fatal("relay did not request successive bounded chunks")
	}
}

func TestYouTubeOpenRangeRejectsBrokenUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-262143" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-262143/524288")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(youtubeRangeChunk)))
	}))
	defer upstream.Close()
	request := httptest.NewRequest(http.MethodGet, "http://bridge/video", nil)
	response := httptest.NewRecorder()
	started, err := relayYouTubeOpenRange(response, request, upstream.Client(), upstream.URL, nil)
	if !started || err == nil || response.Body.Len() != int(youtubeRangeChunk) {
		t.Fatalf("broken upstream accepted: started=%t err=%v bytes=%d", started, err, response.Body.Len())
	}
}

func TestYouTubeRangeOriginGate(t *testing.T) {
	for _, raw := range []string{"http://r1.googlevideo.com/v", "https://googlevideo.com/v", "https://r1.googlevideo.com.evil.invalid/v", "https://name@r1.googlevideo.com/v", "https://r1.googlevideo.com:444/v"} {
		if YouTubeRangeOrigin(raw) {
			t.Fatalf("unsafe YouTube range origin %q", raw)
		}
	}
	if !YouTubeRangeOrigin("https://r1.googlevideo.com/v") {
		t.Fatal("safe YouTube origin rejected")
	}
}

func TestYouTubeProgressiveRelayRequiresCombinedTrustedSource(t *testing.T) {
	request := httptest.NewRequest(http.MethodHead, "http://gateway/stream", nil)
	for _, source := range []domain.Source{
		{Item: domain.Item{Provider: "youtube"}, URL: "http://r1.googlevideo.com/video"},
		{Item: domain.Item{Provider: "other"}, URL: "https://r1.googlevideo.com/video"},
		{Item: domain.Item{Provider: "youtube"}, URL: "https://r1.googlevideo.com/video", AudioURL: "https://r1.googlevideo.com/audio"},
	} {
		if _, err := RelayYouTubeProgressive(httptest.NewRecorder(), request, youtubeRangeClient{}, source); err == nil {
			t.Fatal("untrusted source was relayed")
		}
	}
	response := httptest.NewRecorder()
	started, err := RelayYouTubeProgressive(response, request, youtubeRangeClient{}, domain.Source{Item: domain.Item{Provider: "youtube"}, URL: "https://r1.googlevideo.com/video", MIME: "video/mp4"})
	if err != nil || !started || response.Code != http.StatusOK || response.Header().Get("Content-Length") != "524288" {
		t.Fatalf("progressive HEAD: started=%t err=%v status=%d", started, err, response.Code)
	}
}

func TestYouTubeConversionDoesNotAcceptPartialUpstream(t *testing.T) {
	tools := NewWithRunner("ffmpeg", "ffprobe", ignoreTruncatedYouTubeInput{})
	remote := NewRemote(tools, youtubeRangeClient{})
	source := domain.Source{Item: domain.Item{Provider: "youtube"}, URL: "https://r1.googlevideo.com/video", MIME: "video/mp4"}
	if err := remote.ConvertRemote(t.Context(), source, "REMUX", domain.MediaSelection{}, io.Discard); err == nil {
		t.Fatal("truncated YouTube input was reported as a successful conversion")
	}
}
