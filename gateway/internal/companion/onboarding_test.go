package companion_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"zombiebox.local/gateway/internal/companion"
	"zombiebox.local/gateway/internal/domain"
)

func TestQRGrantsImmediatelyOnceAndExpiresAtFiveMinutes(t *testing.T) {
	s, now, _ := setup(t)
	ctx := context.Background()
	invite, err := s.Invite(ctx, "tv", "Living room")
	if err != nil || invite.Expires.Sub(*now) != 5*time.Minute {
		t.Fatal(invite, err)
	}
	if _, _, err := s.Join(ctx, "host", companion.Join{InvitationID: invite.ID, Secret: strings.Repeat("0", 64), Name: "Phone"}); err == nil {
		t.Fatal("wrong QR secret accepted")
	}
	request, token, err := s.Join(ctx, "host", companion.Join{InvitationID: invite.ID, Secret: invite.Secret, Name: "Phone"})
	if err != nil || request.State != "APPROVED" {
		t.Fatal(request, err)
	}
	if _, err := s.Authenticate(ctx, request.ID, token); err != nil {
		t.Fatal("QR not directly authorized", err)
	}
	pending, _ := s.Pending(ctx, "tv")
	if len(pending) != 0 {
		t.Fatal("QR unexpectedly asks TV again")
	}
	if _, _, err := s.Join(ctx, "host", companion.Join{InvitationID: invite.ID, Secret: invite.Secret, Name: "Replay"}); err == nil {
		t.Fatal("QR replay accepted")
	}
	next, _ := s.Invite(ctx, "tv", "Living room")
	*now = now.Add(5 * time.Minute)
	if _, _, err := s.Join(ctx, "host", companion.Join{InvitationID: next.ID, Secret: next.Secret, Name: "Late"}); err != companion.ErrExpired {
		t.Fatal("QR boundary expiry", err)
	}
}

func TestNetworkRequestNeedsLocalConsentAndIgnoreSurvivesRestart(t *testing.T) {
	s, now, db := setup(t)
	ctx := context.Background()
	id := "c3b32045-a431-457c-9ba0-32a5464f43ec"
	_ = db.PutMany(ctx, domain.Record{Bucket: "devices", ID: id, Value: domain.Device{ID: id}})
	s.Poll(ctx, id, true)
	targets, err := s.Targets(ctx)
	if err != nil || len(targets) != 1 {
		t.Fatal(targets, err)
	}
	input := companion.Join{TargetID: id, ClientKey: strings.Repeat("b", 64), Name: "Phone"}
	request, token, err := s.Join(ctx, "host", input)
	if err != nil || request.State != "PENDING" || len(request.Comparison) != 6 {
		t.Fatal(request, err)
	}
	if request.ClientHash != "" || request.OriginHash != "" {
		t.Fatal("private request identifiers exposed")
	}
	if _, err := s.Authenticate(ctx, request.ID, token); err == nil {
		t.Fatal("network request authorized itself")
	}
	if _, _, err := s.Join(ctx, "host", input); err != companion.ErrBusy {
		t.Fatal("duplicate request", err)
	}
	if err := s.Decide(ctx, id, "TV", request.ID, false); err != nil {
		t.Fatal(err)
	}
	// Ordinary rejection does not imply a 24-hour block.
	request, _, err = s.Join(ctx, "host", input)
	if err != nil {
		t.Fatal("default rejection unexpectedly blocked", err)
	}
	if err := s.Decide(ctx, id, "TV", request.ID, false, true); err != nil {
		t.Fatal(err)
	}
	restarted := companion.New(db, func() time.Time { return *now }, func(int) string { return strings.Repeat("c", 64) }, func(string) {})
	restarted.Poll(ctx, id, true)
	if _, _, err := restarted.Join(ctx, "changed-ip", input); err != companion.ErrDenied {
		t.Fatal("persistent device block", err)
	}
	input.ClientKey = strings.Repeat("d", 64)
	if _, _, err := restarted.Join(ctx, "host", input); err != companion.ErrDenied {
		t.Fatal("IP block did not bound key rotation", err)
	}
	*now = now.Add(24 * time.Hour)
	s.Poll(ctx, id, true)
	if _, _, err := s.Join(ctx, "host", input); err != nil {
		t.Fatal("block did not expire", err)
	}
}

func TestTextCommandsRequireCurrentInputLeaseAndRemainEphemeral(t *testing.T) {
	s, now, _ := setup(t)
	ctx := context.Background()
	grant, _ := accepted(t, s)
	lease := strings.Repeat("a", 32)
	s.Poll(ctx, grant.TargetID, true, lease)
	command, err := s.SendText(ctx, grant, "película 😀", lease)
	if err != nil || command.Text != "película 😀" {
		t.Fatal(command, err)
	}
	for _, text := range []string{"", "line\nbreak", strings.Repeat("a", 513)} {
		if _, err := s.SendText(ctx, grant, text, lease); err == nil {
			t.Fatal("invalid text accepted")
		}
	}
	if _, err := s.SendText(ctx, grant, "wrong field", strings.Repeat("b", 32)); err == nil {
		t.Fatal("stale focus accepted")
	}
	first := s.Poll(ctx, grant.TargetID, true, lease)
	if len(first) != 1 || first[0].InputID != lease {
		t.Fatal(first)
	}
	if len(s.Poll(ctx, grant.TargetID, true, lease)) != 0 {
		t.Fatal("text replay")
	}
	*now = now.Add(6 * time.Second)
	if s.TextInput(grant.TargetID) != "" {
		t.Fatal("offline field advertised")
	}
	s.Poll(ctx, grant.TargetID, false)
	if _, err := s.SendText(ctx, grant, "password", lease); err == nil {
		t.Fatal("inactive input accepted")
	}
}
