// Package server adapts the versioned HTTP protocol to application features.
package server

import (
	"net/http"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/catalog"
	"zombiebox.local/gateway/internal/receivers/inbox"

	youtubereceiver "zombiebox.local/gateway/internal/receivers/youtube"
)

type Health struct {
	Status     string `json:"status"`
	APIVersion int    `json:"apiVersion"`
}
type Options struct {
	ProbeDir                                   string
	ThreadfinURL                               string
	RelayURL, RelayControlURL, RelayAdminToken string
	RTSPPort                                   int
	PairingCode                                string
	MediaDir                                   string
	PollWait                                   time.Duration
	CatalogWait                                time.Duration
}
type attempt struct {
	count int
	until time.Time
}
type Server struct {
	mediaReceiverInbox *inbox.Service
	browse             *catalog.Browser
	youtubeReceiver    *youtubereceiver.Service
	probeKey           string
	browser            *browserSession
	integrationChecks  chan struct{}
	relayJobs          chan struct{}
	casts              map[string]*castSession
	seen               map[string]time.Time
	done               chan struct{}
	closeOnce          sync.Once
	db                 Persistence
	deps               Dependencies
	opt                Options
	events             *eventLog
	mux                *http.ServeMux
	mu                 sync.Mutex
	attempts           map[string]attempt
	sessions           map[string]*session
	polls              chan struct{}
	catalogCache       map[string]catalogEntry
	searchResults      map[string]searchResult
	configRevision     map[string]uint64
	managed            map[string]bool
	streams            chan struct{}
}

func New(db Persistence, opt Options, deps Dependencies) *Server {
	deps.validate(db)
	if opt.PollWait <= 0 {
		opt.PollWait = 20 * time.Second
	}
	if opt.CatalogWait <= 0 {
		opt.CatalogWait = 5 * time.Second
	}
	s := &Server{
		db:           db,
		opt:          opt,
		deps:         deps,
		events:       newEvents(),
		mux:          http.NewServeMux(),
		attempts:     map[string]attempt{},
		sessions:     map[string]*session{},
		polls:        make(chan struct{}, 32),
		catalogCache: map[string]catalogEntry{},
	}
	s.youtubeReceiver = youtubereceiver.New(deps.YouTubeReceiver, s.config, func() string { return randomID(16) })
	s.browse = catalog.NewPersistentBrowser(deps.Browse, db)
	s.probeKey = randomID(32)
	s.relayJobs = make(chan struct{}, 4)
	s.integrationChecks = make(chan struct{}, 2)
	s.casts = map[string]*castSession{}
	s.seen = map[string]time.Time{}
	s.done = make(chan struct{})
	if s.opt.RTSPPort == 0 {
		s.opt.RTSPPort = 8554
	}
	go s.reapCasts()
	s.configRevision = map[string]uint64{}
	s.managed = map[string]bool{}
	s.searchResults = map[string]searchResult{}
	s.streams = make(chan struct{}, 4)
	s.mediaReceiverInbox = inbox.New(receiverAdapter{s}, receiverAdapter{s})
	go s.reapMediaReceiver()
	s.routes()
	return s
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}
