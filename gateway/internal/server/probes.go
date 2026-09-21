package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"zombiebox.local/gateway/internal/devices"
	"zombiebox.local/gateway/internal/domain"
)

type probeAsset struct {
	ID    string `json:"id"`
	File  string `json:"-"`
	URL   string `json:"url"`
	Video bool   `json:"video"`
	Kind  string `json:"kind,omitempty"`
}

var probeAssets = []probeAsset{
	{ID: "h264-baseline-360", File: "baseline-360.mp4", Video: true},
	{ID: "h264-baseline-480", File: "baseline-480.mp4", Video: true},
	{ID: "h264-720-main", File: "main-720.mp4", Video: true},
	{ID: "h264-720-high", File: "high-720.mp4", Video: true},
	{ID: "h264-1080-high", File: "high-1080.mp4", Video: true},
	{ID: "aac", File: "aac.m4a"},
	{ID: "mpegts-h264-aac", File: "baseline.ts", Video: true},
	{ID: "http-fmp4", File: "fragmented.mp4", Video: true},
	{ID: "seek", File: "baseline-360.mp4", Video: true, Kind: "seek"},
	{ID: "pause-resume", File: "baseline-360.mp4", Video: true, Kind: "pause-resume"},
	{ID: "surface-reattach", File: "baseline-360.mp4", Video: true, Kind: "surface-reattach"},
	{ID: "hls-h264-aac", File: "baseline.ts", Video: true, Kind: "hls"},
}

func (s *Server) probeSignature(id, expires string) string {
	mac := hmac.New(sha256.New, []byte(s.probeKey))
	mac.Write([]byte(id + "\n" + expires))
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Server) probeFile(asset probeAsset) (*os.File, error) {
	if s.opt.ProbeDir == "" {
		return nil, os.ErrNotExist
	}
	path := filepath.Join(s.opt.ProbeDir, asset.File)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return nil, os.ErrInvalid
	}
	return os.Open(path)
}
func (s *Server) probeManifest(w http.ResponseWriter, r *http.Request, d domain.Device) {
	assets := []probeAsset{}
	suite := 1
	if r.URL.Query().Get("suite") == "2" {
		suite = devices.ProbeSuiteVersion
	}
	expires := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	for _, asset := range probeAssets {
		if suite == 1 && asset.Kind != "" {
			continue
		}
		f, err := s.probeFile(asset)
		if err != nil {
			continue
		}
		f.Close()
		asset.URL = "/v1/probes/" + asset.ID + "?expires=" + expires + "&ticket=" + s.probeSignature(asset.ID, expires)
		assets = append(assets, asset)
	}
	manifest := map[string]any{"apiVersion": 1, "suiteVersion": suite, "probes": assets}
	if suite == devices.ProbeSuiteVersion {
		manifest["cacheKey"] = devices.ProbeCacheKey(d)
	}
	respond(w, 200, manifest)
}

// Tickets let API9 MediaPlayer fetch only fixed synthetic assets without adding
// provider credentials or relying on the API14 setDataSource(headers) overload.
func (s *Server) probeStream(w http.ResponseWriter, r *http.Request) {
	id, expires := r.PathValue("probe"), r.URL.Query().Get("expires")
	expiry, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || time.Now().Unix() > expiry || expiry > time.Now().Add(10*time.Minute).Unix() || !hmac.Equal([]byte(s.probeSignature(id, expires)), []byte(r.URL.Query().Get("ticket"))) {
		fail(w, 403, "invalid_probe_ticket")
		return
	}
	for _, asset := range probeAssets {
		if asset.ID == id {
			f, err := s.probeFile(asset)
			if err != nil {
				fail(w, 404, "probe_unavailable")
				return
			}
			defer f.Close()
			if asset.Kind == "hls" {
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				w.Header().Set("Cache-Control", "no-store")
				segment := "/v1/probes/mpegts-h264-aac?expires=" + expires + "&ticket=" + s.probeSignature("mpegts-h264-aac", expires)
				fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:3.1,\n%s\n#EXT-X-ENDLIST\n", segment)
				return
			}
			info, err := f.Stat()
			if err != nil {
				fail(w, 404, "probe_unavailable")
				return
			}
			w.Header().Set("Cache-Control", "private, max-age=300")
			http.ServeContent(w, r, asset.File, info.ModTime(), f)
			return
		}
	}
	fail(w, 404, "probe_unavailable")
}
