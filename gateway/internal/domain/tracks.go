package domain

// Track IDs are stream indexes scoped to a playback session, never provider IDs.
type Track struct {
	ID         int    `json:"id"`
	Kind       string `json:"kind"`
	Language   string `json:"language"`
	Title      string `json:"title"`
	Default    bool   `json:"default"`
	Forced     bool   `json:"forced"`
	Selectable bool   `json:"selectable"`
}

type TrackInventory struct {
	SubtitleID *int    `json:"subtitleId,omitempty"`
	Available  bool    `json:"available"`
	Tracks     []Track `json:"tracks"`
	AudioID    *int    `json:"audioId,omitempty"`
}

type SubtitleCue struct {
	StartMS int64  `json:"startMs"`
	EndMS   int64  `json:"endMs"`
	Text    string `json:"text"`
}

type MediaSelection struct {
	AudioID    *int
	PositionMS int64
}
