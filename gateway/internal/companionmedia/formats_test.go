package companionmedia

import "testing"

func TestLegacyContainersDoNotAdmitTextOrOtherRIFFFiles(t *testing.T) {
	for _, input := range []string{"RIFFxxxxWEBP", "#EXTM3U\nhttp://127.0.0.1/private", "ffconcat version 1.0\nfile /etc/passwd", "FLV", "FLV\x02\x05\x00\x00\x00\x09", "FLV\x01\xff\x00\x00\x00\x09"} {
		if mime, _ := identify([]byte(input)); mime != "" {
			t.Fatalf("unexpected candidate %q", input)
		}
	}
	for _, input := range []string{"RIFFxxxxAVI ", "FLV\x01\x05\x00\x00\x00\x09"} {
		if mime, kind := identify([]byte(input)); mime == "" || kind != "video" {
			t.Fatalf("missing candidate %q", input)
		}
	}
}
