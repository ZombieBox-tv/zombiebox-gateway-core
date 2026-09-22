package subtitles

import (
	"errors"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"zombiebox.local/gateway/internal/domain"
)

var markup = regexp.MustCompile(`<[^>]*>|\{\\[^}]*\}`)

// ParseSubtitles normalizes bounded SRT/WebVTT into plain text. Styling and script
// markup never reach the client renderer. FFmpeg simplifies ASS into SRT first.
func ParseSubtitles(data []byte) ([]domain.SubtitleCue, error) {
	if len(data) > 2<<20 || !utf8.Valid(data) {
		return nil, errors.New("invalid subtitle size or encoding")
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n")
	cues := []domain.SubtitleCue{}
	for _, block := range strings.Split(text, "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		for i, line := range lines {
			if !strings.Contains(line, " --> ") {
				continue
			}
			span := strings.SplitN(line, " --> ", 2)
			endFields := strings.Fields(span[1])
			if len(endFields) == 0 {
				return nil, errors.New("missing subtitle end")
			}
			start, err := timestamp(span[0])
			if err != nil {
				return nil, err
			}
			end, err := timestamp(endFields[0])
			if err != nil || end <= start {
				return nil, errors.New("invalid subtitle interval")
			}
			body := strings.TrimSpace(html.UnescapeString(markup.ReplaceAllString(strings.Join(lines[i+1:], "\n"), "")))
			if len(body) > 4096 || len(cues) >= 5000 {
				return nil, errors.New("subtitle cue limit")
			}
			if body != "" {
				cues = append(cues, domain.SubtitleCue{StartMS: start, EndMS: end, Text: body})
			}
			break
		}
	}
	return cues, nil
}

func timestamp(value string) (int64, error) {
	parts := strings.Split(strings.ReplaceAll(strings.TrimSpace(value), ",", "."), ":")
	if len(parts) == 2 {
		parts = append([]string{"0"}, parts...)
	}
	if len(parts) != 3 {
		return 0, errors.New("invalid subtitle timestamp")
	}
	h, e1 := strconv.ParseInt(parts[0], 10, 32)
	m, e2 := strconv.ParseInt(parts[1], 10, 32)
	seconds := strings.Split(parts[2], ".")
	if len(seconds) != 2 || len(seconds[1]) != 3 {
		return 0, errors.New("invalid subtitle milliseconds")
	}
	s, e3 := strconv.ParseInt(seconds[0], 10, 32)
	ms, e4 := strconv.ParseInt(seconds[1], 10, 32)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || h < 0 || h > 167 || m < 0 || m > 59 || s < 0 || s > 59 || ms < 0 || ms > 999 {
		return 0, errors.New("invalid subtitle timestamp")
	}
	return ((h*60+m)*60+s)*1000 + ms, nil
}

// TextFormat describes standalone text files that the gateway can normalize.
func TextFormat(codec string) bool {
	switch strings.ToLower(codec) {
	case "srt", "subrip", "vtt", "webvtt", "ass", "ssa":
		return true
	default:
		return false
	}
}
