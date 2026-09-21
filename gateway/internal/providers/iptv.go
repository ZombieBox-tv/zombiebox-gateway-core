// Package providers isolates remote metadata and credentials from the client protocol.
package providers

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

func (a *Adapters) IPTV(ctx context.Context, c Config) ([]Source, error) {
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
		body, err = a.request(ctx, c.URL, headers)
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
		if guideBody, e := a.request(ctx, c.EPGURL, nil); e == nil {
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
