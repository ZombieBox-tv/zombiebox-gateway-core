package providers

import (
	"context"
	"net/http"
)

// Definition describes an adapter, not the health of its optional process/account.
// Keep dispatch and advertised features together so a UI entry cannot claim a
// working catalog merely because its name is registered.
type Definition struct {
	ID       string
	Features []string
	Fetch    func(context.Context, Config) ([]Source, error)
}

// HTTPClient is implemented by net/http.Client and deterministic test transports.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Adapters struct {
	guides            guideCache
	http, privateHTTP HTTPClient
	catalog           map[string]Definition
}

// New builds an independent registry. Network policy is supplied by the process
// composition root; no mutable package client or registry is shared by servers.
func New(client, privateClient HTTPClient) *Adapters {
	if client == nil || privateClient == nil {
		panic("providers: HTTP clients are required")
	}
	a := &Adapters{http: client, privateHTTP: privateClient}
	a.catalog = map[string]Definition{
		"youtube":  {"youtube", []string{"catalog", "search", "playback"}, a.YouTube},
		"plex":     {"plex", []string{"catalog", "browse", "playback"}, a.Plex},
		"jellyfin": {"jellyfin", []string{"catalog", "browse", "playback"}, a.Jellyfin},
		"stremio":  {"stremio", []string{"catalog", "browse", "playback"}, a.Stremio},
		"iptv":     {"iptv", []string{"catalog", "playback", "epg"}, a.IPTV},
		"spotify":  {"spotify", []string{"catalog", "playback", "now-playing", "remote-control"}, a.Spotify},
		"airplay":  {"airplay", []string{"catalog", "playback", "screen-receiver"}, a.AirPlay},
	}

	return a
}

func (a *Adapters) HasCatalog(id string) bool {
	_, ok := a.catalog[id]
	return id == "local" || ok
}

func (a *Adapters) Features(id string) []string {
	if id == "local" {
		return []string{"catalog", "playback"}
	}
	return append([]string{}, a.catalog[id].Features...)
}
