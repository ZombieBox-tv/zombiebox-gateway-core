// Package integrations models installed adapter capabilities separately from
// runtime availability. It contains no HTTP server or persistence dependencies.
package integrations

type Definition struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	Runtime        string `json:"runtime"`
	Implementation string `json:"implementation"`
	Full           string `json:"full"`
	Edge           string `json:"edge"`
}

func All() []Definition {
	return []Definition{
		{"local", "local", "filesystem", "integrated", "available", "available"},
		{"ffmpeg", "local", "ffmpeg/ffprobe", "integrated", "available", "native-build"},
		{"youtube", "youtube", "YouTube.js", "partial", "available", "native-build"},
		{"youtube_receiver", "youtube", "yt-cast-receiver", "pending", "pending", "pending"},
		{"plex", "plex", "HTTP adapter", "partial", "available", "available"},
		{"jellyfin", "jellyfin", "HTTP adapter", "partial", "available", "available"},
		{"stremio", "stremio", "addon HTTP adapter", "partial", "available", "available"},
		{"spotify", "spotify", "go-librespot", "partial", "available", "experimental"},
		{"airplay", "airplay", "UxPlay + FFmpeg", "partial", "experimental", "pending"},
		{"mediamtx", "android_mirror", "MediaMTX", "integrated", "available", "native-build"},
		{"iptv", "iptv", "M3U/XMLTV adapter", "integrated", "available", "available"},
		{"threadfin", "iptv", "Threadfin", "partial", "available", "native-build"},
		{"rebrowser", "rebrowser", "Chromium / Puppeteer", "partial", "experimental", "remote-only"},
	}
}
