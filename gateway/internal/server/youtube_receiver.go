package server

import (
	"net/http"

	"zombiebox.local/gateway/internal/domain"
	youtubereceiver "zombiebox.local/gateway/internal/receivers/youtube"
)

func receiverError(w http.ResponseWriter, err error) {
	status := 502
	switch err {
	case youtubereceiver.ErrUnsupported:
		status = 503
	case youtubereceiver.ErrNotFound:
		status = 404
	case youtubereceiver.ErrInUse, youtubereceiver.ErrBusy, youtubereceiver.ErrDisabled:
		status = 409
	case youtubereceiver.ErrInvalid:
		status = 400
	}
	fail(w, status, err.Error())
}
func (s *Server) startYouTubeReceiver(w http.ResponseWriter, r *http.Request, d domain.Device) {
	state, err := s.youtubeReceiver.Open(r.Context(), d.ID)
	if err != nil {
		receiverError(w, err)
		return
	}
	respond(w, 201, state)
}
func (s *Server) youTubeReceiverOperation(w http.ResponseWriter, r *http.Request, d domain.Device) {
	id := r.PathValue("receiver")
	switch r.Method {
	case "GET":
		state, err := s.youtubeReceiver.Poll(r.Context(), d.ID, id)
		if err != nil {
			receiverError(w, err)
			return
		}
		respond(w, 200, state)
	case "POST":
		var state domain.ReceiverAcknowledgement
		if !decode(w, r, &state) {
			return
		}
		if err := s.youtubeReceiver.Acknowledge(r.Context(), d.ID, id, state); err != nil {
			receiverError(w, err)
			return
		}
		respond(w, 200, map[string]bool{"accepted": true})
	case "DELETE":
		if err := s.youtubeReceiver.Stop(r.Context(), d.ID, id); err != nil {
			receiverError(w, err)
			return
		}
		respond(w, 200, map[string]bool{"closed": true})
	}
}
func (s *Server) youTubeReceiverSource(device, id string) *domain.Source {
	return s.youtubeReceiver.Source(device, id)
}
