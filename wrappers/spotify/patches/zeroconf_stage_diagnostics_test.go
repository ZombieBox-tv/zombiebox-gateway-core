//go:build test_unit

package zeroconf

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

type stageCaptureLogger struct {
	librespot.Logger
	mu      sync.Mutex
	entries []string
}

func (l *stageCaptureLogger) record(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, message)
}

func (l *stageCaptureLogger) Info(args ...interface{}) {
	l.record(fmt.Sprint(args...))
}

func (l *stageCaptureLogger) Infof(format string, args ...interface{}) {
	l.record(fmt.Sprintf(format, args...))
}

func (l *stageCaptureLogger) WithField(string, interface{}) librespot.Logger { return l }
func (l *stageCaptureLogger) WithError(error) librespot.Logger               { return l }

func (l *stageCaptureLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

func TestZeroconfStageDiagnosticsUseFixedRedactedEvents(t *testing.T) {
	z := newTestZeroconf(t)
	logger := &stageCaptureLogger{}
	z.log = logger

	if err := z.handleGetInfo(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)); err != nil {
		t.Fatal(err)
	}

	go func() {
		request := <-z.reqsChan
		request.result <- true
	}()
	if err := z.handleAddUser(httptest.NewRecorder(), newAddUserRequest(t, z, "private-account", "private-device")); err != nil {
		t.Fatal(err)
	}

	var getInfo, addUser, checksumAccepted bool
	for _, event := range logger.snapshot() {
		switch event {
		case "zeroconf getinfo request":
			getInfo = true
		case "zeroconf adduser request":
			addUser = true
		case "zeroconf adduser checksum accepted":
			checksumAccepted = true
		}
		if strings.Contains(event, "private-account") || strings.Contains(event, "private-device") {
			if strings.HasPrefix(event, "zeroconf getinfo request") ||
				strings.HasPrefix(event, "zeroconf adduser request") ||
				strings.HasPrefix(event, "zeroconf adduser checksum accepted") {
				t.Fatalf("stage event leaked request data: %q", event)
			}
		}
	}
	if !getInfo || !addUser || !checksumAccepted {
		t.Fatalf("missing fixed stage events: getInfo=%t addUser=%t checksumAccepted=%t", getInfo, addUser, checksumAccepted)
	}
}
