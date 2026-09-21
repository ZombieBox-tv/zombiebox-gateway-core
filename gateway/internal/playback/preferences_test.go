package playback

import (
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestLanguagePreferenceAndSubtitleModes(t *testing.T) {
	stream := func(id int, kind, language string, forced bool) domain.Stream {
		value := domain.Stream{Index: id, Type: kind, Codec: "subrip"}
		value.Tags.Language = language
		if kind == "audio" {
			value.Codec = "aac"
		}
		if forced {
			value.Disposition.Forced = 1
		}
		return value
	}
	metadata := domain.Metadata{Streams: []domain.Stream{
		stream(1, "audio", "eng", false), stream(2, "audio", "spa", false),
		stream(3, "subtitle", "es", false), stream(4, "subtitle", "spa", true),
		stream(5, "subtitle", "eng", false),
	}}
	for _, mode := range []string{"off", "forced", "auto", "always"} {
		preferences := domain.Preferences{AudioLanguages: []string{"es-MX", "en"}, SubtitleLanguages: []string{"es"}, SubtitleMode: mode}
		audio, subtitle := PreferredTracks(metadata, preferences)
		if audio == nil || *audio != 2 || !RequiresAudioMapping(metadata, audio) {
			t.Fatal(mode, "audio", audio)
		}
		expected := 4
		if mode == "always" {
			expected = 3
		}
		if mode == "off" {
			if subtitle != nil {
				t.Fatal("off selected subtitle")
			}
		} else if subtitle == nil || *subtitle != expected {
			t.Fatal(mode, "subtitle", subtitle)
		}
	}
	audio, subtitle := PreferredTracks(metadata, domain.Preferences{AudioLanguages: []string{"ja"}, SubtitleLanguages: []string{"en-US"}, SubtitleMode: "auto"})
	if audio == nil || *audio != 1 || subtitle == nil || *subtitle != 5 {
		t.Fatal("unmatched audio requires readable subtitles")
	}
	_, subtitle = PreferredTracks(metadata, domain.Preferences{AudioLanguages: []string{"ja"}, SubtitleLanguages: []string{"fr"}, SubtitleMode: "auto"})
	if subtitle != nil {
		t.Fatal("auto chose unrelated subtitle language")
	}
}
