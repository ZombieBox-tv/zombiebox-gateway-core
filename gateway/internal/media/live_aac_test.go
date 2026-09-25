package media

import (
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestLiveAACRemuxRequiresAudioOnlyHLS(t *testing.T) {
	source := domain.Source{
		URL:  "http://receiver.test/audio.m3u8",
		MIME: "application/vnd.apple.mpegurl",
		Live: true,
	}
	aac := &domain.Metadata{Streams: []domain.Stream{{Type: "audio", Codec: "aac"}}}
	for _, test := range []struct {
		name     string
		source   domain.Source
		metadata *domain.Metadata
		mode     string
		want     bool
	}{
		{"live AAC HLS remux", source, aac, "REMUX", true},
		{"direct HLS", source, aac, "DIRECT_PLAY", false},
		{"transcode", source, aac, "TRANSCODE", false},
		{"no probe", source, nil, "REMUX", false},
		{"video present", source, &domain.Metadata{Streams: []domain.Stream{{Type: "audio", Codec: "aac"}, {Type: "video", Codec: "h264"}}}, "REMUX", false},
		{"MP3 audio", source, &domain.Metadata{Streams: []domain.Stream{{Type: "audio", Codec: "mp3"}}}, "REMUX", false},
		{"not live", domain.Source{URL: source.URL, MIME: source.MIME}, aac, "REMUX", false},
		{"not HLS", domain.Source{URL: "http://receiver.test/audio.aac", MIME: "audio/aac", Live: true}, aac, "REMUX", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := LiveAACRemux(test.source, test.metadata, test.mode); got != test.want {
				t.Fatalf("LiveAACRemux = %v, want %v", got, test.want)
			}
		})
	}
}
