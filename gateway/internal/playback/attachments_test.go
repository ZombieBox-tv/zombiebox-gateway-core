package playback

import (
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestAttachmentInventoryAndLanguageSelection(t *testing.T) {
	metadata := domain.Metadata{Streams: []domain.Stream{{Index: 0, Type: "video", Codec: "h264"}}}
	source := domain.Source{Subtitles: []domain.SubtitleSource{{URL: "http://provider/sub", Codec: "srt", Language: "spa"}}}
	with := WithSubtitles(metadata, source)
	_, selected := PreferredTracks(with, domain.Preferences{SubtitleMode: "always", SubtitleLanguages: []string{"es"}})
	if selected == nil || *selected != domain.ExternalSubtitleBase || len(metadata.Streams) != 1 {
		t.Fatal("attachment selection or metadata ownership failed")
	}
	source.Live = true
	if len(WithSubtitles(metadata, source).Streams) != 1 {
		t.Fatal("live attachment incorrectly advertised")
	}
}
