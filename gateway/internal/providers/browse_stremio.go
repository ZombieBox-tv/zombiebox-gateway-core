package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"zombiebox.local/gateway/internal/domain"
)

type addonNode struct {
	Resource string
	Type     string
	ID       string
	Season   int
	Skip     bool
	Search   bool
	Query    string `json:",omitempty"`
}

func addonPath(node addonNode) string {
	data, _ := json.Marshal(node)
	return string(data)
}

type addonMeta struct {
	ID, Name, Description, Poster string
	Videos                        []struct {
		ID, Title, Name, Overview, Thumbnail string
		Season, Episode                      int
	}
}

func (a *Adapters) browseStremio(ctx context.Context, c Config, parent, query string, offset int) (domain.BrowseResult, error) {
	if c.URL == "" {
		return domain.BrowseResult{}, errors.New("configuration required")
	}
	base := strings.TrimSuffix(strings.TrimRight(c.URL, "/"), "/manifest.json")
	if parent == "" {
		return a.addonCatalogs(ctx, base, query, offset)
	}
	var node addonNode
	if json.Unmarshal([]byte(parent), &node) != nil {
		return domain.BrowseResult{}, errors.New("invalid addon node")
	}
	if node.Resource == "catalog" {
		// Search continuations keep their scope in the server-owned locator. The
		// client does not need upstream catalog IDs or addon query syntax.
		if query == "" {
			query = node.Query
		}
		return a.addonPage(ctx, base, node, query, offset)
	}
	body, err := a.request(ctx, base+"/meta/"+url.PathEscape(node.Type)+"/"+url.PathEscape(node.ID)+".json", nil)
	if err != nil {
		return domain.BrowseResult{}, err
	}
	var result struct{ Meta addonMeta }
	if err := json.Unmarshal(body, &result); err != nil {
		return domain.BrowseResult{}, err
	}
	sources := []Source{}
	if node.Resource == "seasons" {
		seasons := map[int]bool{}
		for _, video := range result.Meta.Videos {
			if video.Season >= 0 {
				seasons[video.Season] = true
			}
		}
		order := []int{}
		for season := range seasons {
			order = append(order, season)
		}
		sort.Ints(order)
		for _, season := range order {
			child := node
			child.Resource = "episodes"
			child.Season = season
			sources = append(sources, Source{
				BrowsePath: addonPath(child),
				ArtworkURL: result.Meta.Poster,
				Item: domain.Item{
					ID:       fmt.Sprintf("stremio-%s-season-%d", node.ID, season),
					Provider: "stremio",
					Kind:     "season",
					Title:    fmt.Sprintf("Season %d", season),
				},
			})
		}
	} else {
		sort.SliceStable(result.Meta.Videos, func(i, j int) bool { return result.Meta.Videos[i].Episode < result.Meta.Videos[j].Episode })
		for _, video := range result.Meta.Videos {
			if video.Season != node.Season {
				continue
			}
			title := video.Title
			if title == "" {
				title = video.Name
			}
			if title == "" {
				title = fmt.Sprintf("Episode %d", video.Episode)
			}
			source := addonVideo(base, node.Type, addonMeta{ID: video.ID, Name: title, Description: video.Overview, Poster: video.Thumbnail})
			source.Item.Kind = "episode"
			sources = append(sources, source)
		}
	}
	return sourcePage(matchingSources(sources, query), offset), nil
}

func (a *Adapters) addonCatalogs(ctx context.Context, base, query string, offset int) (domain.BrowseResult, error) {
	body, err := a.request(ctx, base+"/manifest.json", nil)
	if err != nil {
		return domain.BrowseResult{}, err
	}
	var manifest struct {
		Catalogs []struct {
			ID, Type, Name string
			Extra          []struct{ Name string }
		}
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return domain.BrowseResult{}, err
	}
	sources := []Source{}
	searched, succeeded := 0, 0
	seen := map[string]bool{}
	for _, catalog := range manifest.Catalogs {
		node := addonNode{Resource: "catalog", Type: catalog.Type, ID: catalog.ID}
		for _, extra := range catalog.Extra {
			if extra.Name == "skip" {
				node.Skip = true
			}
			if extra.Name == "search" {
				node.Search = true
			}
		}
		if query != "" {
			if !node.Search || searched >= 4 {
				continue
			}
			searched++
			page, err := a.addonPage(ctx, base, node, query, 0)
			if err != nil {
				continue
			}
			succeeded++
			if page.NextOffset >= 0 {
				node.Query = query
				title := catalog.Name
				if title == "" {
					title = catalog.ID
				}
				sources = append(sources, Source{
					BrowsePath: addonPath(node),
					Item: domain.Item{
						ID:       "stremio-search-catalog-" + catalog.Type + "-" + catalog.ID,
						Provider: "stremio", Kind: "library", Title: title,
						Subtitle: query,
					},
				})
			}
			for _, source := range page.Sources {
				if !seen[source.Item.ID] {
					seen[source.Item.ID] = true
					sources = append(sources, source)
				}
			}
			continue
		}
		title := catalog.Name
		if title == "" {
			title = catalog.ID
		}
		sources = append(sources, Source{
			BrowsePath: addonPath(node),
			Item: domain.Item{
				ID:       "stremio-catalog-" + catalog.Type + "-" + catalog.ID,
				Provider: "stremio",
				Kind:     "library",
				Title:    title,
			},
		})
	}
	if query != "" && succeeded == 0 {
		return domain.BrowseResult{}, errors.New("addon search unavailable")
	}
	return sourcePage(sources, offset), nil
}

func (a *Adapters) addonPage(ctx context.Context, base string, node addonNode, query string, offset int) (domain.BrowseResult, error) {
	start := 0
	extra := url.Values{}
	if node.Skip {
		start = (offset / 100) * 100
		if start > 0 {
			extra.Set("skip", strconv.Itoa(start))
		}
	}
	if query != "" {
		if !node.Search {
			return domain.BrowseResult{}, errors.New("catalog search unsupported")
		}
		extra.Set("search", query)
	}
	path := base + "/catalog/" + url.PathEscape(node.Type) + "/" + url.PathEscape(node.ID)
	if len(extra) > 0 {
		path += "/" + extra.Encode()
	}
	body, err := a.request(ctx, path+".json", nil)
	if err != nil {
		return domain.BrowseResult{}, err
	}
	var data struct{ Metas []addonMeta }
	if err := json.Unmarshal(body, &data); err != nil {
		return domain.BrowseResult{}, err
	}
	sources := []Source{}
	for _, meta := range data.Metas {
		source := addonVideo(base, node.Type, meta)
		if node.Type == "series" {
			source.Item.Playable = false
			source.BrowsePath = addonPath(addonNode{Resource: "seasons", Type: node.Type, ID: meta.ID})
		}
		sources = append(sources, source)
	}
	page := sourcePage(sources, offset-start)
	if page.NextOffset >= 0 {
		page.NextOffset += start
	} else if node.Skip && len(sources) >= 100 && len(page.Sources) > 0 {
		page.NextOffset = start + len(sources)
	}
	return page, nil
}

func addonVideo(base, kind string, meta addonMeta) Source {
	return Source{
		ArtworkURL: meta.Poster,
		Item:       domain.Item{ID: "stremio-" + kind + "-" + meta.ID, Provider: "stremio", Kind: kind, Title: meta.Name, Description: meta.Description, Playable: true},
		URL:        base + "/stream/" + url.PathEscape(kind) + "/" + url.PathEscape(meta.ID) + ".json",
		MIME:       "application/x-zombie-stremio",
	}
}
