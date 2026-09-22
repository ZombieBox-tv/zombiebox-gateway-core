package companionmedia

import "bytes"

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
