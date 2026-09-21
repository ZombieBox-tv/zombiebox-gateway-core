package providers

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

type plexNode struct {
	Key       string `xml:"key,attr"`
	RatingKey string `xml:"ratingKey,attr"`
	Title     string `xml:"title,attr"`
	Summary   string `xml:"summary,attr"`
	Thumb     string `xml:"thumb,attr"`
	Type      string `xml:"type,attr"`
	Duration  int64  `xml:"duration,attr"`
	Media     []struct {
		Parts []struct {
			Key string `xml:"key,attr"`
		} `xml:"Part"`
	} `xml:"Media"`
}

func (a *Adapters) browsePlex(ctx context.Context, c Config, parent, query string, offset int) (domain.BrowseResult, error) {
	if c.URL == "" || c.Token == "" {
		return domain.BrowseResult{}, errors.New("configuration required")
	}
	base := strings.TrimRight(c.URL, "/")
	path := "/library/sections"
	if strings.HasPrefix(parent, "section:") {
		path += "/" + url.PathEscape(strings.TrimPrefix(parent, "section:")) + "/all"
	}
	if strings.HasPrefix(parent, "metadata:") {
		path = "/library/metadata/" + url.PathEscape(strings.TrimPrefix(parent, "metadata:")) + "/children"
	}
	q := url.Values{"X-Plex-Container-Start": {strconv.Itoa(offset)}, "X-Plex-Container-Size": {"40"}}
	if query != "" {
		q.Set("title", query)
	}
	if parent == "" {
		q.Del("X-Plex-Container-Start")
		q.Del("X-Plex-Container-Size")
	}
	headers := http.Header{"X-Plex-Token": {c.Token}, "X-Plex-Client-Identifier": {"zombie-box-tv"}}
	body, err := a.request(ctx, base+path+"?"+q.Encode(), headers)
	if err != nil {
		return domain.BrowseResult{}, err
	}
	var data struct {
		Total       int        `xml:"totalSize,attr"`
		Directories []plexNode `xml:"Directory"`
		Videos      []plexNode `xml:"Video"`
		Tracks      []plexNode `xml:"Track"`
	}
	if err := xml.Unmarshal(body, &data); err != nil {
		return domain.BrowseResult{}, err
	}
	result := domain.BrowseResult{Sources: []Source{}, NextOffset: -1}
	for _, node := range append(append(data.Directories, data.Videos...), data.Tracks...) {
		source := Source{
			Headers:        headers,
			ArtworkHeaders: headers,
			Item: domain.Item{
				ID:          "plex-" + node.RatingKey,
				Provider:    "plex",
				Kind:        node.Type,
				Title:       node.Title,
				Description: node.Summary,
				DurationMS:  node.Duration,
			},
		}
		if strings.HasPrefix(node.Thumb, "/") && !strings.HasPrefix(node.Thumb, "//") {
			source.ArtworkURL = base + node.Thumb
		}
		if len(node.Media) > 0 && len(node.Media[0].Parts) > 0 {
			key := node.Media[0].Parts[0].Key
			if !strings.HasPrefix(key, "/") || strings.HasPrefix(key, "//") {
				continue
			}
			source.URL, source.MIME, source.Item.Playable = base+key, "video/mp4", true
			if node.Type == "track" {
				source.MIME = "audio/mpeg"
			}
		} else if parent == "" {
			source.Item.ID = "plex-section-" + node.Key
			source.BrowsePath = "section:" + node.Key
		} else if node.RatingKey != "" {
			source.BrowsePath = "metadata:" + node.RatingKey
		} else {
			continue
		}
		result.Sources = append(result.Sources, source)
		if parent != "" && len(result.Sources) == 40 {
			break
		}
	}
	if parent == "" {
		return sourcePage(matchingSources(result.Sources, query), offset), nil
	}
	if offset+len(result.Sources) < data.Total {
		result.NextOffset = offset + len(result.Sources)
	}
	// Some servers omit totalSize. A full page permits one bounded follow-up.
	if data.Total == 0 && len(result.Sources) == 40 {
		result.NextOffset = offset + 40
	}
	if len(result.Sources) == 0 {
		result.NextOffset = -1
	}
	return result, nil
}
