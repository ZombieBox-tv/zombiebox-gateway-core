package server

import (
	"context"
	"strings"
	"time"
	"unicode"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/providers"
)

const (
	youtubeActivityRecommendationTimeout = 8 * time.Second
	youtubeActivityRecommendationMaxAge  = 30 * 24 * time.Hour
	youtubeActivityRecommendationSeeds   = 2
	youtubeActivitySuggestionsPerSeed    = 6
	youtubeActivitySuggestionLimit       = 12
)

// youtubeActivityRecommendations runs a small number of ordinary title searches
// from this device's recent watch history. These are search suggestions, not
// YouTube account recommendations or an official related-videos feed.
func (s *Server) youtubeActivityRecommendations(ctx context.Context, device string, activity []domain.Progress) []domain.Item {
	if ctx.Err() != nil || s.deps.Browse == nil {
		return nil
	}

	s.mu.Lock()
	config, revision := s.config(ctx, "youtube"), s.configRevision["youtube"]
	s.mu.Unlock()
	if !config.Enabled {
		return nil
	}

	watched := make(map[string]struct{}, len(activity)*2)
	queries := make([]string, 0, youtubeActivityRecommendationSeeds)
	seenQueries := make(map[string]struct{}, youtubeActivityRecommendationSeeds)
	now := time.Now()
	for _, record := range activity {
		if now.Sub(time.Unix(record.UpdatedAt, 0)) > youtubeActivityRecommendationMaxAge {
			break
		}
		query := youtubeActivitySearchQuery(record.Item)
		if query == "" {
			continue
		}
		key := strings.ToLower(query)
		if _, exists := seenQueries[key]; !exists {
			seenQueries[key] = struct{}{}
			queries = append(queries, query)
		}
		if len(queries) >= youtubeActivityRecommendationSeeds {
			break
		}
	}
	for _, record := range activity {
		id := strings.TrimSpace(record.Item.ID)
		if id == "" {
			continue
		}
		watched[id] = struct{}{}
		watched[strings.TrimPrefix(id, "youtube-")] = struct{}{}
	}
	if len(queries) == 0 {
		return nil
	}

	suggestions := make([]domain.Item, 0, youtubeActivitySuggestionLimit)
	seen := make(map[string]struct{}, youtubeActivitySuggestionLimit)
	for _, query := range queries {
		if ctx.Err() != nil {
			return suggestions
		}
		result, err := s.deps.Browse.Browse(ctx, "youtube", config, "", query, 0)
		if err != nil {
			if ctx.Err() != nil {
				return suggestions
			}
			continue
		}

		s.mu.Lock()
		configUnchanged := revision == s.configRevision["youtube"]
		s.mu.Unlock()
		if !configUnchanged || ctx.Err() != nil {
			return suggestions
		}

		selected := make([]domain.Source, 0, youtubeActivitySuggestionsPerSeed)
		for _, source := range result.Sources {
			item := source.Item
			if item.Provider != "youtube" || item.Kind != "video" || !item.Playable {
				continue
			}
			if _, exists := watched[item.ID]; exists {
				continue
			}
			if _, exists := watched[strings.TrimPrefix(item.ID, "youtube-")]; exists {
				continue
			}
			if _, exists := seen[item.ID]; exists {
				continue
			}
			seen[item.ID] = struct{}{}
			selected = append(selected, source)
			if len(selected) >= youtubeActivitySuggestionsPerSeed {
				break
			}
		}
		if len(selected) == 0 {
			continue
		}
		result.Sources = selected
		page := s.browse.Present(
			ctx,
			device,
			"youtube",
			providers.Titles["youtube"],
			revision,
			config,
			"",
			query,
			0,
			result,
		)
		suggestions = append(suggestions, page.Items...)
		if len(suggestions) >= youtubeActivitySuggestionLimit {
			break
		}
	}
	return suggestions
}

func youtubeActivitySearchQuery(item domain.Item) string {
	title := strings.TrimSpace(strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return ' '
		}
		return char
	}, item.Title))
	title = strings.Join(strings.Fields(title), " ")
	if title == "" || strings.EqualFold(title, "youtube") || strings.EqualFold(title, "youtube video") {
		videoID := strings.TrimPrefix(strings.TrimSpace(item.ID), "youtube-")
		if youtubeVideoIDPattern.MatchString(videoID) {
			return videoID
		}
		return ""
	}
	runes := []rune(title)
	if len(runes) < 3 {
		return ""
	}
	if len(runes) > 120 {
		title = string(runes[:120])
	}
	return title
}
