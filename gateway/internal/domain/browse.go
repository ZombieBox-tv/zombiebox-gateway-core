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
