// Package server exposes the versioned client protocol. Provider DTOs stay here.
package server

import (
	"net/http"

	"zombiebox.local/gateway/internal/domain"
)

func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/youtube/receiver", s.auth(s.startYouTubeReceiver))
	s.mux.HandleFunc("GET /v1/youtube/receiver/{receiver}", s.auth(s.youTubeReceiverOperation))
	s.mux.HandleFunc("POST /v1/youtube/receiver/{receiver}/state", s.auth(s.youTubeReceiverOperation))
	s.mux.HandleFunc("DELETE /v1/youtube/receiver/{receiver}", s.auth(s.youTubeReceiverOperation))
	s.mux.HandleFunc("GET /v1/artwork/{item}", s.auth(s.artwork))
	s.mux.HandleFunc("GET /v1/probes", s.auth(s.probeManifest))
	s.mux.HandleFunc("GET /v1/probes/{probe}", s.probeStream)
	s.mux.HandleFunc("GET /v1/cast/receivers", s.auth(s.castReceivers))
	s.mux.HandleFunc("POST /v1/cast", s.auth(s.createCast))
	s.mux.HandleFunc("GET /v1/cast/active", s.auth(s.activeCast))
	s.mux.HandleFunc("PUT /v1/cast/{cast}", s.auth(s.castLease))
	s.mux.HandleFunc("POST /v1/cast/{cast}/ready", s.auth(s.castReady))
	s.mux.HandleFunc("DELETE /v1/cast/{cast}", s.auth(s.stopCast))
	s.mux.HandleFunc("POST /internal/relay/auth", s.relayAuth)
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, Health{"ok", 1}) })
	s.mux.HandleFunc("POST /v1/devices/register", s.register)
	s.mux.HandleFunc("GET /v1/device", s.auth(func(w http.ResponseWriter, r *http.Request, d domain.Device) { respond(w, 200, d) }))
	s.mux.HandleFunc("GET /v1/device/preferences", s.auth(func(w http.ResponseWriter, r *http.Request, d domain.Device) { respond(w, 200, d.Preferences) }))
	s.mux.HandleFunc("PUT /v1/device/preferences", s.auth(s.preferences))
	s.mux.HandleFunc("PUT /v1/device/hardware", s.auth(s.hardwareReport))
	s.mux.HandleFunc("PUT /v1/device/capabilities", s.auth(s.capabilities))
	s.mux.HandleFunc("GET /v1/modules", s.auth(s.modules))
	s.mux.HandleFunc("GET /v1/catalog", s.auth(s.catalogPage))
	s.mux.HandleFunc("GET /v1/home", s.auth(s.home))
	s.mux.HandleFunc("GET /v1/events", s.auth(s.poll))
	s.mux.HandleFunc("GET /v1/integrations", s.auth(s.integrationList))
	s.mux.HandleFunc("POST /v1/browser", s.auth(s.startBrowser))
	s.mux.HandleFunc("GET /v1/browser/{browser}/frame", s.auth(s.browserOperation))
	s.mux.HandleFunc("POST /v1/browser/{browser}/input", s.auth(s.browserOperation))
	s.mux.HandleFunc("DELETE /v1/browser/{browser}", s.auth(s.browserOperation))
	s.mux.HandleFunc("GET /v1/player/spotify", s.auth(s.nowPlaying))
	s.mux.HandleFunc("GET /v1/player/spotify/authorization", s.auth(s.spotifyAuthorization))
	s.mux.HandleFunc("POST /v1/player/spotify", s.auth(s.playerCommand))
	s.mux.HandleFunc("GET /v1/providers", s.auth(s.providers))
	s.mux.HandleFunc("PUT /v1/providers/{provider}", s.auth(s.configureProvider))
	s.mux.HandleFunc("POST /v1/playback", s.auth(s.playback))
	s.mux.HandleFunc("PUT /v1/playback/{session}/progress", s.auth(s.progress))
	s.mux.HandleFunc("DELETE /v1/playback/{session}", s.auth(s.stop))
	s.mux.HandleFunc("GET /v1/streams/{session}", s.stream)
	s.mux.HandleFunc("GET /v1/streams/{session}/{resource}", s.stream)
}
