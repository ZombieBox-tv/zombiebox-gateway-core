package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/media"

	"zombiebox.local/gateway/internal/companion"
	"zombiebox.local/gateway/internal/companionmedia"
	"zombiebox.local/gateway/internal/domain"
)

func TestCompanionMediaUploadHandoffAndRevocation(t *testing.T) {
	s := testServer(t, nil, "")
	disk, err := companionmedia.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	s.deps.Uploads = disk
	s.deps.Media = &trackMedia{}
	tv := pair(t, s, "media-tv")
	inv, _ := s.companions.Invite(t.Context(), "media-tv", "TV")
	request, token, _ := s.companions.Join(t.Context(), "127.0.0.1", companion.Join{Code: inv.Code, Name: "Phone"})
	if err = s.companions.Decide(t.Context(), "media-tv", "TV", request.ID, true); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 32)
	path := "/v1/companion/media/" + id
	file := "\x00\x00\x00\x18ftypisomcontents"
	if w := call(s, "PUT", path, file, "media-tv", tv, ""); w.Code != 401 {
		t.Fatal("device token accepted", w.Code)
	}
	if w := call(s, "PUT", path, file, request.ID, token, ""); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(s, "POST", path+"/play", `{"title":"My file","receiverId":"unapproved-tv"}`, request.ID, token, ""); w.Code != 409 {
		t.Fatal("missing receiver consent", w.Code, w.Body)
	}
	w := call(s, "PUT", "/v1/device/preferences", `{"mode":"TV","uiLanguage":"en","subtitleMode":"auto","allowCasting":true}`, "media-tv", tv, "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	for i := 0; i < 2; i++ {
		w = call(s, "POST", path+"/play", `{"title":"My file","receiverId":"unapproved-tv"}`, request.ID, token, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if len(s.casts) != 1 || len(s.sessions) != 1 {
		t.Fatal("duplicate playback")
	}
	w = call(s, "GET", "/v1/cast/active", "", "media-tv", tv, "")
	var active struct{ Plan domain.Plan }
	if json.Unmarshal(w.Body.Bytes(), &active) != nil || active.Plan.Live || active.Plan.Item.Kind != "audio" || active.Plan.Item.Title != "My file" {
		t.Fatal(w.Body)
	}
	// No MediaMTX dependency, local path or receiver stream ticket in phone receipts.
	w = call(s, "GET", "/v1/companion/media", "", request.ID, token, "")
	if !strings.Contains(w.Body.String(), `"state":"ACCEPTED"`) || strings.Contains(w.Body.String(), "ticket") {
		t.Fatal(w.Body)
	}
	if w = call(s, "GET", active.Plan.URL, "", "", "", ""); w.Code != 200 || w.Body.String() != file {
		t.Fatal(w.Code, w.Body)
	}
	a, _ := disk.Get(request.ID, id)
	w = call(s, "DELETE", "/v1/device/companions/"+request.ID, "", "media-tv", tv, "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if _, err = os.Stat(a.Path); !os.IsNotExist(err) {
		t.Fatal("revoked file retained")
	}
	if w = call(s, "GET", active.Plan.URL, "", "", "", ""); w.Code != 401 {
		t.Fatal("revoked stream alive")
	}
}

func TestPhoneAudioUploadConvertsThroughOwnedStream(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("FFprobe unavailable")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "sample.flac")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "0.2", "-c:a", "flac", input).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	file, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t, nil, "")
	disk, _ := companionmedia.New(t.TempDir())
	defer disk.Close()
	s.deps.Uploads = disk
	s.deps.Media = media.New(ffmpeg, ffprobe)
	tv := pair(t, s, "file-television")
	_ = call(s, "PUT", "/v1/device/preferences", `{"mode":"TV","uiLanguage":"en","subtitleMode":"auto","allowCasting":true}`, "file-television", tv, "")
	inv, _ := s.companions.Invite(ctx, "file-television", "TV")
	request, token, err := s.companions.Join(ctx, "127.0.0.1", companion.Join{Code: inv.Code, Name: "Phone"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.companions.Decide(ctx, "file-television", "TV", request.ID, true); err != nil {
		t.Fatal(err)
	}
	path := "/v1/companion/media/" + strings.Repeat("e", 32)
	if w := call(s, "PUT", path, string(file), request.ID, token, ""); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(s, "POST", path+"/play", `{"title":"Sine"}`, request.ID, token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w := call(s, "GET", "/v1/cast/active", "", "file-television", tv, "")
	var active struct{ Plan domain.Plan }
	if err = json.Unmarshal(w.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if active.Plan.Mode != "TRANSCODE" || active.Plan.Live || active.Plan.Seekable || active.Plan.Item.Kind != "audio" {
		t.Fatal(active.Plan)
	}
	w = call(s, "GET", active.Plan.URL, "", "", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	output := filepath.Join(dir, "output.mp4")
	if err = os.WriteFile(output, w.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	metadata, err := media.New(ffmpeg, ffprobe).Probe(ctx, output)
	if err != nil || len(metadata.Streams) != 1 || metadata.Streams[0].Codec != "aac" {
		t.Fatal("expected AAC without video", metadata, err)
	}
	if w = call(s, "PUT", "/v1/playback/"+active.Plan.SessionID+"/progress", `{"state":"ENDED","positionMs":200,"durationMs":200}`, "file-television", tv, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w = call(s, "GET", active.Plan.URL, "", "", "", ""); w.Code != 401 {
		t.Fatal("ended stream survived")
	}
	if len(s.casts) != 0 {
		t.Fatal("ended receiver survived")
	}
}
