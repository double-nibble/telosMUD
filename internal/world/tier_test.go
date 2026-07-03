package world

import (
	"context"
	"sync/atomic"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// tier_test.go — #27 Slice 2: a fresh-login attach records the account trust tier (from the VERIFIED session
// assertion, threaded through attachMsg) onto the session, so Slice 3 can apply the matching flags on spawn.

func TestAttachRecordsVerifiedTier(t *testing.T) {
	shard, z, _ := persistShard(t)
	out := make(chan *playv1.ServerFrame, 64)
	var cz atomic.Pointer[Zone]
	loaded, loadedOK := shard.loadCharacterSnapshot(context.Background(), "Buildy")

	z.post(attachMsg{character: "Buildy", out: out, curZone: &cz, loaded: loaded, loadedOK: loadedOK, tier: "admin"})
	waitPlayer(t, z, "Buildy", true)

	if s := z.players["Buildy"]; s == nil || s.tier != "admin" {
		got := "<no session>"
		if s != nil {
			got = s.tier
		}
		t.Fatalf("session tier = %q, want admin (recorded from the verified assertion)", got)
	}
}

// TestAttachDefaultsTierToPlayer: with no tier claim (the dev/unverified path — attachMsg.tier == ""), the
// session tier is empty, which the flag application (Slice 3) treats as player. Fail-safe: no elevation
// without a signed claim.
func TestAttachDefaultsTierToPlayer(t *testing.T) {
	shard, z, _ := persistShard(t)
	out := make(chan *playv1.ServerFrame, 64)
	var cz atomic.Pointer[Zone]
	loaded, loadedOK := shard.loadCharacterSnapshot(context.Background(), "Plain")

	z.post(attachMsg{character: "Plain", out: out, curZone: &cz, loaded: loaded, loadedOK: loadedOK}) // no tier
	waitPlayer(t, z, "Plain", true)

	if s := z.players["Plain"]; s == nil || s.tier != "" {
		t.Fatalf("session tier should be empty (player) with no signed claim, got %q", s.tier)
	}
}

// TestTierFlagsAllowlist pins the DEFAULT ladder's flag grants (Slice 3, now via the ladder — Round 9
// Slice 0): only exactly builder/admin elevate; "", player, and unknown values are the un-elevated
// baseline (fail-safe against drift). Checks the reserved flags each default tier grants.
func TestTierFlagsAllowlist(t *testing.T) {
	ladder := defaultTrustLadder()
	cases := map[string]map[string]bool{ // tier -> {holylight?, builder?, admin?}
		"admin":     {flagHolylight: true, flagBuilder: true, flagAdmin: true},
		"builder":   {flagHolylight: true, flagBuilder: true},
		"player":    {},
		"":          {},
		"ADMIN":     {}, // case-sensitive: not the canonical value
		"superuser": {}, // unknown / drifted → no elevation
	}
	for tier, want := range cases {
		got := map[string]bool{}
		for _, f := range ladder.grantedFlags(tier) {
			got[f] = true
		}
		for _, f := range []string{flagHolylight, flagBuilder, flagAdmin} {
			if got[f] != want[f] {
				t.Errorf("default ladder grantedFlags(%q): flag %q = %v, want %v", tier, f, got[f], want[f])
			}
		}
	}
}

// TestApplyTierFlagsReconciles proves applyTierFlags SETs granted flags and CLEARs the rest — so a
// demotion (admin → player) strips the elevated flags on the next login.
func TestApplyTierFlagsReconciles(t *testing.T) {
	_, caster := abilityTestZone(t)
	e := caster.entity

	applyTierFlags(e, "admin")
	if !hasFlag(e, flagHolylight) || !hasFlag(e, flagBuilder) || !hasFlag(e, flagAdmin) {
		t.Fatal("admin should grant holylight+builder+admin")
	}
	// Demote to builder: admin cleared, holylight+builder kept.
	applyTierFlags(e, "builder")
	if hasFlag(e, flagAdmin) {
		t.Error("demotion to builder should clear the admin flag")
	}
	if !hasFlag(e, flagHolylight) || !hasFlag(e, flagBuilder) {
		t.Error("builder should keep holylight+builder")
	}
	// Demote to player: all elevated flags cleared.
	applyTierFlags(e, "player")
	if hasFlag(e, flagHolylight) || hasFlag(e, flagBuilder) || hasFlag(e, flagAdmin) {
		t.Error("demotion to player should clear all elevated flags")
	}
}

// TestReservedFlagOpsRefused pins the #27/#28 denylist: content's set_flag/clear_flag can't touch a
// reserved trust flag (so a builder pack can't grant itself see-all, nor strip an admin's), while an
// ordinary flag still toggles.
func TestReservedFlagOpsRefused(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	c := seededCtx(z, e, e, dispHelpful)

	// A content set_flag of a reserved flag is a clean no-op (not set). Iterating the reservedFlags SET keeps
	// this exhaustive: every reserved flag — incl. flagWizinvis, the concealment flag no tier grants — is
	// covered, and a future addition to the set is auto-covered here.
	for f := range reservedFlags {
		if err := opSetFlag(c, &effectOp{flag: f}); err != nil {
			t.Fatalf("opSetFlag(%q): %v", f, err)
		}
		if hasFlag(e, f) {
			t.Errorf("content set_flag granted the reserved flag %q", f)
		}
	}

	// An ADMIN's holylight (set by the trusted tier path) can't be stripped by a content clear_flag.
	applyTierFlags(e, "admin")
	if err := opClearFlag(c, &effectOp{flag: flagHolylight}); err != nil {
		t.Fatalf("opClearFlag: %v", err)
	}
	if !hasFlag(e, flagHolylight) {
		t.Error("content clear_flag stripped an admin's reserved holylight flag")
	}

	// A non-reserved flag still works normally.
	if err := opSetFlag(c, &effectOp{flag: "guildmember"}); err != nil {
		t.Fatalf("opSetFlag(guildmember): %v", err)
	}
	if !hasFlag(e, "guildmember") {
		t.Error("a non-reserved flag should still be settable by content")
	}

	// Negative control on the reserved BOUNDARY: flagDetectInvis is DELIBERATELY not reserved (it is a
	// bounded game mechanic — a detect-invisibility effect/racial — not an engine capability), so content
	// MUST be able to set it. This pins that the reserved set stops at engine capabilities and does not
	// creep onto ordinary gameplay flags (the counterpart to flagWizinvis, which IS reserved).
	if reservedFlag(flagDetectInvis) {
		t.Fatal("flagDetectInvis must NOT be reserved — it is a game mechanic, not an engine capability")
	}
	if err := opSetFlag(c, &effectOp{flag: flagDetectInvis}); err != nil {
		t.Fatalf("opSetFlag(%q): %v", flagDetectInvis, err)
	}
	if !hasFlag(e, flagDetectInvis) {
		t.Errorf("content must be able to set the non-reserved %q flag", flagDetectInvis)
	}
}

// TestReservedFlagsNotPersistedOrRestored is the security-audit H-1 regression: the reserved trust flags
// are NEVER persisted (dumpFlags skips them) and NEVER installed from a state/handoff snapshot
// (applyStateComponents skips them) — so a forged/legacy snapshot carrying holylight/builder/admin can't
// inject the capability through the trusted restore path. Normal flags round-trip as before.
func TestReservedFlagsNotPersistedOrRestored(t *testing.T) {
	z, src := abilityTestZone(t)
	e := src.entity
	applyTierFlags(e, "admin") // sets holylight+builder+admin via the trusted tier path
	setFlag(e, "pvp", true)    // a normal, persistable flag

	snap := dumpCharacter(src)
	for _, f := range snap.State.Flags {
		if reservedFlag(f) {
			t.Errorf("reserved flag %q was persisted (must be tier-derived only)", f)
		}
	}
	if !containsStr(snap.State.Flags, "pvp") {
		t.Errorf("a normal flag should persist; got %v", snap.State.Flags)
	}

	// Forge the snapshot with EVERY reserved flag (as if a tampered handoff / injected DB row carried them) —
	// exhaustive over the reserved set, so flagWizinvis and any future addition are covered too.
	for f := range reservedFlags {
		snap.State.Flags = append(snap.State.Flags, f)
	}
	dst := &session{character: "Reload"}
	z.newPlayerEntity(dst, "Reload")
	loadCharacter(z, dst, snap)
	de := dst.entity
	for f := range reservedFlags {
		if hasFlag(de, f) {
			t.Errorf("reserved flag %q in the restored snapshot was installed — H-1: escalation via the trusted restore path", f)
		}
	}
	if !hasFlag(de, "pvp") {
		t.Error("a normal flag should restore")
	}
}

// TestAdminLoginAppliesTierFlags is the Slice1→2→3 end-to-end: a fresh login carrying an admin tier claim
// spawns with holylight+builder+admin; a player login spawns with none.
func TestAdminLoginAppliesTierFlags(t *testing.T) {
	shard, z, _ := persistShard(t)

	spawn := func(name, tier string) *Entity {
		out := make(chan *playv1.ServerFrame, 64)
		var cz atomic.Pointer[Zone]
		loaded, loadedOK := shard.loadCharacterSnapshot(context.Background(), name)
		z.post(attachMsg{character: name, out: out, curZone: &cz, loaded: loaded, loadedOK: loadedOK, tier: tier})
		waitPlayer(t, z, name, true)
		return z.players[name].entity
	}

	admin := spawn("Wizard", "admin")
	if !hasFlag(admin, flagHolylight) || !hasFlag(admin, flagBuilder) || !hasFlag(admin, flagAdmin) {
		t.Error("an admin login should spawn with holylight+builder+admin flags")
	}
	plain := spawn("Peasant", "player")
	if hasFlag(plain, flagHolylight) || hasFlag(plain, flagBuilder) || hasFlag(plain, flagAdmin) {
		t.Error("a player login must spawn with no elevated flags")
	}
}
