package providers

import (
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/subtitles"
)

const maxSubtitleSources = 32

type plexSubtitle struct {
	Type     int    `xml:"streamType,attr"`
	Key      string `xml:"key,attr"`
	Codec    string `xml:"codec,attr"`
	Language string `xml:"languageCode,attr"`
	Title    string `xml:"displayTitle,attr"`
	Default  int    `xml:"default,attr"`
	Forced   int    `xml:"forced,attr"`
}

// Credentialed attachments must resolve to the configured provider origin.
func sameOriginSubtitle(base, reference string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	r, err := url.Parse(reference)
	if err != nil || reference == "" || r.User != nil || r.Fragment != "" {
		return ""
	}
	target := u.ResolveReference(r)
	if target.Scheme != u.Scheme || !strings.EqualFold(target.Host, u.Host) {
		return ""
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return ""
	}
	return target.String()
}

func plexSubtitles(base string, headers http.Header, streams []plexSubtitle) []domain.SubtitleSource {
	var result []domain.SubtitleSource
	for _, stream := range streams {
		if len(result) == maxSubtitleSources {
			break
		}
		if stream.Type != 3 || !subtitles.TextFormat(stream.Codec) {
			continue
		}
		target := sameOriginSubtitle(base, stream.Key)
		if target == "" {
			continue
		}
		result = append(result, domain.SubtitleSource{URL: target, Headers: headers.Clone(), Codec: stream.Codec, Language: stream.Language, Title: stream.Title, Default: stream.Default == 1, Forced: stream.Forced == 1})
	}
	return result
}

type jellyfinMediaSource struct {
	ID           string `json:"Id"`
	MediaStreams []struct {
		Index                               int
		Type, Codec, Language, DisplayTitle string
		IsExternal, IsDefault, IsForced     bool
	}
}

func jellyfinSubtitles(base, item string, headers http.Header, sources []jellyfinMediaSource) []domain.SubtitleSource {
	var result []domain.SubtitleSource
	// The direct stream uses the primary source; never attach another edition's timing.
	if len(sources) == 0 || sources[0].ID == "" {
		return result
	}
	for _, stream := range sources[0].MediaStreams {
		if len(result) == maxSubtitleSources {
			break
		}
		if stream.Type != "Subtitle" || !stream.IsExternal || stream.Index < 0 || !subtitles.TextFormat(stream.Codec) {
			continue
		}
		target := base + "/Videos/" + url.PathEscape(item) + "/" + url.PathEscape(sources[0].ID) + "/Subtitles/" + strconv.Itoa(stream.Index) + "/Stream.srt"
		result = append(result, domain.SubtitleSource{URL: target, Headers: headers.Clone(), Codec: "srt", Language: stream.Language, Title: stream.DisplayTitle, Default: stream.IsDefault, Forced: stream.IsForced})
	}
	return result
}

type stremioSubtitle struct{ URL, Lang, ID string }

func stremioSubtitles(entries []stremioSubtitle) []domain.SubtitleSource {
	var result []domain.SubtitleSource
	for _, entry := range entries {
		if len(result) == maxSubtitleSources {
			break
		}
		u, err := url.Parse(entry.URL)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		codec := strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), ".")
		if codec == "" {
			codec = "srt"
		}
		if !subtitles.TextFormat(codec) {
			continue
		}
		result = append(result, domain.SubtitleSource{URL: u.String(), Codec: codec, Language: entry.Lang})
	}
	return result
}
