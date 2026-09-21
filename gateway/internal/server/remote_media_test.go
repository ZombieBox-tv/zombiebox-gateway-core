package server

import (
	"context"
	"errors"
	"io"
	"testing"

	"zombiebox.local/gateway/internal/domain"
)

type remoteMediaStub struct{ err error }

func (stub remoteMediaStub) ProbeRemote(context.Context, domain.Source) (domain.Metadata, error) {
	return domain.Metadata{Streams: []domain.Stream{{Type: "video", Codec: "h264", Width: 640, Height: 360}, {Type: "audio", Codec: "aac"}}}, stub.err
}
func (remoteMediaStub) ConvertRemote(context.Context, domain.Source, string, domain.MediaSelection, io.Writer) error {
	return nil
}

func TestAdaptivePlaybackNeverReturnsVideoOnlyOrIgnoresFailedFragmentProbe(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	s.deps.RemoteMedia = remoteMediaStub{}
	source := domain.Source{URL: "https://video.test/clip", AudioURL: "https://audio.test/clip", MIME: "video/mp4"}
	device := domain.Device{}
	if mode, err := s.playbackMode(context.Background(), source, device, ""); err != nil || mode.mode != "REMUX" {
		t.Fatal(mode, err)
	}
	for _, requested := range []string{"DIRECT_PLAY", "EXTERNAL_PLAYER"} {
		if _, err := s.playbackMode(context.Background(), source, device, requested); err == nil {
			t.Fatal("accepted video-only mode", requested)
		}
	}
	device.Capabilities.Probes = []domain.Probe{{ID: "http-fmp4", Status: "FAIL"}}
	if _, err := s.playbackMode(context.Background(), source, device, ""); err == nil {
		t.Fatal("ignored failed fragment probe")
	}
	s.deps.RemoteMedia = remoteMediaStub{err: errors.New("network unavailable")}
	if _, err := s.playbackMode(context.Background(), source, domain.Device{}, ""); err == nil {
		t.Fatal("missing mux silently fell back")
	}
	source.AudioURL = ""
	if mode, err := s.playbackMode(context.Background(), source, domain.Device{}, ""); err != nil || mode.mode != "DIRECT_PLAY" {
		t.Fatal("network failure treated as decoder failure", mode, err)
	}
}

func TestLiveTransportConvertsOnlyOnExplicitRetry(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	s.deps.RemoteMedia = remoteMediaStub{err: errors.New("must not probe Auto")}
	source := domain.Source{URL: "https://channel.test/live", MIME: "video/mp2t", Live: true}
	if mode, err := s.playbackMode(t.Context(), source, domain.Device{}, "AUTO"); err != nil || mode.mode != "DIRECT_PLAY" {
		t.Fatal(mode, err)
	}
	s.deps.RemoteMedia = remoteMediaStub{}
	for _, requested := range []string{"REMUX", "TRANSCODE"} {
		if mode, err := s.playbackMode(t.Context(), source, domain.Device{}, requested); err != nil || mode.mode != requested {
			t.Fatal(mode, err)
		}
	}
}

func TestManifestAutoConvertsBothLiveAndVod(t *testing.T) {
	s := testServer(t, nil, t.TempDir())
	s.deps.RemoteMedia = remoteMediaStub{}
	for _, mime := range []string{"application/vnd.apple.mpegurl", "application/dash+xml"} {
		for _, live := range []bool{false, true} {
			source := domain.Source{URL: "https://source.test/manifest", MIME: mime, Live: live}
			if mode, err := s.playbackMode(t.Context(), source, domain.Device{}, "AUTO"); err != nil || mode.mode != "REMUX" {
				t.Fatal(mime, live, mode, err)
			}
			device := domain.Device{Capabilities: domain.Capabilities{Probes: []domain.Probe{{ID: "http-fmp4", Status: "FAIL"}}}}
			if mode, err := s.playbackMode(t.Context(), source, device, "AUTO"); err != nil || mode.mode != "EXTERNAL_PLAYER" {
				t.Fatal(mode, err)
			}
		}
	}
}
