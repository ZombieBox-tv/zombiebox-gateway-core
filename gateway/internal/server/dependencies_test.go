package server

import (
	"zombiebox.local/gateway/internal/artwork"
	"zombiebox.local/gateway/internal/httpclient"
	"zombiebox.local/gateway/internal/providers"
)

func testDependencies() Dependencies {
	adapters := providers.New(httpclient.Metadata(), httpclient.Private())
	return Dependencies{Reception: adapters, Browse: adapters, YouTubeReceiver: adapters, Artwork: artwork.New(httpclient.Metadata(), nil), Catalog: adapters, Search: adapters, Resolver: adapters, Player: adapters, Browser: adapters, ControlHTTP: httpclient.Private(), StreamHTTP: httpclient.Streaming()}
}
func newTestServer(db Persistence, opt Options) *Server { return New(db, opt, testDependencies()) }
