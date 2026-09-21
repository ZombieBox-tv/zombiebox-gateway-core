package manifest

import (
	"errors"
	"regexp"
	"strings"
)

var uriAttribute = regexp.MustCompile(`URI="([^"]+)"`)

func (p *Proxy) hls(base string, body []byte, depth int) ([]byte, error) {
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "#EXTM3U") {
		return nil, errors.New("not HLS")
	}
	lines := strings.Split(string(body), "\n")
	variant := false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) > 16384 || strings.Contains(line, "{$") || strings.HasPrefix(line, "#EXT-X-CONTENT-STEERING") || strings.HasPrefix(line, "#EXT-X-DEFINE") {
			return nil, errors.New("unsupported HLS extension")
		}
		if strings.HasPrefix(line, "#EXT-X-KEY:") && line != "#EXT-X-KEY:METHOD=NONE" || strings.HasPrefix(line, "#EXT-X-SESSION-KEY:") {
			return nil, errors.New("encrypted HLS unsupported")
		}
		if strings.HasPrefix(line, "#") {
			if strings.Contains(uriAttribute.ReplaceAllString(line, ""), "URI=") {
				return nil, errors.New("unquoted HLS resource")
			}
			kind := "segment"
			if strings.HasPrefix(line, "#EXT-X-MEDIA:") || strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:") || strings.HasPrefix(line, "#EXT-X-RENDITION-REPORT:") {
				kind = "hls"
			}
			var rewriteErr error
			line = uriAttribute.ReplaceAllStringFunc(line, func(attr string) string {
				raw, err := resolve(base, uriAttribute.FindStringSubmatch(attr)[1])
				if err != nil {
					rewriteErr = err
					return ""
				}
				mapped, err := p.register(raw, kind, depth+1, nil)
				if err != nil {
					rewriteErr = err
					return ""
				}
				return `URI="` + mapped + `"`
			})
			if rewriteErr != nil {
				return nil, rewriteErr
			}
			if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
				variant = true
			}
		} else if line != "" {
			kind := "segment"
			if variant {
				kind = "hls"
			}
			raw, err := resolve(base, line)
			if err != nil {
				return nil, err
			}
			line, err = p.register(raw, kind, depth+1, nil)
			if err != nil {
				return nil, err
			}
			variant = false
		}
		lines[i] = line
	}
	return []byte(strings.Join(lines, "\n")), nil
}
