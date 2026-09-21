// Package providers isolates remote metadata and credentials from the client protocol.
package providers

import (
	"errors"
	"net/url"

	"zombiebox.local/gateway/internal/domain"
)

type Config = domain.Config
type Source = domain.Source

var Titles = map[string]string{"youtube_receiver": "YouTube Receiver", "local": "My library", "plex": "Plex", "jellyfin": "Jellyfin", "stremio": "Stremio", "youtube": "YouTube", "spotify": "Spotify", "airplay": "AirPlay", "android_mirror": "Android Mirror", "iptv": "IPTV", "rebrowser": "Browser"}
var Order = []string{"local", "youtube", "plex", "jellyfin", "stremio", "spotify", "iptv", "airplay", "android_mirror", "rebrowser"}

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
