package server

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"zombiebox.local/gateway/internal/domain"
)

type eventLog struct {
	mu       sync.Mutex
	epoch    string
	sequence uint64
	events   []domain.Event
	changed  chan struct{}
}

func newEvents() *eventLog { return &eventLog{epoch: randomID(8), changed: make(chan struct{})} }
func (e *eventLog) publish(device, kind string, payload any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sequence++
	e.events = append(e.events, domain.Event{APIVersion: 1, Cursor: fmt.Sprintf("%s:%d", e.epoch, e.sequence), Type: kind, Payload: payload, DeviceID: device})
	if len(e.events) > 256 {
		e.events = e.events[len(e.events)-256:]
	}
	close(e.changed)
	e.changed = make(chan struct{})
}
func (e *eventLog) read(device, cursor string) ([]domain.Event, string, bool, <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := fmt.Sprintf("%s:%d", e.epoch, e.sequence)
	out := []domain.Event{}
	if cursor == "" {
		return out, next, false, e.changed
	}
	parts := strings.Split(cursor, ":")
	if len(parts) != 2 || parts[0] != e.epoch {
		return out, next, true, e.changed
	}
	seq, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || seq > e.sequence || e.sequence-seq > uint64(len(e.events)) {
		return out, next, true, e.changed
	}
	for _, event := range e.events {
		n, _ := strconv.ParseUint(strings.Split(event.Cursor, ":")[1], 10, 64)
		if n > seq && (event.DeviceID == "" || event.DeviceID == device) {
			out = append(out, event)
		}
	}
	return out, next, false, e.changed
}
