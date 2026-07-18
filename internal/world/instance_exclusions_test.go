package world

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// instance_exclusions_test.go — #411's INFRASTRUCTURE exclusions: the places that were written before
// instances existed and that go wrong the moment one is minted.
//
// Each of these is a real bug found in design review, not a nicety. They split into three families:
//
//   - SILENT INERTNESS — region reads miss, so content authored against the template works in playtest and
//     does nothing in every copy, with nothing anywhere to find it by.
//   - FAN-OUT — one world-scope object (a scheduled boss, its loot table, its shared timer) delivered into
//     every private copy at once.
//   - FAIL-CLOSED INGRESS — every door that takes a zone id from off-box or from a durable row must refuse
//     an instance-shaped one, or a poisoned/pre-migration record logs a player into somebody's live dungeon.

// --- region scope ---------------------------------------------------------------------------------------

// TestInstanceResolvesItsRegionByTemplate. region_defs list AUTHORED zone refs, so an instance's synthetic id
// can never appear in one. Resolving by id alone does not fail loudly, it fails SILENTLY: region:get reads
// empty inside every copy of a dungeon, so a war flag or world-event gate authored against the template is
// simply inert everywhere it matters.
func TestInstanceResolvesItsRegionByTemplate(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	s := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	s.WithScopeBus(scopebus.New(commbus.NewMemBus()), lc.Regions)

	inst := newInstanceZone("darkwood#aaaa", "darkwood")
	if got := s.scopes.regionFor(inst); got != "heartlands" {
		t.Fatalf("region for instance %q = %q, want heartlands (resolved via its template)", inst.id, got)
	}
	// The widening must be exactly one step: an instance of a REGION-LESS zone stays region-less.
	cryptInst := newInstanceZone("crypt#aaaa", "crypt")
	if got := s.scopes.regionFor(cryptInst); got != "" {
		t.Fatalf("region for an instance of the region-less crypt = %q, want none", got)
	}
	// And an authored zone is unaffected.
	if got := s.scopes.regionFor(s.zones["darkwood"]); got != "heartlands" {
		t.Fatalf("region for the authored darkwood = %q, want heartlands", got)
	}
}

// TestSignalUpRefusedFromAnInstance is the WRITE direction, which may never widen.
//
// The signal envelope carries no source (scopeSignalJob is scope+event+payload), so a director cannot tell
// one private party's report from the shared world's. The concrete case is the demo's own boss loop: the
// chief's death fires signal_world("boss.died"), which the director's scheduler intercepts to reschedule the
// SHARED world timer. A party farming their own copy would push the whole server's next scheduled spawn out,
// repeatedly, for free.
//
// Asserts BOTH halves: nothing is published from the instance, and the identical script from the AUTHORED
// zone still publishes (so this is about instancing, not about signalling being broken).
func TestSignalUpRefusedFromAnInstance(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	s := NewShardFromContent(lc, []string{"darkwood"}, "darkwood", "", nil, nil)
	js := commbus.NewMemJetStream()
	bus := scopebus.New(commbus.NewMemBus()).WithDurable(js, "world-test")
	s.WithScopeBus(bus, lc.Regions)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.scopes.signalLoop(ctx)

	events := make(chan scopebus.DurableEvent, 8)
	for _, sc := range []scopebus.Scope{scopebus.World(), scopebus.Region("heartlands")} {
		c, err := bus.SubscribeDurable(sc, "dir-"+sc.Kind, func(ev scopebus.DurableEvent) bool {
			events <- ev
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Stop() }()
	}

	const src = `
		signal_region("boss_slain", {by = "hero"})
		signal_world("boss.died", {ref = "boss:warden"})
	`

	// The INSTANCE: both calls must be refused, and neither may reach the bus.
	inst := newInstanceZone("darkwood#aaaa", "darkwood")
	inst.shard = s
	inst.scopes.regionID = "heartlands" // as regionFor would stamp it
	if err := inst.lua.runChunk("signal", src); err != nil {
		t.Fatalf("the refusal must be silent to the SCRIPT (never an error): %v", err)
	}
	select {
	case ev := <-events:
		t.Fatalf("a zone INSTANCE signalled %q up to its director; a director cannot attribute a private "+
			"copy's report, and boss.died reschedules the SHARED world timer", ev.Event)
	case <-time.After(150 * time.Millisecond):
	}

	// The CONTROL: the authored zone still signals.
	if err := s.zones["darkwood"].lua.runChunk("signal", src); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("the AUTHORED zone's signal never reached the director — the refusal is too broad")
	}
}

// --- director schedule fan-out --------------------------------------------------------------------------

// TestReservedScheduleEventWithheldFromInstances. A schedule is a WORLD-scope object with world-scope
// scarcity: one boss, one loot table, one timer. The broadcast fans out to every hosted zone, so one
// spawn.boss would spawn that boss in the template AND in every live private copy simultaneously — and each
// kill then reschedules the one shared timer, last-writer-wins.
//
// Deliberately narrow: only ENGINE-RESERVED events are withheld. A content-authored world event still
// reaches instances (the engine cannot know the author's intent), and state deltas always do — an instance
// READS shared state exactly like its template, which is what regionFor is for.
func TestReservedScheduleEventWithheldFromInstances(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	s := NewShardFromContent(lc, []string{"darkwood"}, "darkwood", "", nil, nil)
	bus := scopebus.New(commbus.NewMemBus())
	s.WithScopeBus(bus, lc.Regions)

	inst := newInstanceZone("darkwood#aaaa", "darkwood")
	s.adopt(inst.id, inst)
	s.scopes.registerZone(inst)
	template := s.zones["darkwood"]

	s.scopes.start()
	defer s.scopes.stop()
	ctx := context.Background()

	fire := func(scope scopebus.Scope, event string, payload []byte) {
		if err := bus.Signal(ctx, scope, event, payload, "world-director"); err != nil {
			t.Fatal(err)
		}
	}
	spawnPayload, _ := json.Marshal(map[string]string{"zone": "darkwood", "proto": "darkwood:mob:goblin-chief"})
	statePayload, _ := json.Marshal(scopebus.StatePayload{Key: "war", Value: json.RawMessage(`true`)})

	fire(scopebus.World(), "spawn.boss", spawnPayload)           // ENGINE-RESERVED: template only
	fire(scopebus.World(), "gate_opened", spawnPayload)          // content event: both
	fire(scopebus.World(), scopebus.EventStateSet, statePayload) // state delta: both

	tmplEvents, tmplDeltas := drainScopeMessages(t, template)
	instEvents, instDeltas := drainScopeMessages(t, inst)

	if !tmplEvents["spawn.boss"] {
		t.Fatal("the reserved schedule event did not reach the AUTHORED zone — the filter is too broad")
	}
	if instEvents["spawn.boss"] {
		t.Fatal("a reserved scheduled-spawn broadcast reached a zone INSTANCE: one schedule would spawn the " +
			"boss, with its full loot table, in the shared zone AND in every private copy")
	}
	if !tmplEvents["gate_opened"] || !instEvents["gate_opened"] {
		t.Fatalf("a CONTENT-authored world event must reach both (template=%v instance=%v); the engine cannot "+
			"know an author's intent, which is what mud.zone() is for",
			tmplEvents["gate_opened"], instEvents["gate_opened"])
	}
	if !tmplDeltas || !instDeltas {
		t.Fatalf("a world STATE delta must reach both (template=%v instance=%v): reads are the direction that "+
			"may widen", tmplDeltas, instDeltas)
	}
}

// drainScopeMessages reads a zone's inbox without running it, reporting which remote-effect events arrived
// and whether a state delta did.
func drainScopeMessages(t *testing.T, z *Zone) (events map[string]bool, delta bool) {
	t.Helper()
	events = map[string]bool{}
	for {
		select {
		case m := <-z.inbox:
			switch v := m.(type) {
			case scopeEventMsg:
				events[v.event] = true
			case scopeDeltaMsg:
				delta = true
			}
		case <-time.After(200 * time.Millisecond):
			return events, delta
		}
	}
}

// TestMudZoneReportsTheLiveZoneID is the content-side half of the fan-out fix. The demo's original idiom was
// `if ev.zone ~= "darkwood" then return end` — a comparison against the AUTHORED ref, which every instance of
// darkwood also matches. mud.zone() answers "which actor am I", so `ev.zone ~= mud.zone()` is the correct
// general filter: it is true in the template and false in every copy.
func TestMudZoneReportsTheLiveZoneID(t *testing.T) {
	for _, tc := range []struct{ id, template, want string }{
		{"darkwood", "darkwood", "darkwood"},
		{"darkwood#aaaa", "darkwood", "darkwood#aaaa"},
	} {
		z := newInstanceZone(tc.id, tc.template)
		if err := z.lua.runChunk("probe", `probed = mud.zone()`); err != nil {
			t.Fatalf("runChunk: %v", err)
		}
		got := z.lua.L.GetGlobal("probed").String()
		if got != tc.want {
			t.Fatalf("mud.zone() in zone %q = %q, want %q", tc.id, got, tc.want)
		}
	}
	// And the demo pack's own reactor must now use it — this file's whole point is that the pack stood as a
	// worked example of the bug.
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	zd := lc.Zone("darkwood")
	if zd == nil {
		t.Fatal("demo pack has no darkwood")
	}
	var herald string
	for _, m := range zd.Mobs {
		if m.Ref == "darkwood:mob:boss-herald" {
			herald = m.Lua
		}
	}
	if herald == "" {
		t.Fatal("the demo boss-herald reactor is gone; this guard needs re-pointing")
	}
	if strings.Contains(herald, `ev.zone ~= "darkwood"`) {
		t.Fatal("the demo boss reactor still compares against the AUTHORED ref — inside every instance of " +
			"darkwood that comparison matches, which is exactly the fan-out bug")
	}
	if !strings.Contains(herald, "mud.zone()") {
		t.Fatal("the demo boss reactor does not filter on mud.zone()")
	}
}

// --- fail-closed ingress --------------------------------------------------------------------------------

// TestHandoffIngressRefusesInstanceIDs. An instance is shard-local by construction — unleased, out of the
// placement pool, resolvable by no peer — so a cross-shard request naming one is a poisoned record, a
// pre-migration artifact, or a probe. Refusing by SHAPE makes the answer identical on every shard and stops a
// peer injecting a player into a live private instance whose id it observed.
func TestHandoffIngressRefusesInstanceIDs(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()
	sh.allowInsecureHandoff = true // keyless: isolate the SHAPE check from the #260 refusal
	h := &handoffServer{shard: sh}
	ctx := context.Background()

	_, err := h.Prepare(ctx, &handoffv1.PrepareRequest{
		TargetZoneId: "darkwood#deadbeef",
		TargetRoomId: "darkwood:room:grove",
		Snapshot:     &handoffv1.PlayerSnapshot{CharacterId: "Hero"},
		Epoch:        1,
	})
	if err == nil {
		t.Fatal("Handoff.Prepare accepted an instance-shaped target zone")
	}
	if !strings.Contains(err.Error(), "not a valid handoff destination") {
		t.Fatalf("unexpected Prepare error: %v", err)
	}

	_, err = h.AdoptZone(ctx, &handoffv1.AdoptZoneRequest{
		ZoneId: "darkwood#deadbeef", FromShardId: "shard-a", ToShardId: "shard-b",
	})
	if err == nil {
		t.Fatal("Handoff.AdoptZone accepted an instance-shaped zone id")
	}
	if !strings.Contains(err.Error(), "cannot be adopted") {
		t.Fatalf("unexpected AdoptZone error: %v", err)
	}
}

// TestDurableZoneRefRefusesAnInstance. No write path stores an instance-shaped durable location — a player
// inside an instance persists the exit ANCHOR (#72) — so such a row is poisoned or pre-migration. Honoring it
// would log a reconnecting player straight into a LIVE private instance they may never have entered, whose
// occupants are somebody else's party.
//
// The setup is the dangerous one on purpose: the instance really is hosted and really would resolve, so the
// shape check is the only thing between the poisoned row and the dungeon.
func TestDurableZoneRefRefusesAnInstance(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()
	ps := &playServer{shard: sh, log: sh.zones["midgaard"].log}

	inst := mustMint(t, sh, "darkwood", "acct-1")
	if sh.ZoneByID(inst.id) == nil {
		t.Fatal("the minted instance is not hosted; the test would pass vacuously")
	}

	got := ps.resolveAttachZone("Hero", "", CharSnapshot{ZoneRef: inst.id}, true)
	if got == inst {
		t.Fatal("a durable zone_ref naming a live INSTANCE attached the player straight into it")
	}
	if got != sh.ZoneByID("midgaard") {
		t.Fatalf("refused instance zone_ref routed to %q, want the home zone", got.id)
	}
	// The CONTROL: an ordinary hosted durable zone is still honored, so the refusal is not "durable zone_ref
	// is ignored".
	if got := ps.resolveAttachZone("Hero", "", CharSnapshot{ZoneRef: "darkwood"}, true); got != sh.ZoneByID("darkwood") {
		t.Fatalf("an ordinary durable zone_ref routed to %q, want darkwood", got.id)
	}
}

// TestDurableLocationGuardIsSymmetric is the WRITE side of the guard above, and the reason it exists is that
// the read side's justification — "no write path stores an instance-shaped ZoneRef" — is an ASSUMPTION with
// an expiry date. Slice 3 (#72) puts players inside instances, at which point dumpCharacter's
// `s.entity.zone.id` and registerPlacement's `z.id` both start writing exactly the shape the read side is
// there to refuse. A read guard whose only adversary is its own slice's write path is not a guard; this makes
// slice 3's exit anchor an enforcement rather than a convention.
//
// THERE ARE THREE WRITE SITES, and they do NOT all do the same thing — "both writers fail closed to no
// location" is the version of this claim that shipped a bug, so it is stated precisely here:
//
//   - dumpCharacter (the durable row + the Redis checkpoint) PRESERVES. It hands "" to the store, and the
//     sink reads "" as "leave zone_ref alone", so the entrance anchor already on the row survives. A
//     CLEARING write there is worse than useless: room_ref keeps the template's authored ref, so the row
//     becomes a real room with no zone and the reconnect start-rooms the player (or, if the shard's home
//     zone IS the template, materializes them inside the SHARED copy).
//   - registerPlacement SKIPS. It writes nothing at all, so the player's last good placement stands.
//   - clearPlacement STILL FIRES, with an empty zone. Dropping the tombstone would leave the record naming
//     a shard that is exiting; carrying the instance id would HSET an ephemeral zone into a record that
//     deliberately OUTLIVES the session (ClearPlayerShard preserves `zone` as the reconnect routing key),
//     and nothing ever overwrites it — ShardForZone finds no lease for a reaped instance and the gate does
//     not fall back to place.ShardID (#320).
func TestDurableLocationGuardIsSymmetric(t *testing.T) {
	// --- the durable character row -------------------------------------------------------------------------
	if got := durableZoneRef(newInstanceZone("darkwood#deadbeef", "darkwood")); got != "" {
		t.Fatalf("dumpCharacter would persist %q as a character's durable location. The row is dangling by "+
			"construction (the instance is reaped in minutes) AND it is a poisoned record aimed straight at the "+
			"read guard, which would otherwise log a reconnecting player into a live private dungeon", got)
	}
	// The CONTROL: an ordinary zone is still persisted, so the guard is not "we stopped saving locations".
	if got := durableZoneRef(newZone("darkwood")); got != "darkwood" {
		t.Fatalf("durableZoneRef(authored darkwood) = %q, want darkwood", got)
	}
	if got := durableZoneRef(nil); got != "" {
		t.Fatalf("durableZoneRef(nil) = %q, want empty", got)
	}

	// --- the directory placement record --------------------------------------------------------------------
	// The placement record is the reconnect-ROUTING spine since #320: the gate resolves a returning player by
	// asking ShardForZone for the recorded zone. An instance is unleased and in no directory at all, so a
	// record naming one resolves to no shard and dead-ends the reconnect.
	sh := NewShardFromContent(nil, nil, "midgaard", "", newFakeLocator(), nil)
	sh.shardID = "shard-a"
	sess := &session{character: "Hero", epoch: 1}

	inst := newInstanceZone("darkwood#deadbeef", "darkwood")
	inst.shard = sh
	inst.registerPlacement(sess)
	if ops, _ := sh.placement.take(); len(ops) != 0 {
		t.Fatalf("registerPlacement enqueued %d op(s) for a player inside an INSTANCE: the recorded zone is "+
			"unleased and resolves to no shard, so the reconnect dead-ends instead of falling back to the "+
			"player's last good placement", len(ops))
	}
	// The CONTROL: an ordinary zone still records, so the refusal is not "placement stopped working".
	plain := newZone("darkwood")
	plain.shard = sh
	plain.registerPlacement(sess)
	if ops, _ := sh.placement.take(); len(ops) == 0 {
		t.Fatal("registerPlacement recorded nothing for an ORDINARY zone either — the refusal is too broad")
	}

	// --- the clean-logout tombstone ------------------------------------------------------------------------
	// The one that OUTLIVES both the session and the instance. ClearPlayerShard deliberately preserves the
	// `zone` field across the tombstone because it is the reconnect routing key, so a quit from inside an
	// instance HSETs an ephemeral id into a durable record and the instance is reaped seconds later. Nothing
	// self-heals it: no later write happens until the player's next login, and until then the gate resolves
	// them through a zone with no lease.
	inst.clearPlacement(sess)
	ops, _ := sh.placement.take()
	if len(ops) != 1 {
		t.Fatalf("clearPlacement enqueued %d op(s) from inside an instance, want 1: the tombstone must still "+
			"FIRE — dropping it leaves the placement record claiming an exiting shard still hosts the player",
			len(ops))
	}
	if !ops[0].clear {
		t.Fatalf("clearPlacement enqueued a REGISTRATION (%+v), not a tombstone", ops[0])
	}
	if ops[0].zoneID != "" {
		t.Fatalf("clearPlacement carried zone %q into the tombstone. ClearPlayerShard HSETs a non-empty zone "+
			"and preserves it across the tombstone as the reconnect routing key, so this records an EPHEMERAL "+
			"id in a durable record; the instance is reaped seconds later, ShardForZone then finds no lease, "+
			"and per #320 the gate will NOT fall back to place.ShardID — the player is routed by home zone "+
			"until some future registerPlacement happens to overwrite it. An empty zone is the fix: the Lua "+
			"script reads it as 'leave the stored zone untouched'", ops[0].zoneID)
	}
	// The CONTROL: quitting an ORDINARY zone still carries that zone, or a logout that coalesced over a
	// pending zone-change registration would leave the record naming the zone the player walked out of.
	plain.clearPlacement(sess)
	ops, _ = sh.placement.take()
	if len(ops) != 1 || !ops[0].clear || ops[0].zoneID != "darkwood" {
		t.Fatalf("clearPlacement from an ORDINARY zone enqueued %+v, want one tombstone carrying darkwood — "+
			"the instance guard is too broad", ops)
	}
}

// TestSavingFromInsideAnInstancePreservesTheDurableAnchor is the PRODUCER-to-SINK half of the durable-row
// guard, and the reason durableZoneRef's "" cannot mean "fail closed to no location" (#411).
//
// "" reaching the sink as a CLEARING write is strictly worse than storing the instance id would have been:
// RoomRef is not blanked (an instance hosts its TEMPLATE's authored rooms, so the snapshot carries a real
// ref like darkwood:room:grove), so the row becomes a real room with no zone. resolveAttachZone's
// `ZoneRef != ""` guard is then false, it falls back to the home zone, resolveRoom cannot find that room
// there, and the player lands at the start room. Every save-cadence tick does it, and so does the drain's own
// flush of every instance occupant on SIGTERM.
//
// The contract is therefore "" == LEAVE IT ALONE, implemented at the sink in all three tiers (COALESCE in
// internal/store, the read-modify-write in internal/checkpoint, and MemStore here). That gives the durable
// row the same entrance-anchor preservation the placement record gets for free.
func TestSavingFromInsideAnInstancePreservesTheDurableAnchor(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()
	ctx := context.Background()

	mem := NewMemStore()
	// The entrance anchor already on the row: where the player was before they stepped into the instance.
	if _, err := mem.CreateCharacter(ctx, "Hero", "midgaard", "midgaard:room:temple"); err != nil {
		t.Fatal(err)
	}

	// A session standing inside a live instance, dumped exactly as the saver dumps it.
	inst := mustMint(t, sh, "darkwood", "acct-1")
	s := &session{character: "Hero"}
	inst.newPlayerEntity(s, "Hero")
	Move(s.entity, inst.rooms["darkwood:room:grove"])
	snap := dumpCharacter(s)
	if snap.ZoneRef != "" {
		t.Fatalf("dumpCharacter persisted %q for a player inside an instance; the rest of this test assumes the "+
			"producer refuses the ephemeral id", snap.ZoneRef)
	}
	if snap.RoomRef == "" {
		t.Fatal("the snapshot carries no room either, so this test cannot show the inconsistent row")
	}

	if _, ok, err := mem.SaveCharacter(ctx, snap); err != nil || !ok {
		t.Fatalf("SaveCharacter: ok=%v err=%v", ok, err)
	}
	if err := mem.Checkpoint(ctx, snap); err != nil {
		t.Fatal(err)
	}

	row, _, err := mem.LoadCharacter(ctx, "Hero")
	if err != nil {
		t.Fatal(err)
	}
	if row.ZoneRef != "midgaard" {
		t.Fatalf("the durable row's zone_ref is now %q (was midgaard) while room_ref kept %q: a real room with "+
			"no zone. The reconnect falls back to home, cannot resolve that room there, and start-rooms the "+
			"player — permanent location loss for every instance occupant on every save tick and every SIGTERM",
			row.ZoneRef, row.RoomRef)
	}
	ck, found, err := mem.LoadCheckpoint(ctx, "Hero")
	if err != nil || !found {
		t.Fatalf("LoadCheckpoint: found=%v err=%v", found, err)
	}
	if ck.ZoneRef != "" && ck.ZoneRef != "midgaard" {
		t.Fatalf("checkpoint zone_ref = %q, want the preserved anchor", ck.ZoneRef)
	}

	// The CONTROL: an ordinary zone still WRITES its id, so the sink's preserve is not "locations stopped
	// being saved".
	s2 := &session{character: "Hero"}
	dw := sh.ZoneByID("darkwood")
	dw.newPlayerEntity(s2, "Hero")
	Move(s2.entity, dw.rooms["darkwood:room:grove"])
	moved := dumpCharacter(s2)
	moved.StateVersion = row.StateVersion
	if _, ok, err := mem.SaveCharacter(ctx, moved); err != nil || !ok {
		t.Fatalf("SaveCharacter (ordinary zone): ok=%v err=%v", ok, err)
	}
	if again, _, _ := mem.LoadCharacter(ctx, "Hero"); again.ZoneRef != "darkwood" {
		t.Fatalf("an ordinary zone change was not persisted: zone_ref = %q, want darkwood", again.ZoneRef)
	}
}
