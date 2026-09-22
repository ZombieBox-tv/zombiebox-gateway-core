package domain

type ReceiverCommand struct {
	ID         string `json:"id"`
	Action     string `json:"action"`
	VideoID    string `json:"videoId,omitempty"`
	ItemID     string `json:"itemId,omitempty"`
	PositionMS int64  `json:"positionMs,omitempty"`
	Volume     int    `json:"volume,omitempty"`
	Muted      bool   `json:"muted,omitempty"`
}
type YouTubeReceiverState struct {
	Epoch      string           `json:"epoch,omitempty"`
	ReceiverID string           `json:"receiverId"`
	State      string           `json:"state"`
	TVCode     string           `json:"tvCode"`
	Command    *ReceiverCommand `json:"command"`
}
type ReceiverAcknowledgement struct {
	Epoch      string `json:"epoch,omitempty"`
	CommandID  string `json:"commandId"`
	Success    bool   `json:"success"`
	State      string `json:"state"`
	PositionMS int64  `json:"positionMs"`
	DurationMS int64  `json:"durationMs"`
	Volume     int    `json:"volume"`
	Muted      bool   `json:"muted"`
}
