package playback

import (
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestLiveVideoHLSEventEvidenceSelectsNativeTransportOnlyAfterContinuousPlayback(t *testing.T) {
	now := time.Now().Unix()
	video := domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "h264", Profile: "High", Width: 1280, Height: 720},
		{Type: "audio", Codec: "aac"},
	}}
	video.Format.Name = "hls,applehttp"
	caps := domain.Capabilities{SuiteVersion: 2, CacheKey: "device-bound", Probes: []domain.Probe{
		{ID: "hls-h264-aac", Status: "UNKNOWN", TestedAt: now},
		{ID: "hls-event-h264-aac", Status: "PASS", Completed: true, PositionMS: 28000, TestedAt: now},
		{ID: "h264-720-high", Status: "PASS", PositionMS: 1000, TestedAt: now},
		{ID: "aac", Status: "PASS", PositionMS: 1000, TestedAt: now},
	}}
	if got := LocalMode(video, "application/vnd.apple.mpegurl", caps, "AUTO"); got != "DIRECT_PLAY" {
		t.Fatalf("completed live HLS evidence did not select native transport: %s", got)
	}
	for _, change := range []struct {
		name string
		edit func(*domain.Capabilities)
	}{
		{"prepared_only", func(c *domain.Capabilities) { c.Probes[1].Completed = false }},
		{"short_progress", func(c *domain.Capabilities) { c.Probes[1].PositionMS = 4000 }},
		{"stalled", func(c *domain.Capabilities) { c.Probes[1].Stalled = true }},
		{"stale", func(c *domain.Capabilities) { c.Probes[1].TestedAt = now - 8*24*60*60 }},
		{"unbound", func(c *domain.Capabilities) { c.CacheKey = "" }},
		{"audio_failed", func(c *domain.Capabilities) { c.Probes[3].Status = "FAIL" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := caps
			candidate.Probes = append([]domain.Probe(nil), caps.Probes...)
			change.edit(&candidate)
			if got := LocalMode(video, "application/vnd.apple.mpegurl", candidate, "AUTO"); got == "DIRECT_PLAY" {
				t.Fatal("native HLS selected without complete transport and codec evidence")
			}
		})
	}

	audioOnly := domain.Metadata{Streams: []domain.Stream{{Type: "audio", Codec: "aac"}}}
	audioOnly.Format.Name = "hls,applehttp"
	if got := LocalMode(audioOnly, "application/vnd.apple.mpegurl", caps, "AUTO"); got == "DIRECT_PLAY" {
		t.Fatal("video EVENT probe was treated as audio-only HLS evidence")
	}
}

func TestFiniteHLSProbeDoesNotCertifyLiveVideo(t *testing.T) {
	now := time.Now().Unix()
	video := domain.Metadata{Streams: []domain.Stream{
		{Type: "video", Codec: "h264", Profile: "High", Width: 1280, Height: 720},
		{Type: "audio", Codec: "aac"},
	}}
	video.Format.Name = "hls,applehttp"
	source := domain.Source{MIME: "application/vnd.apple.mpegurl", Live: true,
		Item: domain.Item{Kind: "channel"}}
	caps := domain.Capabilities{SuiteVersion: 2, CacheKey: "device-bound", Probes: []domain.Probe{
		{ID: "hls-h264-aac", Status: "PASS", Completed: true, PositionMS: 2900, TestedAt: now},
		{ID: "h264-720-high", Status: "PASS", PositionMS: 2900, TestedAt: now},
		{ID: "aac", Status: "PASS", PositionMS: 2900, TestedAt: now},
	}}
	if got := LocalModeSource(video, source, caps, "AUTO"); got == "DIRECT_PLAY" {
		t.Fatal("finite single-segment HLS evidence selected a live video stream")
	}
	caps.Probes = append(caps.Probes, domain.Probe{
		ID: "hls-event-h264-aac", Status: "PASS", Completed: true,
		PositionMS: 28000, TestedAt: now,
	})
	if got := LocalModeSource(video, source, caps, "AUTO"); got != "DIRECT_PLAY" {
		t.Fatalf("completed HLS EVENT evidence did not enable live video: %s", got)
	}
	source.Live = false
	caps.Probes = caps.Probes[:len(caps.Probes)-1]
	if got := LocalModeSource(video, source, caps, "AUTO"); got != "DIRECT_PLAY" {
		t.Fatalf("finite HLS evidence stopped selecting a finite video: %s", got)
	}
}
