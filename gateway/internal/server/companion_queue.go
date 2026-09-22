package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"zombiebox.local/gateway/internal/companion"
	"zombiebox.local/gateway/internal/companionmedia"
	"zombiebox.local/gateway/internal/domain"
	"zombiebox.local/gateway/internal/mediaqueue"
)

func (s *Server) companionQueue(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	if r.Method == "GET" {
		respond(w, 200, s.mediaQueue.State(g.ID))
		return
	}
	if r.Method == "DELETE" {
		s.mediaQueue.Cancel(g.ID)
		respond(w, 200, map[string]string{"phase": "STOPPED"})
		return
	}
	if s.deps.PublicMediaHTTP == nil || s.deps.Uploads == nil || s.deps.Media == nil {
		fail(w, 503, "media_queue_unavailable")
		return
	}
	var request struct {
		ID    string            `json:"id"`
		Items []mediaqueue.Item `json:"items"`
	}
	if !decode(w, r, &request) {
		return
	}
	if !companionmedia.ID.MatchString(request.ID) || mediaqueue.Validate(request.Items) != nil {
		fail(w, 400, "invalid_media_queue")
		return
	}
	for i := range request.Items {
		request.Items[i].Title = strings.TrimSpace(request.Items[i].Title)
		if request.Items[i].Title == "" {
			request.Items[i].Title = "Shared media"
		}
	}
	started := s.mediaQueue.Start(g.ID, request.ID, request.Items,
		func(ctx context.Context, item mediaqueue.Item, id string) error {
			return s.prepareQueuedMedia(ctx, g, item, id)
		},
		func(id string) { s.stopCompanionMedia(g.ID, id) },
		func() string { return randomID(16) },
	)
	if !started {
		fail(w, 409, "media_queue_busy")
		return
	}
	respond(w, 202, s.mediaQueue.State(g.ID))
}

func (s *Server) prepareQueuedMedia(ctx context.Context, g companion.Grant, item mediaqueue.Item, id string) error {
	// No provider credentials, cookies or gateway headers are forwarded to URLs.
	if !s.companions.Active(ctx, g.ID, g.TargetID) {
		return errors.New("companion revoked")
	}
	download, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(download, "GET", item.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	response, err := s.deps.PublicMediaHTTP.Do(req)
	if err != nil {
		return errors.New("media download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.ContentLength == 0 || response.ContentLength > companionmedia.MaxBytes {
		return errors.New("media download unsupported")
	}
	if response.ContentLength == -1 {
		streamStore, ok := s.deps.Uploads.(interface {
			PutStream(context.Context, string, string, io.Reader) (companionmedia.Asset, error)
		})
		if !ok {
			return errors.New("unknown media length unsupported")
		}
		_, err = streamStore.PutStream(download, g.ID, id, response.Body)
	} else {
		_, err = s.deps.Uploads.Put(download, g.ID, id, response.ContentLength, response.Body)
	}
	if err != nil {
		return err
	}
	status, _ := s.startCompanionMedia(ctx, g, id, item.Title)
	if status != 200 {
		return errors.New("media playback unavailable")
	}
	return nil
}

func (s *Server) stopCompanionMedia(owner, id string) {
	s.receiverClaims.Lock()
	defer s.receiverClaims.Unlock()
	s.mu.Lock()
	for _, c := range s.casts {
		if c.sender == "companion-"+owner && c.mediaID == id {
			s.endCastLocked(c)
		}
	}
	s.mu.Unlock()
	if s.deps.Uploads != nil {
		s.deps.Uploads.Remove(owner, id)
	}
}

// An explicit TV Stop cancels even while the next item is downloading. Session
// cleanup never calls this endpoint, so late cleanup cannot cancel a newer item.
func (s *Server) stopReceiverQueue(w http.ResponseWriter, r *http.Request, d domain.Device) {
	owner := s.mediaQueue.Owner()
	if owner != "" && s.companions.Active(r.Context(), owner, d.ID) {
		s.mediaQueue.Cancel(owner)
	}
	respond(w, 200, map[string]string{"state": "STOPPED"})
}
