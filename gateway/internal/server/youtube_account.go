package server

import (
	"errors"
	"net/http"

	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/youtubeaccount"
)

func (s *Server) youtubeAccountStatus(w http.ResponseWriter, r *http.Request, _ domain.Device) {
	respond(w, 200, s.youtubeAccount.Status(r.Context()))
}

func (s *Server) youtubeAccountStart(w http.ResponseWriter, r *http.Request, _ domain.Device) {
	status, err := s.youtubeAccount.Start(r.Context())
	if err != nil {
		youtubeAccountError(w, err)
		return
	}
	respond(w, 200, status)
}

func (s *Server) youtubeAccountPoll(w http.ResponseWriter, r *http.Request, _ domain.Device) {
	status, err := s.youtubeAccount.Poll(r.Context())
	if err != nil {
		youtubeAccountError(w, err)
		return
	}
	if status.Connected {
		s.mu.Lock()
		s.youtubeHomeFeeds = map[string]searchResult{}
		s.mu.Unlock()
	}
	respond(w, 200, status)
}

func (s *Server) youtubeAccountDisconnect(w http.ResponseWriter, r *http.Request, _ domain.Device) {
	if !s.admin(w, r) {
		return
	}
	if err := s.youtubeAccount.Disconnect(r.Context()); err != nil {
		fail(w, 500, "storage_error")
		return
	}
	s.mu.Lock()
	s.youtubeHomeFeeds = map[string]searchResult{}
	s.mu.Unlock()
	respond(w, 200, map[string]any{"connected": false})
}

func (s *Server) youtubeAccountList(w http.ResponseWriter, r *http.Request, d domain.Device) {
	list := r.PathValue("list")
	if list != "subscriptions" && list != "playlists" {
		fail(w, 404, "account_list_unavailable")
		return
	}
	page, err := s.youtubeAccount.List(r.Context(), list, r.URL.Query().Get("pageToken"))
	if err != nil {
		youtubeAccountError(w, err)
		return
	}
	s.mu.Lock()
	config, revision := s.config(r.Context(), "youtube"), s.configRevision["youtube"]
	s.mu.Unlock()
	sources := make([]domain.Source, 0, len(page.Items))
	for _, entry := range page.Items {
		sources = append(sources, domain.Source{Item: domain.Item{ID: entry.ID, Provider: "youtube", Kind: entry.Kind, Title: entry.Title, Subtitle: entry.Subtitle}, BrowsePath: entry.BrowseID})
	}
	result := s.browse.Present(r.Context(), d.ID, "youtube", list, revision, config, "account:"+list, "", 0, domain.BrowseResult{Sources: sources, NextOffset: -1})
	respond(w, 200, map[string]any{"apiVersion": 1, "items": result.Items, "nextPageToken": page.NextPageToken})
}

func youtubeAccountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, youtubeaccount.ErrUnavailable):
		fail(w, 409, "youtube_account_unconfigured")
	case errors.Is(err, youtubeaccount.ErrNotConnected):
		fail(w, 409, "youtube_account_disconnected")
	case errors.Is(err, youtubeaccount.ErrTooSoon):
		fail(w, 429, "authorization_poll_too_soon")
	case errors.Is(err, youtubeaccount.ErrExpired):
		fail(w, 410, "authorization_expired")
	default:
		fail(w, 502, "youtube_account_unavailable")
	}
}
