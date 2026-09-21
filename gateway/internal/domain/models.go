package domain

type Platform struct {
	AndroidAPI   int      `json:"androidApi"`
	Release      string   `json:"release"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	ABIs         []string `json:"abis"`
}
type Display struct {
	Width  int  `json:"width"`
	Height int  `json:"height"`
	DPI    int  `json:"dpi"`
	Touch  bool `json:"touch"`
	Dpad   bool `json:"dpad"`
}
type Memory struct {
	PhysicalMB int `json:"physicalMb"`
	ClassMB    int `json:"memoryClassMb"`
}
type Registration struct {
	ClientVersion   string   `json:"clientVersion"`
	ProtocolVersion int      `json:"protocolVersion"`
	InstallationID  string   `json:"installationId"`
	PairingCode     string   `json:"pairingCode,omitempty"`
	Platform        Platform `json:"platform"`
	Display         Display  `json:"display"`
	Memory          Memory   `json:"memory"`
}
type Preferences struct {
	AllowCasting      bool     `json:"allowCasting"`
	Mode              string   `json:"mode"`
	UILanguage        string   `json:"uiLanguage"`
	AudioLanguages    []string `json:"audioLanguages"`
	SubtitleLanguages []string `json:"subtitleLanguages"`
	SubtitleMode      string   `json:"subtitleMode"`
}

func DefaultPreferences() Preferences {
	return Preferences{Mode: "AUTO", UILanguage: "en", AudioLanguages: []string{"en", "es"}, SubtitleLanguages: []string{"en", "es"}, SubtitleMode: "auto"}
}

type Probe struct {
	FirstFrameMS int    `json:"firstFrameMs,omitempty"`
	PositionMS   int    `json:"positionMs,omitempty"`
	Completed    bool   `json:"completed,omitempty"`
	Stalled      bool   `json:"droppedOrStalled,omitempty"`
	ID           string `json:"id"`
	Status       string `json:"status"`
	PrepareMS    int    `json:"prepareMs"`
}
type Capabilities struct {
	Version  int     `json:"capabilitiesVersion"`
	DeviceID string  `json:"deviceId"`
	Probes   []Probe `json:"probes"`
}
type Device struct {
	ID           string       `json:"deviceId"`
	Registration Registration `json:"registration"`
	Preferences  Preferences  `json:"preferences"`
	Capabilities Capabilities `json:"capabilities"`
}
type Programme struct {
	Title string `json:"title"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

type Item struct {
	Programmes  []Programme `json:"programmes,omitempty"`
	ID          string      `json:"id"`
	Provider    string      `json:"provider"`
	Kind        string      `json:"kind"`
	Title       string      `json:"title"`
	Subtitle    string      `json:"subtitle,omitempty"`
	Description string      `json:"description,omitempty"`
	ImageURL    string      `json:"imageUrl,omitempty"`
	DurationMS  int64       `json:"durationMs,omitempty"`
	PositionMS  int64       `json:"positionMs,omitempty"`
	Playable    bool        `json:"playable"`
}
type Section struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	Items []Item `json:"items"`
}
type Hero struct {
	Item        Item   `json:"item"`
	Description string `json:"description"`
	BackdropURL string `json:"backdropUrl,omitempty"`
}
type Screen struct {
	APIVersion int       `json:"apiVersion"`
	UIVersion  int       `json:"uiSchemaVersion"`
	Screen     string    `json:"screen"`
	Hero       *Hero     `json:"hero,omitempty"`
	Sections   []Section `json:"sections"`
}
type Module struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	State    string   `json:"state"`
	Features []string `json:"features"`
	Message  string   `json:"message,omitempty"`
}
type Event struct {
	APIVersion int    `json:"apiVersion"`
	Cursor     string `json:"cursor"`
	Type       string `json:"type"`
	Payload    any    `json:"payload"`
	DeviceID   string `json:"-"`
}
type Plan struct {
	Version   int    `json:"playbackVersion"`
	SessionID string `json:"sessionId"`
	Mode      string `json:"mode"`
	URL       string `json:"url"`
	MIME      string `json:"mimeType"`
	Live      bool   `json:"live"`
	Seekable  bool   `json:"seekable"`
	ResumeMS  int64  `json:"resumePositionMs"`
	Item      Item   `json:"item"`
}
type Progress struct {
	Item       Item   `json:"item"`
	PositionMS int64  `json:"positionMs"`
	DurationMS int64  `json:"durationMs"`
	State      string `json:"state"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// Record is a transactional persistence operation independent of the SQLite adapter.
type Record struct {
	Bucket, ID string
	Value      any
}
