package media

import (
	"bytes"
	"context"
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
