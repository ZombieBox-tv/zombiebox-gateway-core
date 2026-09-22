package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

func TestSubtitleAttachmentDownloadAndLimits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "private" {
			t.Error("lost credentials")
		}
		if r.URL.Path == "/large" {
			_, _ = w.Write([]byte(strings.Repeat("x", (2<<20)+1)))
			return
		}
		_, _ = w.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\n<b>Hello</b>\n"))
	}))
	defer upstream.Close()
	remote := NewRemote(New("ffmpeg", "ffprobe"), upstream.Client())
	source := domain.Source{Subtitles: []domain.SubtitleSource{{URL: upstream.URL + "/sub", Codec: "srt", Headers: http.Header{"X-Plex-Token": {"private"}}}}}
	cues, err := remote.SubtitlesRemote(context.Background(), source, domain.ExternalSubtitleBase)
	if err != nil || len(cues) != 1 || cues[0].Text != "Hello" || cues[0].StartMS != 1000 {
		t.Fatalf("%+v %v", cues, err)
	}
	if _, err = remote.SubtitlesRemote(context.Background(), source, domain.ExternalSubtitleBase+1); err == nil {
		t.Fatal("unowned index accepted")
	}
	source.Subtitles[0].URL = upstream.URL + "/large"
	if _, err = remote.SubtitlesRemote(context.Background(), source, domain.ExternalSubtitleBase); err == nil {
		t.Fatal("oversized attachment accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = remote.SubtitlesRemote(ctx, source, domain.ExternalSubtitleBase); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

func TestExternalASSIsSimplifiedThroughBoundedFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[Script Info]\nScriptType: v4.00+\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\nStyle: Default,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,1,0,2,10,10,10,1\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Hello\\Nworld\n"))
	}))
	defer upstream.Close()
	remote := NewRemote(New("ffmpeg", "ffprobe"), upstream.Client())
	source := domain.Source{Subtitles: []domain.SubtitleSource{{URL: upstream.URL + "/sub", Codec: "ass"}}}
	cues, err := remote.SubtitlesRemote(context.Background(), source, domain.ExternalSubtitleBase)
	if err != nil || len(cues) != 1 || cues[0].Text != "Hello\nworld" || cues[0].StartMS != 1000 {
		t.Fatalf("%+v %v", cues, err)
	}
}
