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

// capLadder is the load-bearing ceiling ladder (#165). It carries BOTH failure shapes the rank ceiling alone
// cannot separate:
//   - same rank, richer flags: gm(30,{admin}) vs warden(30,{admin,holylight});
//   - LOWER rank, orthogonal flags: mod(30,{admin}) vs builder(20,{holylight,builder}) — every rank distinct,
//     so no duplicate-rank lint would help. This is the case that proves the fix is not a same-rank patch.
var capLadder = []content.TrustTierDTO{
	{Name: "player", Rank: 0},
	{Name: "builder", Rank: 20, Flags: []string{content.FlagHolylight, content.FlagBuilder}},
	{Name: "gm", Rank: 30, Flags: []string{content.FlagAdmin}},
	{Name: "mod", Rank: 30, Flags: []string{content.FlagAdmin}}, // same caps as gm, distinct name
	{Name: "warden", Rank: 30, Flags: []string{content.FlagAdmin, content.FlagHolylight}},
	{Name: "admin", Rank: 40, Flags: []string{content.FlagHolylight, content.FlagBuilder, content.FlagAdmin}},
}

func newCapSvc(fs *fakeStore) *Service { return newTestService(fs).WithTrustLadder(capLadder) }

// TestSetAccountTierGrantCeiling covers the GRANT-side capability ceiling (#165) — unconditional, because
// writing a tier mints its capabilities at the target's next login. It must refuse both the same-rank and the
// cross-rank richer-tier grant, and it must NOT break a legitimate promote.
func TestSetAccountTierGrantCeiling(t *testing.T) {
	// (1) SELF, same rank: a gm may not self-promote to the same-rank warden — the rank ceiling passes
	// (30 > 30 is false), so ONLY the capability ceiling stops the holylight self-grant.
	{
		fs := newFakeStore()
		fs.tiers["gm"] = "gm"
		fs.charAccount["Self"] = "gm"
		resp, err := setTier(newCapSvc(fs), "gm", "Self", "warden")
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetOk() || !strings.Contains(resp.GetReason(), "does not hold") || !strings.Contains(resp.GetReason(), "holylight") {
			t.Fatalf("gm must not self-grant same-rank warden (holylight escalation), got %+v", resp)
		}
		if fs.tiers["gm"] != "gm" {
			t.Fatalf("a refused self-grant must not write, got %q", fs.tiers["gm"])
		}
	}

	// (2) OTHER, same rank: the same escalation laundered through a third party.
	{
		fs := newFakeStore()
		fs.tiers["gm"] = "gm"
		fs.tiers["acct-alt"] = "player"
		fs.charAccount["Alt"] = "acct-alt"
		if resp, _ := setTier(newCapSvc(fs), "gm", "Alt", "warden"); resp.GetOk() || !strings.Contains(resp.GetReason(), "holylight") {
			t.Fatalf("gm must not grant same-rank warden to anyone, got %+v", resp)
		}
		if fs.tiers["acct-alt"] != "player" {
			t.Fatalf("a refused grant must not write, got %q", fs.tiers["acct-alt"])
		}
	}

	// (3, T-2) CROSS-RANK: mod(30,{admin}) OUTRANKS builder(20) but holds orthogonal caps, so it may not mint
	// a builder's holylight+builder. This is the case #111's duplicate-rank lint could never catch.
	{
		fs := newFakeStore()
		fs.tiers["acct-mod"] = "mod"
		fs.tiers["acct-alt"] = "player"
		fs.charAccount["Alt"] = "acct-alt"
		resp, _ := setTier(newCapSvc(fs), "acct-mod", "Alt", "builder")
		if resp.GetOk() || !strings.Contains(resp.GetReason(), "does not hold") {
			t.Fatalf("mod must not grant builder (holds holylight+builder it lacks) despite outranking it, got %+v", resp)
		}
		if fs.tiers["acct-alt"] != "player" {
			t.Fatalf("a refused cross-rank grant must not write, got %q", fs.tiers["acct-alt"])
		}
	}

	// (4) POSITIVE, richer peer grants poorer: warden -> gm is a capability subset, so it is allowed.
	{
		fs := newFakeStore()
		fs.tiers["acct-warden"] = "warden"
		fs.tiers["acct-alt"] = "player"
		fs.charAccount["Alt"] = "acct-alt"
		if resp, _ := setTier(newCapSvc(fs), "acct-warden", "Alt", "gm"); !resp.GetOk() || fs.tiers["acct-alt"] != "gm" {
			t.Fatalf("warden should be able to grant the strictly-poorer gm, got resp=%+v tier=%q", resp, fs.tiers["acct-alt"])
		}
	}

	// (5) POSITIVE, top rank: admin holds every capability, so the fix is vacuous for it — the regression guard
	// for "the ceiling broke promote".
	{
		fs := newFakeStore()
		fs.tiers["acct-admin"] = "admin"
		fs.tiers["acct-alt"] = "player"
		fs.charAccount["Alt"] = "acct-alt"
		svc := newCapSvc(fs)
		if resp, _ := setTier(svc, "acct-admin", "Alt", "warden"); !resp.GetOk() || fs.tiers["acct-alt"] != "warden" {
			t.Fatalf("admin should be able to grant warden, got resp=%+v tier=%q", resp, fs.tiers["acct-alt"])
		}
		if resp, _ := setTier(svc, "acct-admin", "Alt", "player"); !resp.GetOk() || fs.tiers["acct-alt"] != "player" {
			t.Fatalf("admin should be able to demote a warden, got resp=%+v tier=%q", resp, fs.tiers["acct-alt"])
		}
	}
}

// TestSetAccountTierTargetCeiling covers the TARGET-side capability ceiling (#165) and its DELIBERATE asymmetry
// with the grant side: it fires only at EQUAL rank. Above equal rank the actor already dominates by rank and
// stripping capability is revocation, not escalation — refusing there would strand accounts no one can demote
// (the F2 lockout the review caught). So a gm may not strip a peer warden's holylight, but a strictly-higher
// admin may always manage anyone.
func TestSetAccountTierTargetCeiling(t *testing.T) {
	// (1) EQUAL RANK: a gm may not change a same-rank warden — stripping a peer's holylight reaches a capability
	// it never held, and rank gives no separation.
	{
		fs := newFakeStore()
		fs.tiers["gm"] = "gm"
		fs.tiers["acct-warden"] = "warden"
		fs.charAccount["Warden"] = "acct-warden"
		resp, _ := setTier(newCapSvc(fs), "gm", "Warden", "player")
		if resp.GetOk() || !strings.Contains(resp.GetReason(), "cannot change their tier") {
			t.Fatalf("gm must not change a same-rank warden, got %+v", resp)
		}
		if fs.tiers["acct-warden"] != "warden" {
			t.Fatalf("a refused change must not write, got %q", fs.tiers["acct-warden"])
		}
	}

	// (2, T-3) ABOVE RANK: the asymmetry. A strictly-higher admin MAY demote a builder even though — in the
	// default nested ladder it would dominate anyway; here we use a non-nested wrinkle to prove the point:
	// admin(40) dominates builder(20) so this is allowed, and (crucially) it is NOT refused by a target-side
	// capability check because the check is skipped above equal rank. Pins that revocation is never lost.
	{
		fs := newFakeStore()
		fs.tiers["acct-admin"] = "admin"
		fs.tiers["acct-builder"] = "builder"
		fs.charAccount["Builder"] = "acct-builder"
		if resp, _ := setTier(newCapSvc(fs), "acct-admin", "Builder", "player"); !resp.GetOk() || fs.tiers["acct-builder"] != "player" {
			t.Fatalf("a strictly-higher admin must be able to demote a builder, got resp=%+v tier=%q", resp, fs.tiers["acct-builder"])
		}
	}

	// (3) EQUAL RANK, same caps: a gm may manage a peer gm (equal capabilities) — the target ceiling only bites
	// on a capability the actor lacks.
	{
		fs := newFakeStore()
		fs.tiers["gm1"] = "gm"
		fs.tiers["gm2"] = "gm"
		fs.charAccount["Peer"] = "gm2"
		if resp, _ := setTier(newCapSvc(fs), "gm1", "Peer", "player"); !resp.GetOk() || fs.tiers["gm2"] != "player" {
			t.Fatalf("a gm should be able to manage a same-capability peer gm, got resp=%+v tier=%q", resp, fs.tiers["gm2"])
		}
	}
}

// TestSetAccountTierNoDisclosureOrdering (T-4) pins that the grant-side ceiling runs BEFORE target resolution,
// so a `promote NoSuchCharacter <rich-tier>` returns the capability refusal, never "No such character." — the
// authz decision must not leak whether a character exists. It is the property most likely to regress under a
// refactor that hoists target resolution up.
func TestSetAccountTierNoDisclosureOrdering(t *testing.T) {
	fs := newFakeStore()
	fs.tiers["gm"] = "gm" // gm lacks holylight, so warden trips the grant ceiling
	// NOTE: no charAccount entry for the target — it does not exist.
	resp, err := setTier(newCapSvc(fs), "gm", "Ghostly", "warden")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.GetReason(), "No such character") {
		t.Fatalf("the grant ceiling must refuse BEFORE disclosing whether the character exists, got %+v", resp)
	}
	if resp.GetOk() || !strings.Contains(resp.GetReason(), "does not hold") {
		t.Fatalf("expected the capability refusal, got %+v", resp)
	}
}

// TestSetAccountTierCASConflict (#165 F3) proves the ceilings are binding, not advisory: when a concurrent
// promote moves the target's tier inside the service's check→write window, the write is refused with a
// try-again message and nothing is written. Driven via the fake store's beforeSetTier hook.
func TestSetAccountTierCASConflict(t *testing.T) {
	fs := newFakeStore()
	fs.tiers["acct-admin"] = "admin"
	fs.tiers["acct-bob"] = "player"
	fs.charAccount["Bob"] = "acct-bob"
	svc := newCapSvc(fs)

	// The admin authorizes against Bob=player, but a concurrent promote makes Bob a warden before the write.
	fs.beforeSetTier = func() {
		fs.tiers["acct-bob"] = "warden"
		fs.beforeSetTier = nil // fire once
	}
	resp, err := setTier(svc, "acct-admin", "Bob", "gm")
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOk() || !strings.Contains(resp.GetReason(), "changed while you were acting") {
		t.Fatalf("a CAS conflict must be surfaced as a retry, got %+v", resp)
	}
	if fs.tiers["acct-bob"] != "warden" {
		t.Fatalf("the conflicting write must not land, got %q", fs.tiers["acct-bob"])
	}
}

// TestSetAccountTierUnknownStoredTierFailsClosed (#165 F6) pins that a target whose STORED tier is not in the
// loaded ladder is refused, not silently treated as the baseline. accounts.tier is free-form TEXT (migration
// 00021 dropped the CHECK), so a renamed/removed rung — or telos-account loading a different pack set than the
// world (#246) — can leave such rows; reading them as the baseline would void both target-side ceilings.
func TestSetAccountTierUnknownStoredTierFailsClosed(t *testing.T) {
	fs := newFakeStore()
	fs.tiers["acct-admin"] = "admin"
	fs.tiers["acct-bob"] = "sorcerer" // a tier the ladder does not define
	fs.charAccount["Bob"] = "acct-bob"
	resp, err := setTier(newCapSvc(fs), "acct-admin", "Bob", "player")
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOk() || !strings.Contains(resp.GetReason(), "not defined in the loaded content ladder") {
		t.Fatalf("an unrecognized stored tier must fail closed, got %+v", resp)
	}
	if fs.tiers["acct-bob"] != "sorcerer" {
		t.Fatalf("a fail-closed refusal must not write, got %q", fs.tiers["acct-bob"])
	}
}

// TestSetAccountTierDemoteToBaseline (#112) pins the empty-tier sentinel: `demote` sends "" and the service
// resolves it to the ladder's BASELINE (lowest-rank tier), never a hardcoded "player". A pack that renames the
// baseline must still demote correctly, and the response reports the RESOLVED tier so the edge can confirm it.
func TestSetAccountTierDemoteToBaseline(t *testing.T) {
	// A ladder whose baseline is renamed "mortal" — the exact case a hardcoded "player" would break.
	tiers := []content.TrustTierDTO{
		{Name: "mortal", Rank: 0},
		{Name: "wizard", Rank: 40, Flags: []string{content.FlagHolylight, content.FlagBuilder, content.FlagAdmin}},
	}
	fs := newFakeStore()
	fs.tiers["acct-wiz"] = "wizard"
	fs.tiers["acct-bob"] = "wizard"
	fs.charAccount["Bob"] = "acct-bob"
	svc := newTestService(fs).WithTrustLadder(tiers)

	resp, err := setTier(svc, "acct-wiz", "Bob", "") // the demote sentinel
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetOk() {
		t.Fatalf("demote-to-baseline should succeed, got %+v", resp)
	}
	if resp.GetNewTier() != "mortal" {
		t.Errorf("the response must report the RESOLVED baseline, got new_tier=%q want mortal", resp.GetNewTier())
	}
	if fs.tiers["acct-bob"] != "mortal" {
		t.Errorf("the target must be demoted to the renamed baseline, got %q", fs.tiers["acct-bob"])
	}

	// The default ladder resolves the sentinel to "player" (byte-for-byte round-8 behavior).
	fd := newFakeStore()
	fd.tiers["acct-admin"] = "admin"
	fd.tiers["acct-t"] = "builder"
	fd.charAccount["T"] = "acct-t"
	if resp, _ := setTier(newTestService(fd), "acct-admin", "T", ""); resp.GetNewTier() != "player" || fd.tiers["acct-t"] != "player" {
		t.Errorf("the default ladder must demote the sentinel to player, got new_tier=%q tier=%q", resp.GetNewTier(), fd.tiers["acct-t"])
	}
}
