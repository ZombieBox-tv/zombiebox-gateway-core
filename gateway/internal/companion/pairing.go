package companion

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"zombiebox.local/gateway/internal/domain"
)

type Service struct {
	mu           sync.Mutex
	db           Persistence
	now          func() time.Time
	random       func(int) string
	publish      func(string)
	attempts     map[string]rate
	queues       map[string][]Command
	online       map[string]time.Time
	results      map[string]Result
	delivered    map[string]receipt
	commandRates map[string]rate
}
type rate struct {
	count int
	until time.Time
}

func New(db Persistence, now func() time.Time, random func(int) string, publish func(string)) *Service {
	return &Service{
		db:           db,
		now:          now,
		random:       random,
		publish:      publish,
		attempts:     map[string]rate{},
		queues:       map[string][]Command{},
		online:       map[string]time.Time{},
		results:      map[string]Result{},
		delivered:    map[string]receipt{},
		commandRates: map[string]rate{},
	}
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func (s *Service) shortCode() string {
	n, _ := strconv.ParseUint(s.random(4), 16, 32)
	return fmt.Sprintf("%06d", n%1000000)
}

func (s *Service) cleanup(ctx context.Context, bucket string) error {
	records, err := s.db.List(ctx, bucket)
	if err != nil {
		return err
	}
	for _, raw := range records {
		var value struct {
			ID      string
			Expires time.Time
		}
		if json.Unmarshal(raw, &value) != nil {
			return ErrInvalid
		}
		if !s.now().Before(value.Expires) {
			if err := s.db.Delete(ctx, bucket, value.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) Invite(ctx context.Context, target, name string) (Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanup(ctx, "companion-invitations"); err != nil {
		return Invitation{}, err
	}
	records, err := s.db.List(ctx, "companion-invitations")
	if err != nil {
		return Invitation{}, err
	}
	for _, raw := range records {
		var old Invitation
		if json.Unmarshal(raw, &old) != nil {
			return Invitation{}, ErrInvalid
		}
		if old.TargetID == target && !old.Used {
			return old, nil
		}
	}
	if len(records) >= 64 {
		return Invitation{}, ErrBusy
	}
	var code string
	for n := 0; n < 32; n++ {
		candidate := s.shortCode()
		found := false
		for _, raw := range records {
			var old Invitation
			_ = json.Unmarshal(raw, &old)
			if old.Code == candidate {
				found = true
				break
			}
		}
		if !found {
			code = candidate
			break
		}
	}
	if code == "" {
		return Invitation{}, ErrBusy
	}
	value := Invitation{ID: s.random(16), TargetID: target, TargetName: name, Secret: s.random(32), Code: code, Expires: s.now().Add(2 * time.Minute)}
	err = s.db.PutMany(ctx, domain.Record{Bucket: "companion-invitations", ID: value.ID, Value: value})
	return value, err
}

func (s *Service) Join(ctx context.Context, ip string, input Join) (Request, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 80 || strings.IndexFunc(name, func(r rune) bool { return r < 32 }) >= 0 {
		return Request{}, "", ErrInvalid
	}
	for key, value := range s.attempts {
		if !s.now().Before(value.until) {
			delete(s.attempts, key)
		}
	}
	attempt := s.attempts[ip]
	if attempt.count >= 5 || len(s.attempts) >= 128 {
		return Request{}, "", ErrBusy
	}
	attempt.count++
	attempt.until = s.now().Add(time.Minute)
	s.attempts[ip] = attempt
	if err := s.cleanup(ctx, "companion-requests"); err != nil {
		return Request{}, "", err
	}
	records, err := s.db.List(ctx, "companion-requests")
	if err != nil {
		return Request{}, "", err
	}
	if len(records) >= 64 {
		return Request{}, "", ErrBusy
	}
	var invitation Invitation
	if input.InvitationID != "" {
		if !validHex(input.InvitationID, 32) || !validHex(input.Secret, 64) || s.db.Get(ctx, "companion-invitations", input.InvitationID, &invitation) != nil || !equal(invitation.Secret, input.Secret) {
			return Request{}, "", ErrDenied
		}
	} else {
		if len(input.Code) != 6 {
			return Request{}, "", ErrDenied
		}
		records, err := s.db.List(ctx, "companion-invitations")
		if err != nil {
			return Request{}, "", err
		}
		for _, raw := range records {
			var item Invitation
			if json.Unmarshal(raw, &item) == nil && equal(item.Code, input.Code) && s.now().Before(item.Expires) {
				invitation = item
				break
			}
		}
	}
	if invitation.ID == "" || invitation.Used || !s.now().Before(invitation.Expires) {
		return Request{}, "", ErrExpired
	}
	token := s.random(32)
	request := Request{ID: s.random(16), TargetID: invitation.TargetID, Name: name, Comparison: s.shortCode(), State: "PENDING", Expires: invitation.Expires, TokenHash: digest(token)}
	invitation.Used = true
	err = s.db.PutMany(ctx, domain.Record{Bucket: "companion-invitations", ID: invitation.ID, Value: invitation}, domain.Record{Bucket: "companion-requests", ID: request.ID, Value: request})
	if err != nil {
		return Request{}, "", err
	}
	s.publish(invitation.TargetID)
	request.TokenHash = ""
	return request, token, nil
}

func (s *Service) RequestStatus(ctx context.Context, id, token string) (Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var value Request
	if !validHex(id, 32) || !validHex(token, 64) || s.db.Get(ctx, "companion-requests", id, &value) != nil || !equal(value.TokenHash, digest(token)) {
		return Request{}, ErrDenied
	}
	if !s.now().Before(value.Expires) {
		return Request{}, ErrExpired
	}
	value.TokenHash = ""
	return value, nil
}

func (s *Service) Pending(ctx context.Context, target string) ([]Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.db.List(ctx, "companion-requests")
	if err != nil {
		return nil, err
	}
	out := []Request{}
	for _, raw := range records {
		var item Request
		if json.Unmarshal(raw, &item) != nil {
			return nil, ErrInvalid
		}
		if item.TargetID == target && item.State == "PENDING" && s.now().Before(item.Expires) {
			item.TokenHash = ""
			out = append(out, item)
		}
	}
	return out, nil
}

func (s *Service) Decide(ctx context.Context, target, name, id string, accept bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var request Request
	if !validHex(id, 32) || s.db.Get(ctx, "companion-requests", id, &request) != nil || request.TargetID != target {
		return ErrDenied
	}
	if request.State != "PENDING" || !s.now().Before(request.Expires) {
		return ErrExpired
	}
	request.State = "DENIED"
	records := []domain.Record{}
	if accept {
		all, err := s.db.List(ctx, "companion-grants")
		if err != nil {
			return err
		}
		if len(all) >= 128 {
			return ErrBusy
		}
		request.State = "APPROVED"
		grant := Grant{ID: request.ID, TargetID: target, TargetName: name, Name: request.Name, TokenHash: request.TokenHash, Created: s.now()}
		records = append(records, domain.Record{Bucket: "companion-grants", ID: grant.ID, Value: grant})
	}
	records = append(records, domain.Record{Bucket: "companion-requests", ID: request.ID, Value: request})
	if err := s.db.PutMany(ctx, records...); err != nil {
		return err
	}
	s.publish(target)
	return nil
}
