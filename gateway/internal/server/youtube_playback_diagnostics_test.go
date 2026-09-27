package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

func TestYouTubePlaybackDiagnosticIsCategoricalAndRedacted(t *testing.T) {
	buffer, restore := captureYouTubePlaybackLog(t)
	defer restore()

	diagnostic := newYouTubePlaybackDiagnostic("https://private.invalid/video?signature=secret")
	diagnostic.event.TraceID = "0123456789abcdef"
	diagnostic.resolver("busy")
	diagnostic.streamProbe("untrusted probe error https://private.invalid")
	diagnostic.qualityResolution("unavailable")
	diagnostic.hlsGate(false, "fresh_event_probe")
	diagnostic.publisher("first_segment_timeout")
	diagnostic.terminal("resolver", "resolver_busy")
	diagnostic.finish("https://private.invalid/selected?token=secret", "https://private.invalid/route")

	entries := youtubePlaybackDiagnosticEntries(buffer.String())
	if len(entries) != 1 {
		t.Fatalf("terminal diagnostic count = %d, want 1; logs: %s", len(entries), buffer.String())
	}
	var event youtubePlaybackDiagnosticEvent
	if err := json.Unmarshal([]byte(entries[0]), &event); err != nil {
		t.Fatalf("decode diagnostic JSON: %v", err)
	}
	if event.TraceID != "0123456789abcdef" || event.Stage != "resolver" || event.Outcome != "resolver_busy" {
		t.Fatalf("unexpected terminal category: %+v", event)
	}
	if event.RequestedQuality != "unknown" || event.ChosenQuality != "unselected" || event.DeliveryRoute != "unselected" {
		t.Fatalf("unsafe values were not reduced to enums: %+v", event)
	}
	if safeYouTubeChosenQuality("https://private.invalid/?signature=secret") != "unknown" || safeYouTubeDeliveryRoute("https://private.invalid/route") != "unknown" {
		t.Fatal("quality or delivery route mapping accepted raw input")
	}
	if event.Resolver != "busy" || event.StreamProbe != "unknown" || event.QualityResolution != "unavailable" || event.HLSGate != "ineligible" || event.HLSGateReason != "fresh_event_probe" || event.Publisher != "first_segment_timeout" {
		t.Fatalf("unexpected stage summary: %+v", event)
	}
	if event.ElapsedMS < 0 || event.ElapsedMS > 60_000 {
		t.Fatalf("elapsed time out of bounds: %d", event.ElapsedMS)
	}
	for _, private := range []string{"private.invalid", "signature=secret", "token=secret", "untrusted probe error"} {
		if strings.Contains(buffer.String(), private) {
			t.Fatalf("diagnostic leaked %q: %s", private, buffer.String())
		}
	}
}

func captureYouTubePlaybackLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buffer := &bytes.Buffer{}
	previousWriter := log.Writer()
	previousPrefix := log.Prefix()
	previousFlags := log.Flags()
	log.SetOutput(buffer)
	log.SetPrefix("")
	log.SetFlags(0)
	return buffer, func() {
		log.SetOutput(previousWriter)
		log.SetPrefix(previousPrefix)
		log.SetFlags(previousFlags)
	}
}

func youtubePlaybackDiagnosticEntries(output string) []string {
	const marker = "youtube_playback_diagnostic "
	var entries []string
	for _, line := range strings.Split(output, "\n") {
		if index := strings.Index(line, marker); index >= 0 {
			entries = append(entries, strings.TrimSpace(line[index+len(marker):]))
		}
	}
	return entries
}

type youtubeDiagnosticContextResolver struct {
	inner Resolver
	ids   []string
}

func (r *youtubeDiagnosticContextResolver) Resolve(ctx context.Context, source domain.Source) (domain.Source, error) {
	r.ids = append(r.ids, providers.YouTubeDiagnosticIDFromContext(ctx))
	return r.inner.Resolve(ctx, source)
}
