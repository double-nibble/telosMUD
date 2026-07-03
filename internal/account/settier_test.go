package account

import (
	"context"
	"testing"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
)

// settier_test.go — #27 Slice 4: SetAccountTier authorization + validation. Authz is enforced HERE (the
// actor must be an admin per the store), never at the edge.

func setTier(svc *Service, actor, target, tier string) (*accountv1.SetAccountTierResponse, error) {
	return svc.SetAccountTier(context.Background(), &accountv1.SetAccountTierRequest{
		ActorAccountId: actor, TargetCharacter: target, NewTier: tier,
	})
}

func TestSetAccountTierRequiresAdminActor(t *testing.T) {
	fs := newFakeStore()
	fs.tiers["acct-admin"] = "admin"
	fs.tiers["acct-player"] = "player"
	fs.tiers["acct-target"] = "player"
	fs.charAccount["Bob"] = "acct-target"
	svc := newTestService(fs)

	// A non-admin actor is refused (ok=false), and the target's tier is UNCHANGED.
	resp, err := setTier(svc, "acct-player", "Bob", "builder")
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOk() || resp.GetReason() == "" {
		t.Fatalf("a non-admin actor must be refused with a reason, got %+v", resp)
	}
	if fs.tiers["acct-target"] != "player" {
		t.Fatalf("a refused promote must not change the tier, got %q", fs.tiers["acct-target"])
	}

	// An UNKNOWN actor account (no tier row, found==false) is also refused — fail-closed, no write.
	if resp, _ := setTier(svc, "acct-ghost", "Bob", "builder"); resp.GetOk() {
		t.Fatal("an unknown actor account must be refused (found==false)")
	}
	if fs.tiers["acct-target"] != "player" {
		t.Fatal("an unknown-actor promote must not change the tier")
	}

	// An admin actor succeeds; the target is promoted and the old tier is reported.
	resp, err = setTier(svc, "acct-admin", "Bob", "builder")
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetOk() || resp.GetOldTier() != "player" {
		t.Fatalf("admin promote should succeed with old_tier=player, got %+v", resp)
	}
	if fs.tiers["acct-target"] != "builder" {
		t.Fatalf("target tier = %q, want builder", fs.tiers["acct-target"])
	}
}

func TestSetAccountTierValidation(t *testing.T) {
	fs := newFakeStore()
	fs.tiers["acct-admin"] = "admin"
	fs.tiers["acct-target"] = "player"
	fs.charAccount["Bob"] = "acct-target"
	svc := newTestService(fs)

	// Unknown tier → refused, no change.
	if resp, _ := setTier(svc, "acct-admin", "Bob", "wizard"); resp.GetOk() {
		t.Fatal("an unknown tier must be refused")
	}
	if fs.tiers["acct-target"] != "player" {
		t.Fatal("an invalid tier must not change the target")
	}

	// Unknown target character → refused.
	if resp, _ := setTier(svc, "acct-admin", "Nobody", "builder"); resp.GetOk() {
		t.Fatal("an unknown target character must be refused")
	}

	// Missing args → gRPC InvalidArgument.
	if _, err := setTier(svc, "", "Bob", "builder"); err == nil {
		t.Fatal("a missing actor should be an InvalidArgument error")
	}
}
