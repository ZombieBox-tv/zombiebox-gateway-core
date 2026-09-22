// Package playback holds media policy independently of HTTP and process adapters.
package playback

import (
	"strings"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/subtitles"
)

// Attachment IDs occupy a separate, bounded session-local range, never FFmpeg indexes.
func WithSubtitles(metadata domain.Metadata, source domain.Source) domain.Metadata {
	if source.Live || source.AudioURL != "" || source.Path != "" {
		return metadata
	}
	metadata.Streams = append([]domain.Stream(nil), metadata.Streams...)
	for index, attachment := range source.Subtitles {
		if index >= 32 {
			break
		}
		codec := strings.ToLower(attachment.Codec)
		if !subtitles.TextFormat(codec) || attachment.URL == "" {
			continue
		}
		stream := domain.Stream{Index: domain.ExternalSubtitleBase + index, Type: "subtitle", Codec: codec}
		if len(attachment.Language) <= 32 {
			stream.Tags.Language = attachment.Language
		}
		title := []rune(attachment.Title)
		if len(title) > 128 {
			title = title[:128]
		}
		stream.Tags.Title = string(title)
		if attachment.Default {
			stream.Disposition.Default = 1
		}
		if attachment.Forced {
			stream.Disposition.Forced = 1
		}
		metadata.Streams = append(metadata.Streams, stream)
	}
	return metadata
}

func Inventory(metadata domain.Metadata, selected *int) domain.TrackInventory {
	result := domain.TrackInventory{Available: true, Tracks: []domain.Track{}, AudioID: selected}
	for _, stream := range metadata.Streams {
		if stream.Type != "audio" && stream.Type != "subtitle" {
			continue
		}
		selectable := stream.Type == "audio" || TextSubtitle(stream.Codec)
		result.Tracks = append(result.Tracks, domain.Track{
			ID: stream.Index, Kind: stream.Type, Language: stream.Tags.Language,
			Title: stream.Tags.Title, Default: stream.Disposition.Default == 1,
			Forced: stream.Disposition.Forced == 1, Selectable: selectable,
		})
	}
	return result
}

func TextSubtitle(codec string) bool {
	switch codec {
	case "subrip", "srt", "webvtt", "vtt", "ass", "ssa", "mov_text", "text":
		return true
	default:
		return false
	}
}

func HasTrack(inventory domain.TrackInventory, id int, kind string) bool {
	for _, track := range inventory.Tracks {
		if track.ID == id && track.Kind == kind && track.Selectable {
			return true
		}
	}
	return false
}
