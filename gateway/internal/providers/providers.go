// Package providers isolates remote metadata and credentials from the client protocol.
package providers

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"zombiebox.local/gateway/internal/domain"
)

type Config struct {
	Enabled      bool   `json:"enabled"`
	URL          string `json:"url,omitempty"`
	Token        string `json:"token,omitempty"`
	UserID       string `json:"userId,omitempty"`
	PlaylistPath string `json:"playlistPath,omitempty"`
	EPGURL       string `json:"epgUrl,omitempty"`
	CatalogID    string `json:"catalogId,omitempty"`
	MediaType    string `json:"mediaType,omitempty"`
}
type Source struct {
	EPGID   string
	Item    domain.Item
	URL     string
	Path    string
	Headers http.Header
	MIME    string
	Live    bool
}

var Titles = map[string]string{"local": "My library", "plex": "Plex", "jellyfin": "Jellyfin", "stremio": "Stremio", "youtube": "YouTube", "spotify": "Spotify", "airplay": "AirPlay", "android_mirror": "Android Mirror", "iptv": "IPTV", "rebrowser": "Browser"}
var Order = []string{"local", "youtube", "plex", "jellyfin", "stremio", "spotify", "iptv", "airplay", "android_mirror", "rebrowser"}
var Client = &http.Client{Timeout: 5 * 1000000000, CheckRedirect: SafeRedirect}

func Validate(c Config) error {
	for _, raw := range []string{c.URL, c.EPGURL} {
		if raw == "" {
			continue
		}
		u, e := url.Parse(raw)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return errors.New("invalid URL")
		}
	}
	if len(c.Token) > 4096 || len(c.URL) > 4096 || len(c.UserID) > 200 || len(c.CatalogID) > 200 || len(c.EPGURL) > 4096 || len(c.PlaylistPath) > 4096 || len(c.MediaType) > 40 {
		return errors.New("configuration too large")
	}
	return nil
}
func request(ctx context.Context, raw string, headers http.Header) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	res, err := Client.Do(req)
	if err != nil {
		return nil, errors.New("provider unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("provider HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20+1))
	if len(body) > 8<<20 {
		return nil, errors.New("provider response too large")
	}
	return body, err
}
func Fetch(ctx context.Context, id string, c Config, mediaDir string) ([]Source, error) {
	if id == "local" {
		return Local(mediaDir)
	}
	if !c.Enabled {
		return []Source{}, nil
	}
	switch id {
	case "youtube":
		return YouTube(ctx, c)
	case "iptv":
		return IPTV(ctx, c)
	case "jellyfin":
		return Jellyfin(ctx, c)
	case "plex":
		return Plex(ctx, c)
	case "stremio":
		return Stremio(ctx, c)
	}
	return nil, errors.New("adapter not implemented")
}
func Local(dir string) ([]Source, error) {
	out := []Source{}
	if dir == "" {
		return out, nil
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if len(out) >= 100 {
			break
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		mime := "video/mp4"
		kind := "video"
		switch ext {
		case ".mp4", ".m4v":
		case ".mp3":
			mime = "audio/mpeg"
			kind = "track"
		case ".m4a":
			mime = "audio/mp4"
			kind = "track"
		default:
			continue
		}
		sum := sha256.Sum256([]byte(entry.Name()))
		id := "local-" + hex.EncodeToString(sum[:8])
		title := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		out = append(out, Source{Item: domain.Item{ID: id, Provider: "local", Kind: kind, Title: title, Description: "From your gateway library", Playable: true}, Path: filepath.Join(root, entry.Name()), MIME: mime})
	}
	return out, nil
}
func IPTV(ctx context.Context, c Config) ([]Source, error) {
	var body []byte
	var err error
	if c.PlaylistPath != "" {
		f, e := os.Open(c.PlaylistPath)
		if e != nil {
			return nil, errors.New("playlist unavailable")
		}
		defer f.Close()
		body, err = io.ReadAll(io.LimitReader(f, 8<<20+1))
	} else {
		headers := http.Header{}
		if c.Token != "" {
			headers.Set("Authorization", "Bearer "+c.Token)
		}
		body, err = request(ctx, c.URL, headers)
	}
	if err != nil {
		return nil, err
	}
	if len(body) > 8<<20 {
		return nil, errors.New("playlist too large")
	}
	sources, err := ParseM3U(body, c.URL)
	if err != nil {
		return nil, err
	}
	origin, _ := url.Parse(c.URL)
	for i := range sources {
		target, _ := url.Parse(sources[i].URL)
		if c.Token != "" && origin != nil && target != nil && origin.Host == target.Host && origin.Scheme == target.Scheme {
			sources[i].Headers = http.Header{"Authorization": []string{"Bearer " + c.Token}}
		}
	}
	if c.EPGURL != "" {
		if guideBody, e := request(ctx, c.EPGURL, nil); e == nil {
			now := time.Now()
			if guide, e := ParseXMLTV(guideBody, now); e == nil {
				for i := range sources {
					sources[i].Item.Programmes = guide[sources[i].EPGID]
					for _, p := range sources[i].Item.Programmes {
						if p.Start <= now.Unix() && p.End > now.Unix() {
							sources[i].Item.Subtitle = p.Title
							break
						}
					}
				}
			}
		}
	}
	return sources, nil
}

var tvgID = regexp.MustCompile(`tvg-id="([^"]*)"`)

func ParseM3U(body []byte, base string) ([]Source, error) {
	scan := bufio.NewScanner(strings.NewReader(string(body)))
	scan.Buffer(make([]byte, 4096), 65536)
	title := ""
	epgID := ""
	out := []Source{}
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if strings.HasPrefix(line, "#EXTINF:") {
			title = ""
			epgID = ""
			if match := tvgID.FindStringSubmatch(line); len(match) == 2 {
				epgID = match[1]
			}
			quoted := false
			for n, ch := range line {
				if ch == '"' {
					quoted = !quoted
				}
				if ch == ',' && !quoted {
					title = strings.TrimSpace(line[n+1:])
					break
				}
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, e := url.Parse(line)
		if e != nil {
			continue
		}
		if !u.IsAbs() {
			b, _ := url.Parse(base)
			if b == nil {
				continue
			}
			u = b.ResolveReference(u)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			continue
		}
		if title == "" {
			title = fmt.Sprintf("Channel %d", len(out)+1)
		}
		sum := sha256.Sum256([]byte(u.String()))
		id := "iptv-" + hex.EncodeToString(sum[:8])
		mime := "video/mp2t"
		if strings.Contains(strings.ToLower(u.Path), "m3u8") {
			mime = "application/vnd.apple.mpegurl"
		}
		out = append(out, Source{EPGID: epgID, Item: domain.Item{ID: id, Provider: "iptv", Kind: "channel", Title: title, Playable: true}, URL: u.String(), MIME: mime, Live: true})
		title = ""
		if len(out) >= 5000 {
			break
		}
	}
	return out, scan.Err()
}
func Jellyfin(ctx context.Context, c Config) ([]Source, error) {
	if c.URL == "" || c.Token == "" || c.UserID == "" {
		return nil, errors.New("configuration required")
	}
	base := strings.TrimRight(c.URL, "/")
	headers := http.Header{"X-Emby-Token": []string{c.Token}}
	q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Movie,Episode,Audio"}, "Limit": {"40"}, "Fields": {"Overview"}, "SortBy": {"DateCreated"}, "SortOrder": {"Descending"}}
	body, err := request(ctx, base+"/Users/"+url.PathEscape(c.UserID)+"/Items?"+q.Encode(), headers)
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []struct {
			ID       string `json:"Id"`
			Name     string
			Overview string
			Type     string
		}
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	out := []Source{}
	for _, x := range result.Items {
		path := "/Videos/"
		mime := "video/mp4"
		kind := "video"
		if x.Type == "Audio" {
			path = "/Audio/"
			mime = "audio/mpeg"
			kind = "track"
		}
		out = append(out, Source{Item: domain.Item{ID: "jellyfin-" + x.ID, Provider: "jellyfin", Kind: kind, Title: x.Name, Description: x.Overview, Playable: true}, URL: base + path + url.PathEscape(x.ID) + "/stream?static=true", Headers: headers, MIME: mime})
	}
	return out, nil
}
func Plex(ctx context.Context, c Config) ([]Source, error) {
	if c.URL == "" || c.Token == "" {
		return nil, errors.New("configuration required")
	}
	base := strings.TrimRight(c.URL, "/")
	headers := http.Header{"X-Plex-Token": []string{c.Token}, "X-Plex-Client-Identifier": []string{"zombie-box-tv"}}
	body, err := request(ctx, base+"/library/recentlyAdded?X-Plex-Container-Size=40", headers)
	if err != nil {
		return nil, err
	}
	var data struct {
		Videos []struct {
			Key     string `xml:"ratingKey,attr"`
			Title   string `xml:"title,attr"`
			Summary string `xml:"summary,attr"`
			Media   []struct {
				Parts []struct {
					Key string `xml:"key,attr"`
				} `xml:"Part"`
			} `xml:"Media"`
		} `xml:"Video"`
	}
	if err = xml.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	out := []Source{}
	for _, v := range data.Videos {
		if len(v.Media) == 0 || len(v.Media[0].Parts) == 0 {
			continue
		}
		key := v.Media[0].Parts[0].Key
		if !strings.HasPrefix(key, "/") || strings.HasPrefix(key, "//") {
			continue
		}
		out = append(out, Source{Item: domain.Item{ID: "plex-" + v.Key, Provider: "plex", Kind: "video", Title: v.Title, Description: v.Summary, Playable: true}, URL: base + key, Headers: headers, MIME: "video/mp4"})
	}
	return out, nil
}
func Stremio(ctx context.Context, c Config) ([]Source, error) {
	if c.URL == "" || c.CatalogID == "" {
		return nil, errors.New("configuration required")
	}
	base := strings.TrimSuffix(strings.TrimRight(c.URL, "/"), "/manifest.json")
	kind := c.MediaType
	if kind == "" {
		kind = "movie"
	}
	body, err := request(ctx, base+"/catalog/"+url.PathEscape(kind)+"/"+url.PathEscape(c.CatalogID)+".json", nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Metas []struct {
			ID          string
			Name        string
			Description string
		}
	}
	if err = json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	out := []Source{}
	for _, m := range data.Metas {
		if len(out) >= 40 {
			break
		}
		out = append(out, Source{Item: domain.Item{ID: "stremio-" + m.ID, Provider: "stremio", Kind: kind, Title: m.Name, Description: m.Description, Playable: true}, URL: base + "/stream/" + url.PathEscape(kind) + "/" + url.PathEscape(m.ID) + ".json", MIME: "application/x-zombie-stremio"})
	}
	return out, nil
}
func Resolve(ctx context.Context, source Source) (Source, error) {
	if source.MIME == "application/x-zombie-youtube" {
		return resolveYouTube(ctx, source)
	}
	if source.MIME != "application/x-zombie-stremio" {
		return source, nil
	}
	body, err := request(ctx, source.URL, nil)
	if err != nil {
		return source, err
	}
	var data struct{ Streams []struct{ URL string } }
	if err = json.Unmarshal(body, &data); err != nil {
		return source, err
	}
	for _, stream := range data.Streams {
		u, e := url.Parse(stream.URL)
		if e == nil && (u.Scheme == "http" || u.Scheme == "https") {
			source.URL = stream.URL
			source.MIME = "video/mp4"
			return source, nil
		}
	}
	return source, errors.New("no HTTP stream; torrents are unsupported")
}
