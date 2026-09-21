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
