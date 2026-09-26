package server

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

type mediaTraceCounters struct {
	requests atomic.Uint64
	failures atomic.Uint64
}

// mediaRangeResponseRecorder observes only status and size for local QA. It
// forwards the same response to ServeContent without retaining body data.
type mediaRangeResponseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *mediaRangeResponseRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *mediaRangeResponseRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

// Do not record raw ranges, URLs, session IDs, tickets or headers. Sample the
// repeated reads that some MediaPlayer implementations issue for one file.
func traceKnownLengthResponse(sess *session, r *http.Request, observed *mediaRangeResponseRecorder) {
	if sess.rangeTrace == nil {
		return
	}
	count := sess.rangeTrace.requests.Add(1)
	if observed.status >= 400 {
		failures := sess.rangeTrace.failures.Add(1)
		if failures > 3 && failures%64 != 0 {
			return
		}
	} else if count > 8 && count%64 != 0 {
		return
	}
	length := int64(-1)
	if raw := observed.Header().Get("Content-Length"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed >= 0 {
			length = parsed
		}
	}
	rangeKind := "none"
	if raw := r.Header.Get("Range"); raw != "" {
		rangeKind = "other"
		if strings.HasPrefix(raw, "bytes=") {
			rangeKind = "closed"
			if strings.HasSuffix(raw, "-") {
				rangeKind = "open"
			}
		}
	}
	log.Printf("media known-length response provider=%s mode=%s request=%d method=%s range=%s status=%d content_length=%d content_range=%t written=%d", sess.source.Item.Provider, sess.mode, count, r.Method, rangeKind, observed.status, length, observed.Header().Get("Content-Range") != "", observed.bytes)
}
