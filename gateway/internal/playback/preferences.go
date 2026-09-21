package playback

import (
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

// PreferredTracks applies language intent without depending on a platform selector.
// Off disables subtitles; forced never selects a full subtitle track; auto uses
// full subtitles only when the selected audio is outside the preferred languages.
func PreferredTracks(metadata domain.Metadata, preferences domain.Preferences) (audio, subtitle *int) {
	tracks := Inventory(metadata, nil).Tracks
	selectedAudio := preferredTrack(tracks, "audio", preferences.AudioLanguages, false, true)
	if selectedAudio != nil {
		id := selectedAudio.ID
		audio = &id
	}
	if preferences.SubtitleMode == "off" || preferences.SubtitleMode == "" {
		return audio, nil
	}
	forcedOnly := preferences.SubtitleMode == "forced"
	if preferences.SubtitleMode == "auto" && selectedAudio != nil && languageRank(selectedAudio.Language, preferences.AudioLanguages) >= 0 {
		forcedOnly = true
	}
	selectedSubtitle := preferredTrack(tracks, "subtitle", preferences.SubtitleLanguages, forcedOnly, preferences.SubtitleMode == "always" || forcedOnly)
	if selectedSubtitle != nil {
		id := selectedSubtitle.ID
		subtitle = &id
	}
	return audio, subtitle
}

func preferredTrack(tracks []domain.Track, kind string, languages []string, forcedOnly, fallback bool) *domain.Track {
	var selected *domain.Track
	best := -1
	for _, track := range tracks {
		if track.Kind != kind || !track.Selectable || (forcedOnly && !track.Forced) {
			continue
		}
		rank := languageRank(track.Language, languages)
		if rank < 0 && !fallback {
			continue
		}
		score := 0
		if rank >= 0 {
			score = 1000 - rank*10
		}
		if track.Default {
			score += 2
		}
		if score > best {
			copy := track
			selected, best = &copy, score
		}
	}
	return selected
}

func languageRank(language string, preferences []string) int {
	language = languageCode(language)
	if language == "" || language == "und" {
		return -1
	}
	for index, preferred := range preferences {
		if language == languageCode(preferred) {
			return index
		}
	}
	return -1
}

func languageCode(value string) string {
	value = strings.SplitN(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-"), "-", 2)[0]
	aliases := map[string]string{"eng": "en", "spa": "es", "jpn": "ja", "fra": "fr", "fre": "fr", "deu": "de", "ger": "de", "ita": "it", "por": "pt", "zho": "zh", "chi": "zh", "kor": "ko", "rus": "ru", "ara": "ar", "hin": "hi", "nld": "nl", "dut": "nl"}
	if alias, ok := aliases[value]; ok {
		return alias
	}
	return value
}

func RequiresAudioMapping(metadata domain.Metadata, selected *int) bool {
	if selected == nil {
		return false
	}
	for _, stream := range metadata.Streams {
		if stream.Type == "audio" {
			return stream.Index != *selected
		}
	}
	return false
}
