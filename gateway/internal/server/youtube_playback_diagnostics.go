package server

import (
	"encoding/json"
	"log"
	"time"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/playback"
)

type youtubePlaybackDiagnostic struct {
	started time.Time
	event   youtubePlaybackDiagnosticEvent
}

type youtubePlaybackDiagnosticEvent struct {
	Event             string `json:"event"`
	TraceID           string `json:"trace_id"`
	RequestedQuality  string `json:"requested_quality"`
	ChosenQuality     string `json:"chosen_quality"`
	ActualVideoWidth  int    `json:"actual_video_width,omitempty"`
	ActualVideoHeight int    `json:"actual_video_height,omitempty"`
	DeliveryRoute     string `json:"delivery_route"`
	ElapsedMS         int64  `json:"elapsed_ms"`
	Stage             string `json:"stage"`
	Outcome           string `json:"outcome"`
	Resolver          string `json:"resolver"`
	StreamProbe       string `json:"stream_probe"`
	QualityResolution string `json:"quality_resolution"`
	HLSGate           string `json:"hls_gate"`
	HLSGateReason     string `json:"hls_gate_reason"`
	Publisher         string `json:"publisher"`
}

func newYouTubePlaybackDiagnostic(requestedQuality string) *youtubePlaybackDiagnostic {
	return &youtubePlaybackDiagnostic{
		started: time.Now(),
		event: youtubePlaybackDiagnosticEvent{
			Event:             "youtube_playback_terminal",
			TraceID:           randomID(8),
			RequestedQuality:  safeYouTubeQuality(requestedQuality),
			ChosenQuality:     "unselected",
			DeliveryRoute:     "unselected",
			Stage:             "playback",
			Outcome:           "aborted",
			Resolver:          "unreached",
			StreamProbe:       "unreached",
			QualityResolution: "unreached",
			HLSGate:           "unreached",
			HLSGateReason:     "unreached",
			Publisher:         "unreached",
		},
	}
}

func (d *youtubePlaybackDiagnostic) terminal(stage, outcome string) {
	if d == nil {
		return
	}
	d.event.Stage = safeYouTubeDiagnosticStage(stage)
	d.event.Outcome = safeYouTubeDiagnosticOutcome(outcome)
}

func (d *youtubePlaybackDiagnostic) resolver(outcome string) {
	if d != nil {
		d.event.Resolver = category(outcome, "passed", "busy", "unavailable", "failed", "cancelled")
	}
}

func (d *youtubePlaybackDiagnostic) streamProbe(outcome string) {
	if d != nil {
		d.event.StreamProbe = category(outcome, "passed", "not_required", "busy", "failed", "unavailable")
	}
}

func (d *youtubePlaybackDiagnostic) qualityResolution(outcome string) {
	if d != nil {
		d.event.QualityResolution = category(outcome, "passed", "unavailable")
	}
}

func (d *youtubePlaybackDiagnostic) videoDimensions(metadata *domain.Metadata) {
	if d == nil {
		return
	}
	d.event.ActualVideoWidth = 0
	d.event.ActualVideoHeight = 0
	if metadata == nil {
		return
	}
	const maxDiagnosticDimension = 16384
	videoCount := 0
	width, height := 0, 0
	for _, stream := range metadata.Streams {
		if stream.Type != "video" {
			continue
		}
		videoCount++
		if stream.Width <= 0 || stream.Height <= 0 || stream.Width > maxDiagnosticDimension || stream.Height > maxDiagnosticDimension {
			return
		}
		width, height = stream.Width, stream.Height
	}
	if videoCount == 1 {
		d.event.ActualVideoWidth = width
		d.event.ActualVideoHeight = height
	}
}

func (d *youtubePlaybackDiagnostic) hlsGate(eligible bool, reason string) {
	if d == nil {
		return
	}
	d.event.HLSGateReason = safeYouTubeHLSGateReason(reason)
	if eligible {
		d.event.HLSGate = "eligible"
		return
	}
	d.event.HLSGate = "ineligible"
}

func (d *youtubePlaybackDiagnostic) publisher(outcome string) {
	if d != nil {
		d.event.Publisher = category(outcome, "starting", "ready_first_segment", "first_segment_timeout", "failed", "busy", "cancelled")
	}
}

func (d *youtubePlaybackDiagnostic) finish(chosenQuality, deliveryMode string) {
	if d == nil {
		return
	}
	switch d.event.Outcome {
	case "quality_unavailable", "quality_resolution_unavailable":
		chosenQuality = "unavailable"
		deliveryMode = ""
	case "aborted", "item_unavailable", "resolver_busy", "resolver_unavailable", "conversion_unavailable", "stream_probe_busy", "stream_probe_failed":
		chosenQuality = "unselected"
		deliveryMode = ""
	}
	d.event.ChosenQuality = safeYouTubeChosenQuality(chosenQuality)
	d.event.DeliveryRoute = safeYouTubeDeliveryRoute(deliveryMode)
	d.event.ElapsedMS = time.Since(d.started).Milliseconds()
	encoded, err := json.Marshal(d.event)
	if err != nil {
		log.Print(`youtube_playback_diagnostic {"event":"youtube_playback_terminal","outcome":"encoding_failed"}`)
		return
	}
	log.Printf("youtube_playback_diagnostic %s", encoded)
}

func category(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return candidate
		}
	}
	return "unknown"
}

func safeYouTubeQuality(value string) string {
	if value == "" || value == "auto" {
		return "auto"
	}
	if playback.ValidQuality(value) {
		return value
	}
	return "unknown"
}

func safeYouTubeChosenQuality(value string) string {
	if value == "unselected" || value == "unavailable" {
		return value
	}
	return safeYouTubeQuality(value)
}

func safeYouTubeDeliveryRoute(value string) string {
	if value == youtubeHLSSessionMode {
		return "youtube_hls"
	}
	if value == "" {
		return "unselected"
	}
	return category(value, "DIRECT_PLAY", "REMUX", "TRANSCODE", "EXTERNAL_PLAYER", "HYBRID", "PCM_STREAM")
}

func safeYouTubeDiagnosticStage(value string) string {
	return category(value, "source", "resolver", "planning", "stream_probe", "quality", "quality_resolution", "receiver", "session", "publisher", "playback")
}

func safeYouTubeDiagnosticOutcome(value string) string {
	return category(
		value,
		"aborted",
		"item_unavailable",
		"resolver_busy",
		"resolver_unavailable",
		"conversion_unavailable",
		"stream_probe_busy",
		"stream_probe_failed",
		"quality_unavailable",
		"quality_resolution_unavailable",
		"receiver_changed",
		"session_limit",
		"handoff_disabled",
		"publisher_busy",
		"publisher_first_segment_timeout",
		"publisher_failed",
		"publisher_cancelled",
		"server_stopping",
		"success",
	)
}

func safeYouTubeHLSGateReason(value string) string {
	return category(
		value,
		"unreached",
		"eligible",
		"native_streaming_preferred",
		"publisher_unavailable",
		"publisher_directory_unavailable",
		"source_provider",
		"source_kind",
		"source_live",
		"source_video_missing",
		"source_audio_missing",
		"source_duration",
		"source_manifest",
		"source_remote_ineligible",
		"metadata_missing",
		"fresh_event_probe",
		"quality_height_ineligible",
		"video_dimensions_ineligible",
		"video_codec",
		"video_height",
		"audio_codec",
		"stream_count",
		"native_decoder_incompatible",
	)
}
