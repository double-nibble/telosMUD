package account

import (
	"context"
	"strings"
	"testing"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/content"
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

// TestSetAccountTierCeilings covers the two RANK-CEILING guards (service.go: "grant a tier above your own
// standing" / "change the tier of someone above your own standing") that had ZERO coverage. They are only
// reachable with a CONTENT ladder where an admin-granting tier ranks BELOW another admin-granting tier — the
// default ladder can't express "an actor below the tier it could otherwise grant", so a custom WithTrustLadder
// is load-bearing here: `gm` (rank 30) grants FlagAdmin (passes the manage-tiers gate) but sits under `admin`
// (rank 40).
func TestSetAccountTierCeilings(t *testing.T) {
	tiers := []content.TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "builder", Rank: 20, Flags: []string{content.FlagBuilder}},
		{Name: "gm", Rank: 30, Flags: []string{content.FlagAdmin}},
		{Name: "admin", Rank: 40, Flags: []string{content.FlagHolylight, content.FlagBuilder, content.FlagAdmin}},
	}
	fs := newFakeStore()
	fs.tiers["acct-gm"] = "gm"
	fs.tiers["acct-admin"] = "admin"
	fs.tiers["acct-lo"] = "player"
	fs.tiers["acct-peer"] = "player"
	fs.charAccount["Lowly"] = "acct-lo"
	fs.charAccount["Highness"] = "acct-admin" // an admin-tier target that OUTRANKS gm
	fs.charAccount["Peer"] = "acct-peer"
	svc := newTestService(fs).WithTrustLadder(tiers)

	// Ceiling 1 — grant-above-own: gm passes the manage-tiers gate (grants FlagAdmin) but may not grant a
	// tier ranked above its own standing (admin, rank 40 > gm's 30).
	if resp, _ := setTier(svc, "acct-gm", "Lowly", "admin"); resp.GetOk() || !strings.Contains(resp.GetReason(), "above your own standing") {
		t.Fatalf("gm granting admin must be refused 'above your own standing', got %+v", resp)
	}
	if fs.tiers["acct-lo"] != "player" {
		t.Fatalf("a refused grant must not change the tier, got %q", fs.tiers["acct-lo"])
	}

	// Ceiling 2 — change-above-you: gm may not change an account (Highness = admin, rank 40) that outranks
	// it, even to a LOW tier (the guard fires on the target's rank, not the requested one).
	if resp, _ := setTier(svc, "acct-gm", "Highness", "player"); resp.GetOk() || !strings.Contains(resp.GetReason(), "above your own standing") {
		t.Fatalf("gm changing an admin must be refused 'above your own standing', got %+v", resp)
	}
	if fs.tiers["acct-admin"] != "admin" {
		t.Fatalf("a refused change must not change the tier, got %q", fs.tiers["acct-admin"])
	}

	// Positive control A: gm MAY grant a tier at or below its own standing (builder, rank 20 <= 30).
	if resp, _ := setTier(svc, "acct-gm", "Lowly", "builder"); !resp.GetOk() {
		t.Fatalf("gm should be able to grant builder (<= its own rank), got %+v", resp)
	}
	if fs.tiers["acct-lo"] != "builder" {
		t.Fatalf("gm's builder grant did not apply, got %q", fs.tiers["acct-lo"])
	}

	// Positive control B: admin MAY grant admin (rank 40 == its own standing — equal is NOT "above").
	if resp, _ := setTier(svc, "acct-admin", "Peer", "admin"); !resp.GetOk() {
		t.Fatalf("admin should be able to grant admin (== its own rank), got %+v", resp)
	}
	if fs.tiers["acct-peer"] != "admin" {
		t.Fatalf("admin's admin grant did not apply, got %q", fs.tiers["acct-peer"])
	}
}
