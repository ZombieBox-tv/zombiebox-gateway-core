package domain

// BrowseResult is adapter output. Sources and upstream paths never cross the API.
type BrowseResult struct {
	Sources    []Source
	NextOffset int
}

type BrowsePage struct {
	Items      []Item `json:"items"`
	Title      string `json:"title"`
	NextOffset int    `json:"nextOffset"`
}

type RelatedPage struct {
	APIVersion   int    `json:"apiVersion"`
	VideoID      string `json:"videoId"`
	CurrentVideo *Item  `json:"currentVideo,omitempty"`
	Items        []Item `json:"items"`
	NextCursor   string `json:"nextCursor,omitempty"`
	HasMore      bool   `json:"hasMore"`
	Total        int    `json:"total"`
}
