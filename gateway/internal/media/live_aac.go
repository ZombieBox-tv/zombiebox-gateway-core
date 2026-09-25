package media

import "zombiebox.local/gateway/internal/domain"

// LiveAACRemux identifies audio-only HLS that can be emitted as progressive
// ADTS rather than fragmented MP4. The server uses this for its MIME contract;
// conversion independently probes the current upstream before writing bytes.
func LiveAACRemux(source domain.Source, metadata *domain.Metadata, mode string) bool {
	if mode != "REMUX" || !source.Live || ManifestKind(source) != "hls" || metadata == nil {
		return false
	}
	hasAAC := false
	for _, stream := range metadata.Streams {
		if stream.Type == "video" {
			return false
		}
		if stream.Type == "audio" {
			if stream.Codec != "aac" {
				return false
			}
			hasAAC = true
		}
	}
	return hasAAC
}
