package domain

type CodecProfileHint struct {
	MIME    string `json:"mime"`
	Profile int    `json:"profile"`
	Level   int    `json:"level"`
}
type DisplayModeHint struct {
	ID             int `json:"id"`
	Width          int `json:"width"`
	Height         int `json:"height"`
	RefreshMilliHz int `json:"refreshMilliHz"`
}
type DisplayHint struct {
	ID             int               `json:"id"`
	Default        bool              `json:"defaultDisplay"`
	Presentation   bool              `json:"presentation"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	RefreshMilliHz int               `json:"refreshMilliHz"`
	ActiveModeID   int               `json:"activeModeId"`
	Modes          []DisplayModeHint `json:"modes"`
}
type CodecHint struct {
	Profiles        []CodecProfileHint `json:"profiles,omitempty"`
	Acceleration    string             `json:"acceleration,omitempty"`
	ProbeCandidates []string           `json:"probeCandidates,omitempty"`
	Name            string             `json:"name"`
	Types           []string           `json:"types"`
}
type HardwareReport struct {
	Encoders         []CodecHint   `json:"encoders,omitempty"`
	Displays         []DisplayHint `json:"displays,omitempty"`
	InventoryLimited bool          `json:"inventoryLimited,omitempty"`
	IntegrationHints []string      `json:"integrationHints,omitempty"`
	Version          int           `json:"scannerVersion"`
	Fingerprint      string        `json:"fingerprint"`
	Product          string        `json:"product"`
	Device           string        `json:"device"`
	ABIs             []string      `json:"abis"`
	CPUCores         int           `json:"cpuCores"`
	PhysicalMB       int           `json:"physicalMb"`
	StorageFreeMB    int64         `json:"storageFreeMb"`
	GLES             int           `json:"glesVersion"`
	Keyboard         bool          `json:"keyboard"`
	Mouse            bool          `json:"mouse"`
	Network          string        `json:"network"`
	GatewayLatencyMS int           `json:"gatewayLatencyMs"`
	Decoders         []CodecHint   `json:"decoders"`
	ExternalPlayers  []string      `json:"externalPlayers"`
	NativeDIAL       string        `json:"nativeDial"`
	Multicast        string        `json:"multicast"`
}
