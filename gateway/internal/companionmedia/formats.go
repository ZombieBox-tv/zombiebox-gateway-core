package companionmedia

import (
	"bytes"
	"encoding/binary"
)

// Reject text manifests, URLs and arbitrary files before any media subprocess.
// Actual streams/codecs are still determined by the injected media probe.
func identify(p []byte) (string, string) {
	switch {
	case len(p) >= 12 && string(p[4:8]) == "ftyp":
		return "video/mp4", "video"
	case bytes.HasPrefix(p, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "video/x-matroska", "video"
	case len(p) >= 12 && string(p[:4]) == "RIFF" && string(p[8:12]) == "WAVE":
		return "audio/wav", "audio"
	case len(p) >= 12 && string(p[:4]) == "RIFF" && string(p[8:12]) == "AVI ":
		return "video/x-msvideo", "video"
	case len(p) >= 9 && string(p[:3]) == "FLV" && p[3] == 1 && p[4]&^byte(5) == 0 && p[4] != 0 && binary.BigEndian.Uint32(p[5:9]) >= 9:
		return "video/x-flv", "video"
	case len(p) >= 30 && bytes.Equal(p[:16], []byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11, 0xa6, 0xd9, 0x00, 0xaa, 0x00, 0x62, 0xce, 0x6c}) && binary.LittleEndian.Uint64(p[16:24]) >= 30:
		return "video/x-ms-asf", "video"
	case bytes.HasPrefix(p, []byte("fLaC")):
		return "audio/flac", "audio"
	case bytes.HasPrefix(p, []byte("OggS")):
		return "audio/ogg", "audio"
	case bytes.HasPrefix(p, []byte("ID3")) || len(p) >= 2 && p[0] == 0xff && p[1]&0xe6 == 0xe2:
		return "audio/mpeg", "audio"
	default:
		return "", ""
	}
}
