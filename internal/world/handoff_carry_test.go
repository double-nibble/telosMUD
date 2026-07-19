package world

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/test/bufconn"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/directory"
)

// handoff_carry_test.go pins the cross-shard full-state carry (the live "walking midgaard->darkwood
// drops equipment/inventory/stats/affects" bug). buildSnapshot now carries the player's REMAINING
// entity state (state_json, field 15) and the destination prepare re-installs it via the SAME applier
// loadCharacter uses (applyStateComponents) — so a fresh login and a handoff arrive at identical state.

// seamPlayer builds a fully-stated demo player on z: gear (a worn helmet + a carried torch), bumped
// attribute bases + a wounded resource, an active affect with a decayed remaining, and an armed
// cooldown — the union of every subtree the carry must conserve.
func seamPlayer(t *testing.T, z *Zone, name string) *session {
	t.Helper()
	s := &session{character: name, stateVersion: 3}
	e := z.newPlayerEntity(s, name)

	// Gear: wear a helmet (a Wearer slot) + carry a torch (plain inventory).
	helmet, _ := loadItem(z, e, ItemJSON{ProtoRef: "midgaard:obj:helmet"}, 0)
	if helmet == nil {
		t.Fatal("seam setup: helmet prototype missing from demo pack")
	}
	wr := actorWearer(e)
	wr.worn[WearLocHead] = helmet
	if torch, _ := loadItem(z, e, ItemJSON{ProtoRef: "midgaard:obj:torch"}, 0); torch == nil {
		t.Fatal("seam setup: torch prototype missing from demo pack")
	}

	// Stats: bump constitution+level (raises derived max_hp) and wound hp.
	setAttrBase(e, "constitution", 14)
	setAttrBase(e, "level", 5)
	e.SetHP(100)

	// Affect: weaken (duration 20), decayed to remaining 7 — conserved, not reset to full.
	inst := applyAffect(e, "weaken", attachOpts{}, nil)
	if inst == nil {
		t.Fatal("seam setup: could not apply weaken affect")
	}
	inst.remaining = 7

	// Cooldown: arm "bash" with 9 pulses remaining.
	z.rearmCooldown(s, "bash", 9)
	return s
}

// TestCrossShardCarrySeam drives the carry at the world handoff seam: buildSnapshot on a source zone,
// then prepare on a SEPARATE destination zone, and asserts the destination entity has the gear,
// attributes, resources, the affect (with REMAINING conserved), and the cooldown.
func TestCrossShardCarrySeam(t *testing.T) {
	src := newDemoZone("midgaard", newProtoCache())
	dst := newDemoZone("midgaard", newProtoCache()) // same content, distinct zone instance (the destination shard)

	s := seamPlayer(t, src, "Carrier")
	snap := buildSnapshot(s)

	// The carry must be populated (not the pre-fix empty), and must NOT route AppliedSeq or comms.
	if snap.GetStateJson() == "" {
		t.Fatal("buildSnapshot carried no state_json (the full-state-carry regression)")
	}

	// Destination prepare rehydrates the pending entity from the snapshot.
	reply := make(chan error, 1)
	dst.claimInboundArrival() // the claim the production resolver takes under s.mu; the handler releases one unconditionally (#413)
	dst.prepare(prepareMsg{snap: snap, room: "", epoch: 5, token: "tok", reply: reply})
	if err := <-reply; err != nil {
		t.Fatalf("prepare replied error: %v", err)
	}
	ds := dst.players["Carrier"]
	if ds == nil || ds.entity == nil {
		t.Fatal("prepare did not park a pending entity")
	}
	de := ds.entity

	// Equipment: helmet on the head slot.
	dwr, ok := Get[*Wearer](de)
	if !ok || dwr.worn[WearLocHead] == nil {
		t.Fatal("carry: worn helmet did not survive the hop")
	}
	if got := string(dwr.worn[WearLocHead].proto); got != "midgaard:obj:helmet" {
		t.Fatalf("carry: head slot = %q, want midgaard:obj:helmet", got)
	}
	// Inventory: the torch is carried (the helmet is worn, so it is in contents but excluded from the
	// inventory list — count only non-worn protos).
	worn := wornSet(de)
	var carried []string
	for _, it := range de.contents {
		if it.proto != "" && !worn[it] {
			carried = append(carried, string(it.proto))
		}
	}
	if len(carried) != 1 || carried[0] != "midgaard:obj:torch" {
		t.Fatalf("carry: inventory = %v, want [midgaard:obj:torch]", carried)
	}

	// Attributes + resources: bases conserved, derived max recomputed, wounded hp conserved.
	if got := de.Attr("constitution"); got != 14 {
		t.Fatalf("carry: constitution = %v, want 14", got)
	}
	if got := de.MaxHP(); got != 165 {
		t.Fatalf("carry: MaxHP() = %d, want recomputed 165", got)
	}
	if got := de.HP(); got != 100 {
		t.Fatalf("carry: HP() = %d, want conserved 100", got)
	}

	// Affect: present with REMAINING conserved (7), NOT reset to the def's full 20.
	a, ok := Get[*Affected](de)
	if !ok || len(a.list) != 1 {
		t.Fatalf("carry: affects = %v, want exactly one (weaken)", a)
	}
	if a.list[0].def.ref != "weaken" {
		t.Fatalf("carry: affect ref = %q, want weaken", a.list[0].def.ref)
	}
	if a.list[0].remaining != 7 {
		t.Fatalf("carry: affect remaining = %d, want conserved 7 (NOT reset to 20)", a.list[0].remaining)
	}

	// Cooldown: re-armed (present on the destination, remaining > 0).
	if de.living == nil || len(de.living.cooldowns) != 1 {
		t.Fatalf("carry: cooldowns = %v, want bash re-armed", de.living.cooldowns)
	}
	if _, ok := de.living.cooldowns["bash"]; !ok {
		t.Fatalf("carry: cooldowns = %v, want bash present", de.living.cooldowns)
	}

	// Linchpin: AppliedSeq is seeded from the dedicated field, not the embedded carry.
	if ds.appliedSeq != snap.GetAppliedSeq() {
		t.Fatalf("carry: appliedSeq = %d, want %d (from the dedicated field)", ds.appliedSeq, snap.GetAppliedSeq())
	}
	if ds.stateVersion != 3 {
		t.Fatalf("carry: stateVersion = %d, want 3 (CAS base unchanged)", ds.stateVersion)
	}
}

// TestCrossShardCarryParityWithSaveLoad asserts an item survives the hop with PARITY to the save/load
// path (NOT asserting COW-delta fidelity the engine lacks on either path): the same prototype + the
// same container nesting round-trip, exactly as dumpCharacter/loadCharacter would.
func TestCrossShardCarryParityWithSaveLoad(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	src := &session{character: "Parity"}
	e := z.newPlayerEntity(src, "Parity")
	if torch, _ := loadItem(z, e, ItemJSON{ProtoRef: "midgaard:obj:torch"}, 0); torch == nil {
		t.Fatal("parity setup: torch prototype missing")
	}

	// save/load path: dumpCharacter -> loadCharacter into a fresh entity.
	snapSL := dumpCharacter(src)
	dstSL := &session{character: "Parity"}
	z.newPlayerEntity(dstSL, "Parity")
	loadCharacter(z, dstSL, snapSL)

	// handoff path: buildSnapshot -> prepare on a fresh destination zone.
	dstZone := newDemoZone("midgaard", newProtoCache())
	hsnap := buildSnapshot(src)
	reply := make(chan error, 1)
	dstZone.claimInboundArrival() // the claim the production resolver takes under s.mu; the handler releases one unconditionally (#413)
	dstZone.prepare(prepareMsg{snap: hsnap, room: "", epoch: 2, token: "tok", reply: reply})
	if err := <-reply; err != nil {
		t.Fatalf("prepare replied error: %v", err)
	}
	dstHO := dstZone.players["Parity"]

	protosOf := func(e *Entity) []string {
		var out []string
		for _, it := range e.contents {
			if it.proto != "" {
				out = append(out, string(it.proto))
			}
		}
		return out
	}
	slProtos := protosOf(dstSL.entity)
	hoProtos := protosOf(dstHO.entity)
	if len(slProtos) != 1 || len(hoProtos) != 1 || slProtos[0] != hoProtos[0] {
		t.Fatalf("carry/save-load parity broken: save-load=%v handoff=%v", slProtos, hoProtos)
	}
}

// TestCrossShardCarryBareSnapshotEmpty: a player with no entity state carries an empty state_json
// (parity with the all-default comms "" case), so the destination resolves from defaults exactly
// as a pre-fix snapshot — the bare-engine / backward-compat default.
func TestCrossShardCarryBareSnapshotEmpty(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := &session{character: "Bare"}
	z.newPlayerEntity(s, "Bare")
	// A freshly built player carries default attributes only (no overrides), no items/affects/cooldowns.
	if got := buildSnapshot(s).GetStateJson(); got != "" {
		t.Fatalf("bare player carried state_json = %q, want empty", got)
	}
}

// TestApplyStateClampsResourceAfterMaxRaisingAffect is the regression for the latent ordering bug
// the combat reviewer caught in applyStateComponents (the SHARED applier, so it covers BOTH the
// save/load reload path AND the cross-shard handoff carry): resource currents must be installed +
// clamped AFTER attributes, affects, and gear — i.e. to the genuinely-final derived max — not to a
// pre-affect/pre-gear max the read-side clamp (which only clamps DOWN) could never recover. We
// register a max-RAISING affect (the demo lacks one), wound the player to a current that sits ABOVE
// the base max but within the boosted max, dump, then re-apply via loadCharacter and assert the
// wounded current is CONSERVED. With the pre-fix ordering this clamped down to the base max.
func TestApplyStateClampsResourceAfterMaxRaisingAffect(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	// A demo affect that RAISES max_hp by +50 (the demo's own affects only ever lower attributes).
	z.defs.affect.register("vigor", &affectDef{
		ref: "vigor", name: "Vigorous", stacking: stackRefresh, maxStacks: 1, duration: 30,
		modifiers: []affectModifier{{attr: "max_hp", add: true, value: 50}},
	})

	src := &session{character: "Buffed"}
	e := z.newPlayerEntity(src, "Buffed")
	// Base max_hp = constitution(10)*10 + level(1)*5 = 105.
	if got := e.MaxHP(); got != 105 {
		t.Fatalf("base MaxHP() = %d, want 105", got)
	}
	// Apply the buff: max_hp now 155. Wound to 150 — ABOVE the base max (105), within the boosted max.
	applyAffect(e, "vigor", attachOpts{}, nil)
	if got := e.MaxHP(); got != 155 {
		t.Fatalf("buffed MaxHP() = %d, want 155", got)
	}
	e.SetHP(150)
	if got := e.HP(); got != 150 {
		t.Fatalf("source HP() = %d, want 150", got)
	}

	snap := dumpCharacter(src)
	if snap.State.Resources["hp"].Cur != 150 {
		t.Fatalf("dumped hp cur = %d, want 150", snap.State.Resources["hp"].Cur)
	}

	// Reload into a fresh entity via the shared applier (loadCharacter -> applyStateComponents).
	dst := &session{character: "Buffed"}
	z.newPlayerEntity(dst, "Buffed")
	loadCharacter(z, dst, snap)
	de := dst.entity

	// The buff re-attached and the max recomputed to 155; the wounded current is CONSERVED at 150,
	// NOT clamped down to the base max 105 (the bug). This pins the install-currents-LAST ordering.
	if got := de.MaxHP(); got != 155 {
		t.Fatalf("reloaded MaxHP() = %d, want recomputed 155", got)
	}
	if got := de.HP(); got != 150 {
		t.Fatalf("reloaded HP() = %d, want conserved 150 (pre-fix bug clamped to 105)", got)
	}
}

// TestCrossShardCarryEndToEnd drives the carry BLACK-BOX through the two-shard gate harness (the
// real Prepare wire path): a player picks up + wears a helmet in midgaard's market, walks
// midgaard->darkwood, and on the destination `equipment` STILL shows the helmet. Pre-fix this
// dropped on the hop. Gear is set up via real player commands (get/wear), so the whole journey is
// black-box end-to-end. The affect/attribute/resource/cooldown conservation is pinned by the seam
// test above (no player command self-applies those). It reuses the TestEpochResumeOnRelogin harness.
func TestCrossShardCarryEndToEnd(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-b"))

	lisA := bufconn.Listen(1 << 20)
	lisB := bufconn.Listen(1 << 20)
	lisByAddr := map[string]*bufconn.Listener{"addr-a": lisA, "addr-b": lisB}
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		lis := lisByAddr[addr]
		if lis == nil {
			return nil, fmt.Errorf("unknown shard %q", addr)
		}
		return handoffv1.NewHandoffClient(dialBuf(t, lis)), nil
	}
	bPlay := serveShard(t, NewShard("darkwood", "addr-b", dir, peers), lisB)
	aPlay := serveShard(t, NewShard("midgaard", "addr-a", dir, peers), lisA)

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sA, err := aPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sA, attach("Geared"))
	recvAttached(t, sA)

	// Walk temple -> market (the helmet is reset onto the market floor), then get + wear it.
	send(t, sA, inputSeq(1, "north")) // temple -> market
	recvUntilOutput(t, sA, "Market Square")
	send(t, sA, inputSeq(2, "get helmet"))
	recvUntilOutput(t, sA, "helmet")
	send(t, sA, inputSeq(3, "wear helmet"))
	recvUntilOutput(t, sA, "helmet")

	// Walk market -> darkwood (the cross-shard hop).
	send(t, sA, inputSeq(4, "north"))
	redirB := recvRedirect(t, sA)

	sB, err := bPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sB, attachWithToken("Geared", redirB.GetHandoffToken()))
	recvAttached(t, sB)
	recvUntilOutput(t, sB, "Moonlit Grove")

	// On the destination, the worn helmet survived the hop (pre-fix: equipment was empty). The
	// input seq must CONTINUE past the carried appliedSeq (the source applied seqs 1-4): a seq <=
	// the carried high-water mark is dropped as a replay (exactly-once across the hop), so the gate
	// resumes numbering — seq 5 here.
	send(t, sB, inputSeq(5, "equipment"))
	recvUntilOutput(t, sB, "iron helmet")
}

// TestCrossShardTierCarry pins #106: the account trust tier rides the SIGNED snapshot and the destination
// re-DERIVES the reserved flags from it — while the flags THEMSELVES are NOT carried (H-1: a flag restore
// bypasses the content op guard, so a forged snapshot must not be able to inject one). Concretely: a source
// admin has holylight set; buildSnapshot carries tier="admin" but NOT the flag; prepare parks the pending
// session with s.tier=="admin" and the entity WITHOUT holylight; applying the tier (as attach does on
// activation) restores it.
func TestCrossShardTierCarry(t *testing.T) {
	src := newDemoZone("midgaard", newProtoCache())
	dst := newDemoZone("midgaard", newProtoCache())

	s := &session{character: "Wizard", stateVersion: 1}
	e := src.newPlayerEntity(s, "Wizard")
	Add(e, &Living{})
	// The source session is an admin with the derived reserved flags actually set (the login reconcile).
	s.tier = "admin"
	applyTierFlags(e, "admin")
	if !hasFlag(e, flagHolylight) || !hasFlag(e, flagAdmin) {
		t.Fatal("source admin must hold the reserved flags before the hop")
	}

	snap := buildSnapshot(s)
	if snap.GetTier() != "admin" {
		t.Fatalf("the snapshot must carry the tier, got %q", snap.GetTier())
	}
	// The reserved flags must NOT ride the carried state (H-1): dumpFlags omits them.
	if raw := snap.GetStateJson(); raw != "" {
		var st StateJSON
		if err := json.Unmarshal([]byte(raw), &st); err == nil {
			for _, f := range st.Flags {
				if reservedFlag(f) {
					t.Fatalf("a reserved flag %q must never be carried in the handoff state_json (H-1)", f)
				}
			}
		}
	}

	// Destination prepare parks the pending session carrying the tier but WITHOUT the derived flags yet.
	reply := make(chan error, 1)
	dst.claimInboundArrival() // the claim the production resolver takes under s.mu; the handler releases one unconditionally (#413)
	dst.prepare(prepareMsg{snap: snap, room: "", epoch: 2, token: "tok", reply: reply})
	if err := <-reply; err != nil {
		t.Fatalf("prepare replied error: %v", err)
	}
	ds := dst.players["Wizard"]
	if ds == nil || ds.entity == nil {
		t.Fatal("prepare did not park a pending entity")
	}
	if ds.tier != "admin" {
		t.Fatalf("the pending session must carry the tier, got %q", ds.tier)
	}
	if hasFlag(ds.entity, flagHolylight) || hasFlag(ds.entity, flagAdmin) {
		t.Fatal("the reserved flags must NOT be present before activation (they are re-derived, not carried)")
	}

	// Activation (what attach does after s.pending=false) re-derives the flags from the carried tier.
	applyTierFlags(ds.entity, ds.tier)
	if !hasFlag(ds.entity, flagHolylight) || !hasFlag(ds.entity, flagAdmin) {
		t.Fatal("activation must re-derive the reserved flags from the carried tier (elevation survives the hop)")
	}
}

// TestCrossShardBaselineTierCarry: a baseline (empty-tier) player carries no elevation and arrives with no
// reserved flags — the fail-closed default, and the common case that must not regress.
func TestCrossShardBaselineTierCarry(t *testing.T) {
	src := newDemoZone("midgaard", newProtoCache())
	dst := newDemoZone("midgaard", newProtoCache())
	s := &session{character: "Peon", stateVersion: 1}
	src.newPlayerEntity(s, "Peon") // no tier, no flags

	snap := buildSnapshot(s)
	if snap.GetTier() != "" {
		t.Fatalf("a baseline player must carry an empty tier, got %q", snap.GetTier())
	}
	reply := make(chan error, 1)
	dst.claimInboundArrival() // the claim the production resolver takes under s.mu; the handler releases one unconditionally (#413)
	dst.prepare(prepareMsg{snap: snap, room: "", epoch: 2, token: "tok", reply: reply})
	if err := <-reply; err != nil {
		t.Fatalf("prepare replied error: %v", err)
	}
	ds := dst.players["Peon"]
	applyTierFlags(ds.entity, ds.tier)
	for _, f := range []string{flagHolylight, flagBuilder, flagAdmin, flagWizinvis} {
		if hasFlag(ds.entity, f) {
			t.Errorf("a baseline player must arrive with no reserved flags, got %q", f)
		}
	}
}
