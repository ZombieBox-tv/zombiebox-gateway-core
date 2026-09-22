package companion

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

var targetID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,80}$`)

type PairingTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Public LAN routing hints only. No credentials, capability reports or grant inventory.
func (s *Service) Targets(ctx context.Context) ([]PairingTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []PairingTarget{}
	for id, seen := range s.present {
		if s.now().Sub(seen) > 15*time.Second {
			continue
		}
		var device domain.Device
		if err := s.db.Get(ctx, "devices", id, &device); err != nil {
			continue
		}
		result = append(result, PairingTarget{id, targetName(device)})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].ID < result[j].ID
	})
	if len(result) > 32 {
		result = result[:32]
	}
	return result, nil
}

func targetName(d domain.Device) string {
	name := strings.TrimSpace(d.Registration.Platform.Manufacturer + " " + d.Registration.Platform.Model)
	if name == "" {
		return "Zombie Box TV"
	}
	runes := []rune(name)
	if len(runes) > 120 {
		return string(runes[:120])
	}
	return name
}

// The QR secret is the consent capability. Codes/target IDs never authorize grants.
func (s *Service) joinTarget(ctx context.Context, input Join) (Invitation, bool, error) {
	var invitation Invitation
	if input.TargetID != "" {
		if input.InvitationID != "" || input.Secret != "" || input.Code != "" || !targetID.MatchString(input.TargetID) || !validHex(input.ClientKey, 64) {
			return invitation, false, ErrInvalid
		}
		var target domain.Device
		if s.now().Sub(s.present[input.TargetID]) > 15*time.Second || s.db.Get(ctx, "devices", input.TargetID, &target) != nil {
			return invitation, false, ErrBusy
		}
		return Invitation{TargetID: target.ID, TargetName: targetName(target)}, false, nil
	}
	qr := input.InvitationID != ""
	if qr {
		if input.Code != "" || !validHex(input.InvitationID, 32) || !validHex(input.Secret, 64) || s.db.Get(ctx, "companion-invitations", input.InvitationID, &invitation) != nil || !equal(invitation.Secret, input.Secret) {
			return invitation, false, ErrDenied
		}
	} else {
		if len(input.Code) != 6 || input.Secret != "" {
			return invitation, false, ErrDenied
		}
		values, err := s.db.List(ctx, "companion-invitations")
		if err != nil {
			return invitation, false, err
		}
		for _, raw := range values {
			var candidate Invitation
			if json.Unmarshal(raw, &candidate) == nil && equal(candidate.Code, input.Code) && s.now().Before(candidate.Expires) {
				invitation = candidate
				break
			}
		}
	}
	if invitation.ID == "" || invitation.Used || !s.now().Before(invitation.Expires) {
		return invitation, false, ErrExpired
	}
	return invitation, qr && invitation.AutoApprove, nil
}

type pairingBlock struct {
	ID      string    `json:"id"`
	Expires time.Time `json:"expires"`
}

func blockID(target, kind, hash string) string { return digest(target + ":" + kind + ":" + hash) }
func (s *Service) blocked(ctx context.Context, target, client, origin string) (bool, error) {
	// Listing propagates persistence failures instead of treating them as "not blocked".
	values, err := s.db.List(ctx, "companion-blocks")
	if err != nil {
		return false, err
	}
	for _, raw := range values {
		var value pairingBlock
		if json.Unmarshal(raw, &value) != nil {
			return false, ErrInvalid
		}
		if s.now().Before(value.Expires) && (value.ID == blockID(target, "origin", origin) || client != "" && value.ID == blockID(target, "client", client)) {
			return true, nil
		}
	}
	return false, nil
}
func (s *Service) blockRecords(ctx context.Context, request Request) ([]domain.Record, error) {
	if err := s.cleanup(ctx, "companion-blocks"); err != nil {
		return nil, err
	}
	existing, err := s.db.List(ctx, "companion-blocks")
	if err != nil {
		return nil, err
	}
	if len(existing) > 254 {
		return nil, ErrBusy
	}
	result := []domain.Record{}
	for kind, hash := range map[string]string{"client": request.ClientHash, "origin": request.OriginHash} {
		if hash == "" {
			continue
		}
		block := pairingBlock{blockID(request.TargetID, kind, hash), s.now().Add(24 * time.Hour)}
		result = append(result, domain.Record{Bucket: "companion-blocks", ID: block.ID, Value: block})
	}
	return result, nil
}
