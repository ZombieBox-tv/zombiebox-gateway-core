package server

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var hlsURI = regexp.MustCompile(`URI="([^"]+)"`)

// Replace every URI with an opaque, session-scoped relay path. Provider URLs,
// authorization headers and subscription keys never enter the client playlist.
func (s *Server) rewritePlaylist(sess *session, id, base, body string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(body), "#EXTM3U") {
		return "", errors.New("not HLS")
	}
	origin, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.ctx.Err() != nil {
		return "", sess.ctx.Err()
	}
	var failed bool
	rewrite := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			failed = true
			return ""
		}
		u = origin.ResolveReference(u)
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			failed = true
			return ""
		}
		key := ""
		for k, v := range sess.resources {
			if v == u.String() {
				key = k
				break
			}
		}
		if key == "" {
			if len(sess.resourceOrder) >= 2048 {
				delete(sess.resources, sess.resourceOrder[0])
				sess.resourceOrder = sess.resourceOrder[1:]
			}
			key = randomID(12)
			sess.resources[key] = u.String()
			sess.resourceOrder = append(sess.resourceOrder, key)
		}
		return "/v1/streams/" + id + "/" + key + "?ticket=" + sess.ticket
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// Unsupported content steering and variable interpolation must not escape the relay.
			if strings.HasPrefix(line, "#EXT-X-CONTENT-STEERING") || strings.Contains(line, "{$") {
				return "", errors.New("unsupported HLS extension")
			}
			lines[i] = hlsURI.ReplaceAllStringFunc(line, func(attr string) string { m := hlsURI.FindStringSubmatch(attr); return `URI="` + rewrite(m[1]) + `"` })
		} else {
			lines[i] = rewrite(line)
		}
	}
	if failed {
		return "", errors.New("invalid HLS resource")
	}
	return strings.Join(lines, "\n"), nil
}
