package media

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

// Googlevideo currently rejects open-ended HTTP ranges, which FFprobe and
// FFmpeg use for progressive inputs. Relay finite chunks as one continuous
// response without buffering the video or exposing its signed URL to a player.
const youtubeRangeChunk = int64(256 << 10)
const youtubeMaxMediaBytes = int64(32 << 30)

var openByteRange = regexp.MustCompile(`^bytes=([0-9]+)-$`)
var contentByteRange = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)

// YouTubeRangeOrigin limits the range workaround to resolved signed media hosts.
func YouTubeRangeOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.User == nil && parsed.Port() == "" && strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".googlevideo.com")
}

// RelayYouTubeProgressive serves a validated combined YouTube MP4 as a normal
// byte-range HTTP resource. The client sees this gateway only; each upstream
// request remains finite even when a legacy player asks for an open range.
func RelayYouTubeProgressive(w http.ResponseWriter, r *http.Request, client HTTPClient, source domain.Source) (bool, error) {
	if source.Item.Provider != "youtube" || source.AudioURL != "" || !YouTubeRangeOrigin(source.URL) {
		return false, errors.New("YouTube progressive source unavailable")
	}
	return relayYouTubeOpenRange(w, r, client, source.URL, source.Headers)
}

func openRangeStart(value string) (int64, bool) {
	if value == "" {
		return 0, true
	}
	match := openByteRange.FindStringSubmatch(value)
	if match == nil {
		return 0, false
	}
	start, err := strconv.ParseInt(match[1], 10, 64)
	return start, err == nil
}

func parseContentRange(value string, start, requestedEnd int64) (end, total int64, err error) {
	match := contentByteRange.FindStringSubmatch(value)
	if match == nil {
		return 0, 0, errors.New("invalid upstream content range")
	}
	first, e1 := strconv.ParseInt(match[1], 10, 64)
	last, e2 := strconv.ParseInt(match[2], 10, 64)
	size, e3 := strconv.ParseInt(match[3], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || first != start || last < first || last > requestedEnd || size <= last {
		return 0, 0, errors.New("inconsistent upstream content range")
	}
	return last, size, nil
}

// relayYouTubeOpenRange writes a single HTTP response from bounded upstream
// requests. An error after headers have been sent ends the body early, which
// lets the media process detect a truncated stream rather than accept bad data.
func relayYouTubeOpenRange(w http.ResponseWriter, r *http.Request, client HTTPClient, target string, headers http.Header) (bool, error) {
	start, ok := openRangeStart(r.Header.Get("Range"))
	if !ok {
		return false, errors.New("unsupported open range")
	}
	next := start
	var total int64
	for {
		if err := r.Context().Err(); err != nil {
			return total != 0, err
		}
		end := next + youtubeRangeChunk - 1
		if end < next {
			return total != 0, errors.New("media range overflow")
		}
		if total != 0 && end >= total {
			end = total - 1
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
		if err != nil {
			return total != 0, err
		}
		request.Header = headers.Clone()
		if request.Header == nil {
			request.Header = make(http.Header)
		}
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", next, end))
		request.Header.Set("Accept-Encoding", "identity")
		response, err := client.Do(request)
		if err != nil {
			return total != 0, err
		}
		if response.StatusCode != http.StatusPartialContent {
			response.Body.Close()
			return total != 0, errors.New("upstream did not return a bounded range")
		}
		last, size, err := parseContentRange(response.Header.Get("Content-Range"), next, end)
		if err != nil || size > youtubeMaxMediaBytes || start >= size || (total != 0 && size != total) {
			response.Body.Close()
			return total != 0, errors.New("upstream range changed")
		}
		if total == 0 {
			total = size
			if os.Getenv("ZOMBIE_MEDIA_TRACE") == "1" {
				log.Printf("media progressive first range method=%s start=%d total_bytes=%d first_bytes=%d", r.Method, start, total, last-next+1)
			}
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
			w.Header().Set("Content-Length", strconv.FormatInt(total-start, 10))
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
				w.WriteHeader(http.StatusPartialContent)
			} else {
				w.WriteHeader(http.StatusOK)
			}
		}
		if r.Method == http.MethodHead {
			response.Body.Close()
			return true, nil
		}
		_, err = io.CopyN(w, response.Body, last-next+1)
		response.Body.Close()
		if err != nil {
			return true, err
		}
		next = last + 1
		if next == total {
			return true, nil
		}
	}
}
