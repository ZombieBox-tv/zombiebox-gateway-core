package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"zombiebox.local/gateway/internal/domain"
)

// Ports belong to the application that consumes them. The executable supplies
// implementations; constructing a Server never opens a DB, network or process.
type Persistence interface {
	Get(context.Context, string, string, any) error
	Put(context.Context, string, string, any) error
	PutMany(context.Context, ...domain.Record) error
	List(context.Context, string) ([]json.RawMessage, error)
	Count(context.Context, string) (int, error)
	Delete(context.Context, string, string) error
}
type Catalog interface {
	Fetch(context.Context, string, domain.Config, string) ([]domain.Source, error)
	HasCatalog(string) bool
	Features(string) []string
}
type Search interface {
	YouTube(context.Context, domain.Config) ([]domain.Source, error)
}
type Resolver interface {
	Resolve(context.Context, domain.Source) (domain.Source, error)
}
type Player interface {
	SpotifyStatus(context.Context, domain.Config) (domain.NowPlaying, error)
	SpotifyAuthorization(context.Context, domain.Config) (domain.AuthorizationPrompt, error)
	SpotifyCommand(context.Context, domain.Config, domain.PlayerCommand) error
}
type Browser interface {
	BrowserRequest(context.Context, domain.Config, string, string, any) ([]byte, error)
}
type YouTubeReceiver interface {
	OpenReceiver(context.Context, domain.Config, string) (domain.YouTubeReceiverState, error)
	PollReceiver(context.Context, domain.Config, string) (domain.YouTubeReceiverState, error)
	AcknowledgeReceiver(context.Context, domain.Config, string, domain.ReceiverAcknowledgement) error
	CloseReceiver(context.Context, domain.Config, string) error
}

type Artwork interface {
	Image(context.Context, domain.Source, bool) ([]byte, error)
}

type Media interface {
	Probe(context.Context, string) (domain.Metadata, error)
	Convert(context.Context, string, string, io.Writer) error
}
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Dependencies are immutable after construction. Media and Artwork are optional.
// Keep control requests and unbounded-duration streaming on separate transports.
type Dependencies struct {
	YouTubeReceiver YouTubeReceiver
	Artwork         Artwork
	Catalog         Catalog
	Search          Search
	Resolver        Resolver
	Player          Player
	Browser         Browser
	Media           Media
	ControlHTTP     HTTPClient
	StreamHTTP      HTTPClient
}

func (d Dependencies) validate(db Persistence) {
	if db == nil || d.Catalog == nil || d.Search == nil || d.Resolver == nil || d.Player == nil || d.Browser == nil || d.ControlHTTP == nil || d.StreamHTTP == nil {
		panic("server: required dependency missing")
	}
}
