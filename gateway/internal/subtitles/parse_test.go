package subtitles

import (
	"strings"
	"testing"
)

func TestSRTAndVTTPlainTextIntervals(t *testing.T) {
	for _, source := range []string{
		"1\r\n00:00:01,250 --> 00:00:03,500\r\n<b>Hello</b> &amp; hola\r\nSecond line\r\n",
		"WEBVTT\n\ncue-id\n00:01.250 --> 00:03.500 align:middle\n<b>Hello</b> &amp; hola\nSecond line\n",
	} {
		cues, err := ParseSubtitles([]byte(source))
		if err != nil || len(cues) != 1 {
			t.Fatalf("parse: %+v %v", cues, err)
		}
		if cues[0].StartMS != 1250 || cues[0].EndMS != 3500 || cues[0].Text != "Hello & hola\nSecond line" {
			t.Fatalf("cue: %+v", cues[0])
		}
	}
}

func TestRejectMalformedAndUnboundedSubtitles(t *testing.T) {
	for _, value := range []string{
		"1\n00:00:03,000 --> 00:00:02,000\nreverse",
		"1\n00:99:01,000 --> 00:99:02,000\nminutes",
		"1\n00:00:01,000 --> 00:00:02,000\n" + strings.Repeat("x", 4097),
		strings.Repeat("x", (2<<20)+1), string([]byte{255}),
	} {
		if _, err := ParseSubtitles([]byte(value)); err == nil {
			t.Fatal("accepted invalid subtitles")
		}
	}
}
