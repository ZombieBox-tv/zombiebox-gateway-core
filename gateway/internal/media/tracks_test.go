package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestRealTrackSelectionAndSubtitleExtraction(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	folder := t.TempDir()
	captions := filepath.Join(folder, "captions.srt")
	if err := os.WriteFile(captions, []byte("1\n00:00:00,500 --> 00:00:02,500\nHello Zombie\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(folder, "multitrack.mkv")
	cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=green:s=160x90:r=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=44100", "-i", captions, "-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:s", "-t", "3", "-c:v", "libx264", "-threads", "1", "-c:a", "aac", "-c:s", "ass", "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=spa", "-metadata:s:s:0", "language=eng", input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	tools := New(ffmpeg, ffprobe)
	metadata, err := tools.Probe(ctx, input)
	if err != nil || len(metadata.Streams) != 4 {
		t.Fatalf("probe: %v %+v", err, metadata)
	}
	if metadata.Streams[2].Index != 2 || metadata.Streams[2].Tags.Language != "spa" {
		t.Fatalf("track identity lost: %+v", metadata.Streams[2])
	}
	cues, err := tools.Subtitles(ctx, input, 3)
	if err != nil || len(cues) != 1 || cues[0].Text != "Hello Zombie" || cues[0].StartMS < 500 {
		t.Fatalf("ASS simplification: %+v %v", cues, err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-private" {
			t.Error("remote credentials missing")
			w.WriteHeader(401)
			return
		}
		http.ServeFile(w, r, input)
	}))
	defer upstream.Close()
	remote := NewRemote(tools, upstream.Client())
	source := domain.Source{URL: upstream.URL + "/movie.mkv", MIME: "video/x-matroska", Headers: http.Header{"Authorization": {"Bearer fixture-private"}}}
	remoteCues, err := remote.SubtitlesRemote(ctx, source, 3)
	if err != nil || len(remoteCues) != 1 || remoteCues[0].Text != "Hello Zombie" {
		t.Fatal("remote text extraction", remoteCues, err)
	}
	output := filepath.Join(folder, "selected.mp4")
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	selected := 2
	err = tools.ConvertSelected(ctx, input, "TRANSCODE", domain.MediaSelection{AudioID: &selected, PositionMS: 1000, Quality: "LOW"}, file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools.Probe(ctx, output)
	if err != nil || len(result.Streams) != 2 || result.Streams[1].Codec != "aac" {
		t.Fatalf("selected output: %+v %v", result, err)
	}
	if result.Streams[0].Width > 426 || result.Streams[0].Height > 240 {
		t.Fatalf("low profile dimensions: %+v", result.Streams[0])
	}
	duration, _ := strconv.ParseFloat(result.Format.Duration, 64)
	if duration < 1.8 || duration > 2.4 {
		t.Fatalf("position was not preserved: %f", duration)
	}
	// Compare the decoded spectral peak, not container metadata, to prove the
	// requested second audio stream is in the output rather than the first one.
	pcm, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", output, "-map", "0:a", "-t", "1", "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	crossings := 0
	var previous int16
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8)
		if previous < 0 && sample >= 0 {
			crossings++
		}
		previous = sample
	}
	if crossings < 800 || crossings > 960 {
		t.Fatalf("wrong audio stream: %d crossings", crossings)
	}
}
