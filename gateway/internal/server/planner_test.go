package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/media"
	"zombiebox.local/gateway/internal/providers"
)

func TestLocalPlannerUsesEvidenceNotAndroidVersion(t *testing.T) {
	native := media.Metadata{Streams: []media.Stream{{Type: "video", Codec: "h264", Width: 640, Height: 360}, {Type: "audio", Codec: "aac"}}}
	unsupported := media.Metadata{Streams: []media.Stream{{Type: "video", Codec: "vp9"}}}
	for _, tc := range []struct {
		name, mime, override, want string
		metadata                   media.Metadata
		probes                     []domain.Probe
	}{
		{"native", "video/mp4", "", "DIRECT_PLAY", native, nil},
		{"container", "video/x-matroska", "", "REMUX", native, nil},
		{"codec", "video/webm", "", "TRANSCODE", unsupported, nil},
		{"failed fragment probe", "video/x-matroska", "", "EXTERNAL_PLAYER", native, []domain.Probe{{ID: "http-fmp4", Status: "FAIL"}}},
		{"failed audio probe", "video/webm", "", "EXTERNAL_PLAYER", unsupported, []domain.Probe{{ID: "aac", Status: "FAIL"}}},
		{"explicit override", "video/mp4", "TRANSCODE", "TRANSCODE", native, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := localMode(tc.metadata, tc.mime, domain.Capabilities{Probes: tc.probes}, tc.override); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
func TestHTTPConversionProducesPlayableMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg absent")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe absent")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "sample.mkv")
	cmd := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-t", "0.5", "-c:v", "libx264", "-threads", "1", input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	s := testServer(t, nil, dir)
	s.deps.Media = media.New("ffmpeg", "ffprobe")
	token := pair(t, s, "media-device")
	sources, err := providers.Local(dir)
	if err != nil || len(sources) != 1 {
		t.Fatalf("local MKV catalog: %v %d", err, len(sources))
	}
	for _, mode := range []string{"AUTO", "TRANSCODE"} {
		w := call(s, "POST", "/v1/playback", `{"itemId":"`+sources[0].Item.ID+`","mode":"`+mode+`"}`, "media-device", token, "")
		if w.Code != 201 {
			t.Fatalf("plan: %d %s", w.Code, w.Body)
		}
		var plan domain.Plan
		json.Unmarshal(w.Body.Bytes(), &plan)
		expected := mode
		if mode == "AUTO" {
			expected = "REMUX"
		}
		if plan.Mode != expected || plan.Seekable || plan.ResumeMS != 0 || plan.MIME != "video/mp4" {
			t.Fatalf("invalid plan: %+v", plan)
		}
		stream := call(s, "GET", plan.URL, "", "", "", "")
		if stream.Code != 200 {
			t.Fatal(stream.Code, stream.Body)
		}
		output := filepath.Join(t.TempDir(), "output.mp4")
		os.WriteFile(output, stream.Body.Bytes(), 0600)
		metadata, err := s.deps.Media.Probe(t.Context(), output)
		if err != nil || len(metadata.Streams) == 0 || metadata.Streams[0].Codec != "h264" {
			t.Fatalf("bad conversion: %v %+v", err, metadata)
		}
		if mode == "TRANSCODE" && metadata.Streams[0].Profile != "Constrained Baseline" {
			t.Fatal(metadata.Streams[0])
		}
		call(s, "DELETE", "/v1/playback/"+plan.SessionID, "", "media-device", token, "")
	}
}

func TestProfileEvidenceDoesNotRejectOtherProfiles(t *testing.T) {
	state := func(id string) string {
		if id == "h264-720-main" {
			return "FAIL"
		}
		if id == "h264-1080-high" {
			return "PASS"
		}
		return "UNKNOWN"
	}
	if !videoCandidate(media.Stream{Profile: "Constrained Baseline", Width: 640, Height: 360}, state) {
		t.Fatal("Main failure rejected Baseline")
	}
	if videoCandidate(media.Stream{Profile: "Main", Width: 1280, Height: 720}, state) {
		t.Fatal("failed Main probe ignored")
	}
	if !videoCandidate(media.Stream{Profile: "High", Width: 1920, Height: 1080}, state) {
		t.Fatal("measured 1080 High ignored")
	}
}
