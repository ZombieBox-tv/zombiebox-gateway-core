package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"zombiebox.local/gateway/internal/domain"
)

type probeAsset struct {
	ID    string `json:"id"`
	File  string `json:"-"`
	URL   string `json:"url"`
	Video bool   `json:"video"`
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
	expires := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	for _, asset := range probeAssets {
		f, err := s.probeFile(asset)
		if err != nil {
			continue
		}
		f.Close()
		asset.URL = "/v1/probes/" + asset.ID + "?expires=" + expires + "&ticket=" + s.probeSignature(asset.ID, expires)
		assets = append(assets, asset)
	}
	respond(w, 200, map[string]any{"apiVersion": 1, "suiteVersion": 1, "probes": assets})
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
