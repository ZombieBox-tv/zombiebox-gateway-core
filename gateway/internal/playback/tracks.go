// Package playback holds media policy independently of HTTP and process adapters.
package playback

import (
	"zombiebox.local/gateway/internal/domain"
)

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
