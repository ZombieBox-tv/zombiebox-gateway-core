package companion

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func (s *Service) Authenticate(ctx context.Context, id, token string) (Grant, error) {
	var value Grant
	if !validHex(id, 32) || !validHex(token, 64) || s.db.Get(ctx, "companion-grants", id, &value) != nil || !equal(value.TokenHash, digest(token)) {
		return Grant{}, ErrDenied
	}
	value.TokenHash = ""
	return value, nil
}

func (s *Service) Active(ctx context.Context, id, target string) bool {
	var value Grant
	return validHex(id, 32) && s.db.Get(ctx, "companion-grants", id, &value) == nil && value.TargetID == target
}

// Proof establishes possession of the stored pairing secret before a phone sends
// credentials to a newly discovered address. It does not encrypt trusted-LAN HTTP.
func (s *Service) Proof(ctx context.Context, id, nonce string) (string, error) {
	if !validHex(id, 32) || !validHex(nonce, 32) {
		return "", ErrDenied
	}
	var grant Grant
	if s.db.Get(ctx, "companion-grants", id, &grant) != nil {
		return "", ErrDenied
	}
	mac := hmac.New(sha256.New, []byte(grant.TokenHash))
	_, _ = mac.Write([]byte("zombie-companion-v1\n" + id + "\n" + nonce))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (s *Service) Grants(ctx context.Context, target string) ([]Grant, error) {
	records, err := s.db.List(ctx, "companion-grants")
	if err != nil {
		return nil, err
	}
	out := []Grant{}
	for _, raw := range records {
		var item Grant
		if json.Unmarshal(raw, &item) != nil {
			return nil, ErrInvalid
		}
		if item.TargetID == target {
			item.TokenHash = ""
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *Service) Revoke(ctx context.Context, target, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Active(ctx, id, target) {
		return ErrDenied
	}
	if err := s.db.Delete(ctx, "companion-grants", id); err != nil {
		return err
	}
	for command, receipt := range s.delivered {
		if receipt.Grant == id {
			delete(s.delivered, command)
		}
	}
	delete(s.results, id)
	delete(s.commandRates, id)
	pending := s.queues[target][:0]
	for _, command := range s.queues[target] {
		if command.GrantID != id {
			pending = append(pending, command)
		}
	}
	s.queues[target] = pending
	s.publish(target)
	return nil
}
