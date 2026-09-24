package domain

// ExternalSubtitleBase reserves session-local IDs for gateway-owned attachments.
const ExternalSubtitleBase = 1600000000

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
	Quality    string
	AudioID    *int
	PositionMS int64
}

type QualityOption struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

type QualityInventory struct {
	SelectedID string          `json:"selectedId"`
	Options    []QualityOption `json:"options"`
}

type QualitySelection struct {
	QualityID  string `json:"qualityId"`
	PositionMS int64  `json:"positionMs"`
}
