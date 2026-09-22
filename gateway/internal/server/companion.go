package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"zombiebox.local/gateway/internal/companion"
	"zombiebox.local/gateway/internal/domain"
)

func companionFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, companion.ErrDenied):
		fail(w, 403, "companion_denied")
	case errors.Is(err, companion.ErrExpired):
		fail(w, 410, "companion_expired")
	case errors.Is(err, companion.ErrBusy):
		fail(w, 429, "companion_busy")
	case errors.Is(err, companion.ErrInvalid):
		fail(w, 400, "companion_invalid")
	default:
		fail(w, 500, "storage_error")
	}
}
func deviceName(d domain.Device) string {
	return strings.TrimSpace(d.Registration.Platform.Manufacturer + " " + d.Registration.Platform.Model)
}

func (s *Server) companionInvite(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var body struct {
		Gateway string `json:"gateway"`
	}
	if !decode(w, r, &body) {
		return
	}
	locator, err := url.Parse(body.Gateway)
	// The paired target supplies its reachable gateway URL, never a Host-header guess.
	if err != nil || len(body.Gateway) > 256 || locator.Hostname() == "" || (locator.Scheme != "http" && locator.Scheme != "https") || locator.User != nil || locator.RawQuery != "" || locator.Fragment != "" || strings.Trim(locator.Path, "/") != "" {
		fail(w, 400, "invalid_gateway")
		return
	}
	invitation, err := s.companions.Invite(r.Context(), d.ID, deviceName(d))
	if err != nil {
		companionFailure(w, err)
		return
	}
	payload := map[string]any{"version": 2, "gateway": strings.TrimRight(body.Gateway, "/"), "invitationId": invitation.ID, "secret": invitation.Secret}
	raw, _ := json.Marshal(payload)
	png, err := qrcode.Encode(string(raw), qrcode.Medium, 384)
	if err != nil {
		fail(w, 500, "qr_failed")
		return
	}
	respond(w, 201, map[string]any{"id": invitation.ID, "code": invitation.Code, "expires": invitation.Expires, "qrPng": png, "remainingMs": max(0, time.Until(invitation.Expires).Milliseconds())})
}

func (s *Server) companionJoin(w http.ResponseWriter, r *http.Request) {
	var input companion.Join
	if !decode(w, r, &input) {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	request, token, err := s.companions.Join(r.Context(), ip, input)
	if err != nil {
		companionFailure(w, err)
		return
	}
	respond(w, 201, map[string]any{"request": request, "token": token})
}
func (s *Server) companionRequest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &input) {
		return
	}
	request, err := s.companions.RequestStatus(r.Context(), r.PathValue("request"), input.Token)
	if err != nil {
		companionFailure(w, err)
		return
	}
	respond(w, 200, request)
}
func (s *Server) companionProof(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID    string `json:"id"`
		Nonce string `json:"nonce"`
	}
	if !decode(w, r, &input) {
		return
	}
	proof, err := s.companions.Proof(r.Context(), input.ID, input.Nonce)
	if err != nil {
		companionFailure(w, err)
		return
	}
	respond(w, 200, map[string]string{"proof": proof})
}
func (s *Server) companionPending(w http.ResponseWriter, r *http.Request, d domain.Device) {
	requests, err := s.companions.Pending(r.Context(), d.ID)
	if err != nil {
		companionFailure(w, err)
		return
	}
	grants, err := s.companions.Grants(r.Context(), d.ID)
	if err != nil {
		companionFailure(w, err)
		return
	}
	respond(w, 200, map[string]any{"requests": requests, "grants": grants})
}
func (s *Server) companionDecision(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var input struct {
		Accept    bool `json:"accept"`
		Ignore24h bool `json:"ignore24h,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	if err := s.companions.Decide(r.Context(), d.ID, deviceName(d), r.PathValue("request"), input.Accept, input.Ignore24h); err != nil {
		companionFailure(w, err)
		return
	}
	respond(w, 200, map[string]bool{"accepted": input.Accept})
}
func (s *Server) companionRevoke(w http.ResponseWriter, r *http.Request, d domain.Device) {
	id := r.PathValue("grant")
	if err := s.companions.Revoke(r.Context(), d.ID, id); err != nil {
		companionFailure(w, err)
		return
	}
	s.revokeCompanionCasts(id)
	respond(w, 200, map[string]string{"state": "REVOKED"})
}

func (s *Server) companionAuth(next func(http.ResponseWriter, *http.Request, companion.Grant)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			fail(w, 401, "unauthorized")
			return
		}
		grant, err := s.companions.Authenticate(r.Context(), r.Header.Get("X-Zombie-Device"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if err != nil {
			fail(w, 401, "unauthorized")
			return
		}
		next(w, r, grant)
	}
}
func (s *Server) companionStatus(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	var target domain.Device
	if s.db.Get(r.Context(), "devices", g.TargetID, &target) != nil {
		fail(w, 404, "target_unavailable")
		return
	}
	online, result := s.companions.RemoteStatus(g.TargetID, g.ID)
	s.mu.Lock()
	castOnline := time.Since(s.seen[g.TargetID]) < 45*time.Second
	s.mu.Unlock()
	respond(w, 200, map[string]any{"grant": g, "remoteOnline": online, "textInputId": s.companions.TextInput(g.TargetID), "lastCommand": result, "mediaAvailable": s.deps.Uploads != nil && s.deps.Media != nil && target.Preferences.AllowCasting && castOnline, "castAvailable": s.opt.RelayURL != "" && target.Preferences.AllowCasting && castOnline})
}
func (s *Server) companionCommand(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	var input struct {
		Action   string `json:"action"`
		Text     string `json:"text,omitempty"`
		InputID  string `json:"inputId,omitempty"`
		Provider string `json:"provider"`
	}
	if !decode(w, r, &input) {
		return
	}
	var command companion.Command
	var err error
	if input.Action == "TEXT" && input.Provider == "" {
		command, err = s.companions.SendText(r.Context(), g, input.Text, input.InputID)
	} else if input.Text != "" || input.InputID != "" {
		err = companion.ErrInvalid
	} else {
		command, err = s.companions.Send(r.Context(), g, input.Action, input.Provider)
	}
	if err != nil {
		companionFailure(w, err)
		return
	}
	respond(w, 202, command)
}
func (s *Server) companionPoll(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var input struct {
		Active  bool   `json:"active"`
		InputID string `json:"inputId,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	respond(w, 200, map[string]any{"commands": s.companions.Poll(r.Context(), d.ID, input.Active, input.InputID)})
}
func (s *Server) companionAck(w http.ResponseWriter, r *http.Request, d domain.Device) {
	var input companion.Result
	if !decode(w, r, &input) {
		return
	}
	if err := s.companions.Acknowledge(d.ID, input.ID, input.Status); err != nil {
		companionFailure(w, err)
		return
	}
	respond(w, 200, map[string]string{"state": "ACKNOWLEDGED"})
}
func (s *Server) companionForget(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	if err := s.companions.Revoke(r.Context(), g.TargetID, g.ID); err != nil {
		companionFailure(w, err)
		return
	}
	s.revokeCompanionCasts(g.ID)
	respond(w, 200, map[string]string{"state": "REVOKED"})
}
func (s *Server) companionCast(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	var request struct {
		Mode           *string `json:"mode,omitempty"`
		MaxVideoHeight *int    `json:"maxVideoHeight"`
	}
	if r.ContentLength != 0 && !decode(w, r, &request) {
		return
	}
	raw, _ := json.Marshal(castRequest{ReceiverID: g.TargetID, ReplaceExisting: true, MaxVideoHeight: request.MaxVideoHeight, Mode: request.Mode})
	r.Body = io.NopCloser(bytes.NewReader(raw))
	s.createCast(w, r, domain.Device{ID: "companion-" + g.ID})
}
func (s *Server) companionCastOperation(w http.ResponseWriter, r *http.Request, g companion.Grant) {
	sender := domain.Device{ID: "companion-" + g.ID}
	switch {
	case r.Method == "DELETE":
		s.stopCast(w, r, sender)
	case r.Method == "PUT":
		s.castLease(w, r, sender)
	default:
		s.castReady(w, r, sender)
	}
}

func (s *Server) revokeCompanionCasts(id string) {
	s.mediaQueue.Cancel(id)
	if s.deps.Uploads != nil {
		defer s.deps.Uploads.RemoveOwner(id)
	}
	s.receiverClaims.Lock()
	defer s.receiverClaims.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cast := range s.casts {
		if cast.sender == "companion-"+id {
			s.endCastLocked(cast)
		}
	}
}

func (s *Server) companionTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.companions.Targets(r.Context())
	if err != nil {
		companionFailure(w, err)
		return
	}
	respond(w, 200, map[string]any{"targets": targets})
}
