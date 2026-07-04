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

// TestSetAccountTierAuthzEdges covers the escalation-adjacent SetAccountTier edges NOT already pinned by
// TestSetAccountTierRankCeiling (service_test.go, which covers grant-above-own + change-above-you): (1) SELF-
// targeting — there is no special self-path, so the grant ceiling still blocks a self-promote-above, while a
// self-DEMOTE is allowed (pinning the current behavior: no self-demote guard); (2) the CHANGE-side EQUAL-rank
// positive control (an admin may manage a peer admin — guards a `>`→`>=` tightening); and (3) the "target has
// no tier row → rank-0 baseline" degradation (the `found` flag is deliberately ignored). Each sub-scenario
// uses its own fake store so mutations don't couple. `gm` (rank 30, FlagAdmin) under `admin` (rank 40) is the
// load-bearing custom ladder.
func TestSetAccountTierAuthzEdges(t *testing.T) {
	tiers := []content.TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "gm", Rank: 30, Flags: []string{content.FlagAdmin}},
		{Name: "admin", Rank: 40, Flags: []string{content.FlagAdmin}},
	}
	newSvc := func(fs *fakeStore) *Service { return newTestService(fs).WithTrustLadder(tiers) }

	// (1a) SELF escalation: gm may not self-promote ABOVE its own rank — the grant ceiling applies to self.
	{
		fs := newFakeStore()
		fs.tiers["gm"] = "gm"
		fs.charAccount["Self"] = "gm"
		if resp, _ := setTier(newSvc(fs), "gm", "Self", "admin"); resp.GetOk() || !strings.Contains(resp.GetReason(), "above your own standing") {
			t.Fatalf("gm must not self-promote to admin, got %+v", resp)
		}
		if fs.tiers["gm"] != "gm" {
			t.Fatalf("a refused self-promote must not change the tier, got %q", fs.tiers["gm"])
		}
	}

	// (1b) SELF de-escalation: an admin MAY self-demote (rank(target)==rank(actor), not above) — pins the
	// CURRENT behavior that no self-demote guard exists (an admin can strip its own admin).
	{
		fs := newFakeStore()
		fs.tiers["adm"] = "admin"
		fs.charAccount["Self"] = "adm"
		if resp, _ := setTier(newSvc(fs), "adm", "Self", "player"); !resp.GetOk() || fs.tiers["adm"] != "player" {
			t.Fatalf("an admin should be able to self-demote (equal rank), got resp=%+v tier=%q", resp, fs.tiers["adm"])
		}
	}

	// (2) CHANGE-side EQUAL-rank positive control: an admin MAY change a same-rank admin (the change ceiling
	// is strictly-greater) — guards against a `>`→`>=` tightening breaking admins-managing-peers.
	{
		fs := newFakeStore()
		fs.tiers["a1"] = "admin"
		fs.tiers["a2"] = "admin"
		fs.charAccount["Peer"] = "a2"
		if resp, _ := setTier(newSvc(fs), "a1", "Peer", "player"); !resp.GetOk() || fs.tiers["a2"] != "player" {
			t.Fatalf("an admin should be able to change a same-rank admin, got resp=%+v tier=%q", resp, fs.tiers["a2"])
		}
	}

	// (3) TARGET-BASELINE degradation: a target whose account has NO tier row reads as the rank-0 baseline
	// (Rank("")==0), which gm outranks — so the change is ALLOWED. The guard degrades to "allow the baseline",
	// never "skip the check" (the `found` flag is deliberately ignored).
	{
		fs := newFakeStore()
		fs.tiers["gm"] = "gm"
		fs.charAccount["NoTier"] = "acct-notier" // resolves, but has no fs.tiers entry
		if resp, _ := setTier(newSvc(fs), "gm", "NoTier", "player"); !resp.GetOk() || fs.tiers["acct-notier"] != "player" {
			t.Fatalf("gm should be able to change a no-tier-row target (rank-0 baseline), got resp=%+v tier=%q", resp, fs.tiers["acct-notier"])
		}
	}
}
