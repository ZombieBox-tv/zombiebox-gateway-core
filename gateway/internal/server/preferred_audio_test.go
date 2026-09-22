package server

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/media"

	"zombiebox.local/gateway/internal/domain"
)

type preferredAudioMedia struct {
	trackMedia
	metadata domain.Metadata
}

func (m *preferredAudioMedia) Probe(context.Context, string) (domain.Metadata, error) {
	return m.metadata, nil
}

func (m *preferredAudioMedia) ProbeRemote(context.Context, domain.Source) (domain.Metadata, error) {
	return m.metadata, nil
}

func (m *preferredAudioMedia) ConvertRemote(ctx context.Context, source domain.Source, mode string, selection domain.MediaSelection, output io.Writer) error {
	return nil
}

func TestPreferredAudioPlanningAcrossLocalRemoteAndManifestSources(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, tc := range []struct {
			name, firstCodec, secondCodec, language, requested, failed, mime, want string
			live, split                                                            bool
			selected                                                               int
		}{
			{name: "discard incompatible alternate", firstCodec: "aac", secondCodec: "dts", language: "en", want: "REMUX", selected: 1},
			{name: "preferred second AAC", firstCodec: "dts", secondCodec: "aac", language: "es", want: "REMUX", selected: 2},
			{name: "preferred DTS", firstCodec: "aac", secondCodec: "dts", language: "es", want: "TRANSCODE", selected: 2},
			{name: "compatible original", firstCodec: "aac", secondCodec: "aac", language: "en", want: "DIRECT_PLAY", selected: 1},
			{name: "fragment failure", firstCodec: "dts", secondCodec: "aac", language: "es", failed: "http-fmp4", want: "EXTERNAL_PLAYER"},
			{name: "AAC failure", firstCodec: "dts", secondCodec: "aac", language: "es", failed: "aac", want: "EXTERNAL_PLAYER"},
			{name: "explicit conversion", firstCodec: "dts", secondCodec: "aac", language: "es", requested: "TRANSCODE", want: "TRANSCODE", selected: 2},
			{name: "vod HLS", firstCodec: "dts", secondCodec: "aac", language: "es", mime: "application/vnd.apple.mpegurl", want: "REMUX", selected: 2},
			{name: "vod DASH", firstCodec: "dts", secondCodec: "aac", language: "es", mime: "application/dash+xml", want: "REMUX", selected: 2},
			{name: "live has no stable selection", firstCodec: "dts", secondCodec: "aac", language: "es", mime: "application/vnd.apple.mpegurl", live: true, want: "TRANSCODE"},
			{name: "split inputs have no single track namespace", firstCodec: "dts", secondCodec: "aac", language: "es", split: true, want: "TRANSCODE"},
		} {
			t.Run(tc.name+map[bool]string{true: "/local", false: "/remote"}[local], func(t *testing.T) {
				first := domain.Stream{Index: 1, Type: "audio", Codec: tc.firstCodec}
				first.Tags.Language = "eng"
				second := domain.Stream{Index: 2, Type: "audio", Codec: tc.secondCodec}
				second.Tags.Language = "spa"
				adapter := &preferredAudioMedia{metadata: domain.Metadata{Streams: []domain.Stream{
					{Index: 0, Type: "video", Codec: "h264", Width: 640, Height: 360}, first, second,
				}}}
				source := domain.Source{URL: "https://media.test/movie", MIME: "video/mp4", Live: tc.live}
				if tc.mime != "" {
					source.MIME = tc.mime
				}
				if local {
					source.Path = "/media/movie"
				}
				if tc.split {
					source.AudioURL = "https://media.test/audio"
				}
				device := domain.Device{Preferences: domain.Preferences{AudioLanguages: []string{tc.language}}}
				if tc.failed != "" {
					device.Capabilities.Probes = []domain.Probe{{ID: tc.failed, Status: "FAIL"}}
				}
				s := testServer(t, nil, "")
				s.deps.Media, s.deps.RemoteMedia = adapter, adapter
				requested := tc.requested
				if requested == "" {
					requested = "AUTO"
				}
				decision, err := s.playbackMode(t.Context(), source, device, requested)
				if err != nil || decision.mode != tc.want {
					t.Fatalf("mode %s, error %v, want %s", decision.mode, err, tc.want)
				}
				if tc.selected == 0 {
					if decision.audioID != nil {
						t.Fatal("unsafe track namespace", *decision.audioID)
					}
				} else if decision.audioID == nil || *decision.audioID != tc.selected {
					t.Fatal("lost preferred audio", decision.audioID)
				}
				if decision.metadata == nil || len(decision.metadata.Streams) != 3 || len(adapter.metadata.Streams) != 3 {
					t.Fatal("source inventory mutated")
				}
			})
		}
	}
}

func TestAutomaticLanguagePlanProducesOnlyPreferredAudio(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg absent")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe absent")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	input := filepath.Join(dir, "languages.mkv")
	fixture := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "color=c=green:s=160x90:r=10",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000",
		"-map", "0:v", "-map", "1:a", "-map", "2:a", "-t", "2",
		"-c:v", "libx264", "-threads", "1", "-c:a:0", "ac3", "-c:a:1", "aac",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=spa", input)
	if output, err := fixture.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	adapter := media.New(ffmpeg, ffprobe)
	s := testServer(t, nil, dir)
	s.deps.Media = adapter
	decision, err := s.playbackMode(ctx, domain.Source{Path: input, MIME: "video/x-matroska"}, domain.Device{
		Preferences: domain.Preferences{AudioLanguages: []string{"es-MX"}},
	}, "AUTO")
	if err != nil || decision.mode != "REMUX" || decision.audioID == nil || *decision.audioID != 2 {
		t.Fatalf("preferred AAC should remux, got %+v %v", decision, err)
	}
	output := filepath.Join(dir, "selected.mp4")
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.ConvertSelected(ctx, input, decision.mode, domain.MediaSelection{AudioID: decision.audioID}, file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	result, err := adapter.Probe(ctx, output)
	if err != nil || len(result.Streams) != 2 {
		t.Fatalf("output: %+v %v", result, err)
	}
	if result.Streams[1].Codec != "aac" {
		t.Fatalf("wrong audio: %+v", result.Streams[1])
	}
	if result.Streams[0].Profile != decision.metadata.Streams[0].Profile || result.Streams[0].Width != 160 {
		t.Fatalf("video unexpectedly converted: %+v", result.Streams[0])
	}
	decoded := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", output, "-f", "null", "-")
	if output, err := decoded.CombinedOutput(); err != nil {
		t.Fatalf("decode: %v %s", err, output)
	}
	// Output metadata is intentionally stripped; decode the signal to prove
	// selection rather than relying on a language tag in the container.
	pcm, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", output,
		"-ss", "0.5", "-map", "0:a", "-t", "1", "-ac", "1", "-ar", "8000",
		"-f", "s16le", "pipe:1").Output()
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
		t.Fatalf("wrong decoded track: %d crossings", crossings)
	}

}
