package providers

import "context"

// Definition describes an adapter, not the health of its optional process/account.
// Keep dispatch and advertised features together so a UI entry cannot claim a
// working catalog merely because its name is registered.
type Definition struct {
	ID       string
	Features []string
	Fetch    func(context.Context, Config) ([]Source, error)
}

var catalogAdapters = map[string]Definition{
	"youtube":  {"youtube", []string{"catalog", "search", "playback"}, YouTube},
	"plex":     {"plex", []string{"catalog", "playback"}, Plex},
	"jellyfin": {"jellyfin", []string{"catalog", "playback"}, Jellyfin},
	"stremio":  {"stremio", []string{"catalog", "playback"}, Stremio},
	"iptv":     {"iptv", []string{"catalog", "playback", "epg"}, IPTV},
	"spotify":  {"spotify", []string{"catalog", "playback", "now-playing", "remote-control"}, Spotify},
	"airplay":  {"airplay", []string{"catalog", "playback", "screen-receiver"}, AirPlay},
}

func HasCatalog(id string) bool {
	_, ok := catalogAdapters[id]
	return id == "local" || ok
}

func Features(id string) []string {
	if id == "local" {
		return []string{"catalog", "playback"}
	}
	return append([]string{}, catalogAdapters[id].Features...)
}
