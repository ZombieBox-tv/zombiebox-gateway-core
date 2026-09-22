package playback

import "zombiebox.local/gateway/internal/domain"

// SelectedAudioMode evaluates only the audio that the replacement will map.
// Accurate nonzero resume and bandwidth caps require the existing transcode path.
func SelectedAudioMode(metadata domain.Metadata, mime string, capabilities domain.Capabilities, selection domain.MediaSelection, currentMode string) string {
	if selection.AudioID == nil {
		return "EXTERNAL_PLAYER"
	}
	selected := metadata
	selected.Streams = nil
	found := false
	for _, stream := range metadata.Streams {
		if stream.Type == "audio" {
			if stream.Index != *selection.AudioID {
				continue
			}
			found = true
		}
		selected.Streams = append(selected.Streams, stream)
	}
	if !found {
		return "EXTERNAL_PLAYER"
	}
	failed := func(id string) bool {
		for _, probe := range capabilities.Probes {
			if probe.ID == id && probe.Status == "FAIL" {
				return true
			}
		}
		return false
	}
	if failed("http-fmp4") {
		return "EXTERNAL_PLAYER"
	}
	mode := LocalMode(selected, mime, capabilities, "AUTO")
	if mode == "EXTERNAL_PLAYER" {
		return mode
	}
	if selection.PositionMS > 0 || selection.Quality == "LOW" || currentMode == "TRANSCODE" {
		mode = "TRANSCODE"
	}
	if mode == "TRANSCODE" && (failed("h264-baseline-360") || failed("aac")) {
		return "EXTERNAL_PLAYER"
	}
	if mode == "DIRECT_PLAY" {
		return "REMUX"
	}
	return mode
}
