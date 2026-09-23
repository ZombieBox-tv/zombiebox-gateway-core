package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func TestManifestConversionHLSAndDASH(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for _, fixture := range []string{"hls", "hls-fmp4", "dash", "dash-list"} {
		kind := strings.Split(fixture, "-")[0]
		t.Run(fixture, func(t *testing.T) {
			dir := t.TempDir()
			name := "index.m3u8"
			mime := "application/vnd.apple.mpegurl"
			if kind == "dash" {
				name = "index.mpd"
				mime = "application/dash+xml"
			}
			args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "2", "-c:v", "libx264", "-threads", "1", "-g", "10", "-c:a", "aac", "-f", kind}
			if fixture == "hls-fmp4" {
				args = append(args, "-hls_segment_type", "fmp4")
			}
			if fixture == "dash-list" {
				args = append(args, "-use_template", "0")
			}
			args = append(args, filepath.Join(dir, name))
			if output, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, output)
			}
			files := http.FileServer(http.Dir(dir))
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer manifest-fixture" {
					t.Error("missing credential")
					w.WriteHeader(401)
					return
				}
				files.ServeHTTP(w, r)
			}))
			defer upstream.Close()
			tools := NewWithRunner(ffmpeg, ffprobe, runFunc(func(ctx context.Context, name string, args []string, output io.Writer) error {
				cmd := exec.CommandContext(ctx, name, args...)
				cmd.Stdout = output
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				err := cmd.Run()
				if err != nil {
					t.Log(stderr.String())
				}
				return err
			}))
			remote := NewRemote(tools, upstream.Client())
			source := domain.Source{URL: upstream.URL + "/" + name, MIME: mime, Headers: http.Header{"Authorization": {"Bearer manifest-fixture"}}}
			for _, mode := range []string{"REMUX", "TRANSCODE"} {
				var output bytes.Buffer
				if err := remote.ConvertRemote(ctx, source, mode, domain.MediaSelection{}, &output); err != nil {
					t.Fatal(mode, err)
				}
				target := filepath.Join(dir, mode+".mp4")
				if err := os.WriteFile(target, output.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				info, err := tools.Probe(ctx, target)
				if err != nil || len(info.Streams) != 2 || info.Streams[0].Codec != "h264" || info.Streams[1].Codec != "aac" {
					t.Fatal(mode, info, err)
				}
			}
		})
	}
}

func TestManifestCancellationStopsResourceFetch(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(cancelled) }))
	defer upstream.Close()
	remote := NewRemote(nil, upstream.Client())
	ctx, cancel := context.WithCancel(t.Context())
	bridge, err := remote.bridge(ctx, domain.Source{URL: upstream.URL + "/index.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := http.Get(bridge.video)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	<-entered
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("resource fetch survived cancellation")
	}
	<-done
}

func TestAuthenticatedHLSAlternateAudioSelection(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	for _, fixture := range []struct {
		name, input string
		video       bool
	}{
		{"video", "testsrc2=size=160x90:rate=10", true},
		{"english", "sine=frequency=440:sample_rate=44100", false},
		{"spanish", "sine=frequency=880:sample_rate=44100", false},
	} {
		args := []string{"-y", "-nostdin", "-v", "error", "-f", "lavfi", "-i", fixture.input, "-t", "2"}
		if fixture.video {
			args = append(args, "-an", "-c:v", "libx264", "-threads", "1", "-g", "10")
		} else {
			args = append(args, "-vn", "-c:a", "aac")
		}
		args = append(args, "-hls_time", "1", "-hls_playlist_type", "vod", filepath.Join(dir, fixture.name+".m3u8"))
		if output, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s fixture: %v %s", fixture.name, err, output)
		}
	}
	master := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,URI="english.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="Spanish",LANGUAGE="es",DEFAULT=NO,AUTOSELECT=YES,URI="spanish.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1500000,CODECS="avc1.42E01E,mp4a.40.2",AUDIO="audio"
video.m3u8
`
	if err := os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(master), 0600); err != nil {
		t.Fatal(err)
	}
	files := http.FileServer(http.Dir(dir))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer alternate-audio" {
			t.Error("manifest resource was requested without provider authorization")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	tools := NewWithRunner(ffmpeg, ffprobe, runFunc(func(ctx context.Context, name string, args []string, output io.Writer) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdout = output
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			t.Log(stderr.String())
		}
		return err
	}))
	remote := NewRemote(tools, upstream.Client())
	source := domain.Source{
		URL:     upstream.URL + "/master.m3u8",
		MIME:    "application/vnd.apple.mpegurl",
		Headers: http.Header{"Authorization": {"Bearer alternate-audio"}},
	}
	metadata, err := remote.ProbeRemote(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	var spanish *int
	for _, stream := range metadata.Streams {
		if stream.Type == "audio" && stream.Tags.Language == "es" {
			index := stream.Index
			spanish = &index
		}
	}
	if spanish == nil {
		t.Fatalf("Spanish HLS rendition not available: %+v", metadata.Streams)
	}
	for _, scenario := range []struct {
		name, mode string
		selection  domain.MediaSelection
		minimum    int
		maximum    int
	}{
		{"default-english", "REMUX", domain.MediaSelection{}, 1300, 2800},
		{"selected-spanish-remux", "REMUX", domain.MediaSelection{AudioID: spanish}, 2800, 5000},
		{"selected-spanish-transcode", "TRANSCODE", domain.MediaSelection{AudioID: spanish}, 2800, 5000},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := remote.ConvertRemote(ctx, source, scenario.mode, scenario.selection, &output); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, scenario.name+".mp4")
			if err := os.WriteFile(path, output.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := tools.Probe(ctx, path)
			if err != nil || len(result.Streams) != 2 || result.Streams[0].Codec != "h264" || result.Streams[1].Codec != "aac" {
				t.Fatalf("HLS rendition lost in %s: %+v, %v", scenario.name, result, err)
			}
			pcm, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", path, "-map", "0:a:0", "-f", "s16le", "-ac", "1", "-ar", "44100", "pipe:1").Output()
			if err != nil {
				t.Fatal(err)
			}
			crossings, sign := 0, 0
			for i := 0; i+1 < len(pcm); i += 2 {
				sample := int16(binary.LittleEndian.Uint16(pcm[i:]))
				current := 0
				if sample > 1000 {
					current = 1
				} else if sample < -1000 {
					current = -1
				}
				if current != 0 {
					if sign != 0 && current != sign {
						crossings++
					}
					sign = current
				}
			}
			if crossings < scenario.minimum || crossings > scenario.maximum {
				t.Fatalf("wrong HLS audio rendition: %d zero crossings", crossings)
			}
		})
	}
}
