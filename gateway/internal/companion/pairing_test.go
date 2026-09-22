package companion_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/domain"

	"zombiebox.local/gateway/internal/companion"
	"zombiebox.local/gateway/internal/store"
)

func setup(t *testing.T) (*companion.Service, *time.Time, *store.Store) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	s := companion.New(db, func() time.Time { return now }, func(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }, func(string) {})
	return s, &now, db
}
func accepted(t *testing.T, s *companion.Service) (companion.Grant, string) {
	t.Helper()
	ctx := context.Background()
	inv, err := s.Invite(ctx, "target-one", "TV")
	if err != nil {
		t.Fatal(err)
	}
	request, token, err := s.Join(ctx, "127.0.0.1", companion.Join{Code: inv.Code, Name: "Phone"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, request.ID, token); err == nil {
		t.Fatal("pending authorized")
	}
	if err = s.Decide(ctx, "other-target", "Other", request.ID, true); err == nil {
		t.Fatal("foreign decision")
	}
	if err = s.Decide(ctx, "target-one", "TV", request.ID, true); err != nil {
		t.Fatal(err)
	}
	grant, err := s.Authenticate(ctx, request.ID, token)
	if err != nil {
		t.Fatal(err)
	}
	return grant, token
}
func TestAtomicConsentAndInvitationReplay(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	inv, _ := s.Invite(ctx, "target-one", "TV")
	request, token, err := s.Join(ctx, "127.0.0.1", companion.Join{Code: inv.Code, Name: "Phone"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Join(ctx, "127.0.0.1", companion.Join{Code: inv.Code, Name: "Other"}); err == nil {
		t.Fatal("replayed invitation")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.Decide(ctx, "target-one", "TV", request.ID, true) }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("non-atomic approval")
	}
	state, err := s.RequestStatus(ctx, request.ID, token)
	if err != nil || state.State != "APPROVED" || state.TokenHash != "" {
		t.Fatal(state, err)
	}
	grants, _ := s.Grants(ctx, "target-one")
	if len(grants) != 1 || grants[0].TokenHash != "" {
		t.Fatal("unsafe inventory")
	}
}
func TestDurabilityExpiryRateAndRevocation(t *testing.T) {
	s, now, db := setup(t)
	ctx := context.Background()
	grant, token := accepted(t, s)
	next := companion.New(db, func() time.Time { return *now }, func(int) string { return "" }, func(string) {})
	if _, err := next.Authenticate(ctx, grant.ID, token); err != nil {
		t.Fatal("grant lost on restart")
	}
	if proof, err := next.Proof(ctx, grant.ID, "0123456789abcdef0123456789abcdef"); err != nil || len(proof) != 64 {
		t.Fatal(err)
	}
	*now = now.Add(3 * time.Minute)
	if _, err := s.RequestStatus(ctx, grant.ID, token); err == nil {
		t.Fatal("expired request accepted")
	}
	if _, err := s.Authenticate(ctx, grant.ID, token); err != nil {
		t.Fatal("durable grant expired")
	}
	for i := 0; i < 5; i++ {
		_, _, _ = s.Join(ctx, "10.0.0.2", companion.Join{Code: "000000", Name: "guess"})
	}
	if _, _, err := s.Join(ctx, "10.0.0.2", companion.Join{Code: "000000", Name: "guess"}); err != companion.ErrBusy {
		t.Fatal("missing rate limit", err)
	}
	if err := s.Revoke(ctx, "target-one", grant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Authenticate(ctx, grant.ID, token); err == nil {
		t.Fatal("revoked grant accepted")
	}
}
func TestCommandScopeTTLRateAndAtMostOnceDelivery(t *testing.T) {
	s, now, _ := setup(t)
	ctx := context.Background()
	g, _ := accepted(t, s)
	if _, err := s.Send(ctx, g, "OK", ""); err == nil {
		t.Fatal("offline accepted")
	}
	s.Poll(ctx, g.TargetID, true)
	if _, err := s.Send(ctx, g, "SHELL", "id"); err == nil {
		t.Fatal("arbitrary command")
	}
	cmd, err := s.Send(ctx, g, "PLAY_PAUSE", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Poll(ctx, "other", true)) != 0 {
		t.Fatal("foreign delivery")
	}
	values := s.Poll(ctx, g.TargetID, true)
	if len(values) != 1 || values[0].ID != cmd.ID {
		t.Fatal(values)
	}
	if len(s.Poll(ctx, g.TargetID, true)) != 0 {
		t.Fatal("toggle replay")
	}
	if s.Acknowledge("other", cmd.ID, "EXECUTED") == nil {
		t.Fatal("foreign ack")
	}
	if err = s.Acknowledge(g.TargetID, cmd.ID, "EXECUTED"); err != nil {
		t.Fatal(err)
	}
	_, result := s.RemoteStatus(g.TargetID, g.ID)
	if result.Status != "EXECUTED" {
		t.Fatal(result)
	}
	_, err = s.Send(ctx, g, "UP", "")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(3 * time.Second)
	if len(s.Poll(ctx, g.TargetID, true)) != 0 {
		t.Fatal("expired key")
	}
	for i := 0; i < 5; i++ {
		if _, err = s.Send(ctx, g, "UP", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Send(ctx, g, "UP", ""); err != companion.ErrBusy {
		t.Fatal("unbounded repeat")
	}
	if err = s.Revoke(ctx, g.TargetID, g.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.Poll(ctx, g.TargetID, true)) != 0 {
		t.Fatal("revoked delivery")
	}
}

func TestGatewayProofVectorAndRevokedDeliveryReceipt(t *testing.T) {
	s, _, db := setup(t)
	ctx := context.Background()
	id := strings.Repeat("a", 32)
	token := strings.Repeat("b", 64)
	hash := sha256.Sum256([]byte(token))
	if err := db.PutMany(ctx, domain.Record{Bucket: "companion-grants", ID: id, Value: companion.Grant{ID: id, TargetID: "target", TokenHash: hex.EncodeToString(hash[:])}}); err != nil {
		t.Fatal(err)
	}
	proof, err := s.Proof(ctx, id, strings.Repeat("c", 32))
	if err != nil || proof != "c3f42a4febd5cb190465ae626aadae2fd5d4cc481209659cdb52902beaaa4711" {
		t.Fatal(proof, err)
	}
	grant, err := s.Authenticate(ctx, id, token)
	if err != nil {
		t.Fatal(err)
	}
	s.Poll(ctx, "target", true)
	command, err := s.Send(ctx, grant, "OK", "")
	if err != nil {
		t.Fatal(err)
	}
	s.Poll(ctx, "target", true)
	if err = s.Revoke(ctx, "target", id); err != nil {
		t.Fatal(err)
	}
	if err = s.Acknowledge("target", command.ID, "EXECUTED"); err == nil {
		t.Fatal("revoked receipt survived")
	}
	if _, err = s.Proof(ctx, id, strings.Repeat("c", 32)); err == nil {
		t.Fatal("revoked proof")
	}
}
