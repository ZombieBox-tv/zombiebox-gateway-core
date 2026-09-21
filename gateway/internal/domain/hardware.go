package domain

type CodecHint struct {
	Name  string   `json:"name"`
	Types []string `json:"types"`
}
type HardwareReport struct {
	Version          int         `json:"scannerVersion"`
	Fingerprint      string      `json:"fingerprint"`
	Product          string      `json:"product"`
	Device           string      `json:"device"`
	ABIs             []string    `json:"abis"`
	CPUCores         int         `json:"cpuCores"`
	PhysicalMB       int         `json:"physicalMb"`
	StorageFreeMB    int64       `json:"storageFreeMb"`
	GLES             int         `json:"glesVersion"`
	Keyboard         bool        `json:"keyboard"`
	Mouse            bool        `json:"mouse"`
	Network          string      `json:"network"`
	GatewayLatencyMS int         `json:"gatewayLatencyMs"`
	Decoders         []CodecHint `json:"decoders"`
	ExternalPlayers  []string    `json:"externalPlayers"`
	NativeDIAL       string      `json:"nativeDial"`
	Multicast        string      `json:"multicast"`
}
