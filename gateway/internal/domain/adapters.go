package domain

import (
	"errors"
	"net/http"
)

var ErrNotFound = errors.New("record not found")

type Config struct {
	Enabled      bool   `json:"enabled"`
	URL          string `json:"url,omitempty"`
	Token        string `json:"token,omitempty"`
	UserID       string `json:"userId,omitempty"`
	PlaylistPath string `json:"playlistPath,omitempty"`
	EPGURL       string `json:"epgUrl,omitempty"`
	CatalogID    string `json:"catalogId,omitempty"`
	MediaType    string `json:"mediaType,omitempty"`
}

type Source struct {
	AudioURL       string
	AudioHeaders   http.Header
	BrowsePath     string
	ArtworkURL     string
	ArtworkHeaders http.Header
	EPGID          string
	Item           Item
	URL            string
	Path           string
	Headers        http.Header
	MIME           string
	Live           bool
}

type NowPlaying struct {
	ArtworkURL string `json:"-"`
	Provider   string `json:"provider"`
	State      string `json:"state"`
	Item       *Item  `json:"item,omitempty"`
	Volume     int    `json:"volume"`
	PositionMS int64  `json:"positionMs"`
}

type AuthorizationPrompt struct {
	State     string `json:"state"`
	URL       string `json:"url,omitempty"`
	Code      string `json:"code,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

type PlayerCommand struct {
	Action     string `json:"action"`
	PositionMS int64  `json:"positionMs,omitempty"`
	Volume     int    `json:"volume,omitempty"`
}

type Stream struct {
	Index int `json:"index"`
	Tags  struct {
		Language string `json:"language"`
		Title    string `json:"title"`
	} `json:"tags"`
	Disposition struct {
		Default int `json:"default"`
		Forced  int `json:"forced"`
	} `json:"disposition"`
	Type    string `json:"codec_type"`
	Codec   string `json:"codec_name"`
	Profile string `json:"profile"`
	Level   int    `json:"level"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

type Metadata struct {
	Streams []Stream `json:"streams"`
	Format  struct {
		Name     string `json:"format_name"`
		Duration string `json:"duration"`
	} `json:"format"`
}
