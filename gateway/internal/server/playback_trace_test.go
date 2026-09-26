package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRangeResponseRecorderPreservesServeContent(t *testing.T) {
	for _, test := range []struct {
		name   string
		range_ string
		status int
		length string
		body   string
	}{
		{name: "closed range", range_: "bytes=2-4", status: http.StatusPartialContent, length: "3", body: "cde"},
		{name: "unsatisfiable range", range_: "bytes=30-40", status: http.StatusRequestedRangeNotSatisfiable},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			observed := &mediaRangeResponseRecorder{ResponseWriter: response}
			request := httptest.NewRequest(http.MethodGet, "/media", nil)
			request.Header.Set("Range", test.range_)
			http.ServeContent(observed, request, "test.mp4", time.Time{}, bytes.NewReader([]byte("abcdefgh")))
			if response.Code != test.status || observed.status != test.status || response.Header().Get("Content-Length") != test.length || observed.bytes != int64(response.Body.Len()) || (test.status == http.StatusPartialContent && response.Body.String() != test.body) {
				t.Fatalf("range response changed: status=%d observed=%d length=%q body=%q written=%d", response.Code, observed.status, response.Header().Get("Content-Length"), response.Body.String(), observed.bytes)
			}
		})
	}
}
