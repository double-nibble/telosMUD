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
// an expiry date. #72 puts players inside instances, at which point dumpCharacter's `s.entity.zone.id` and
// registerPlacement's `z.id` would both start writing exactly the shape the read side is there to refuse. A
// read guard whose only adversary is its own slice's write path is not a guard.
//
// #411 made all three write sites decline to record the instance. #72 makes them record the EXIT ANCHOR — the
// zone and room the player entered from — which is what this asserts. The distinction is not cosmetic: #411
// could only PRESERVE (leave whatever was on the row alone), which happened to be the entrance zone by luck
// of the last walk and left room_ref naming a room in a different zone; the anchor is written positively,
// as a pair, and stays current as the player walks around inside the instance.
//
// THE THREE WRITE SITES DO NOT ALL BEHAVE THE SAME, and "they all fail closed to no location" is the version
// of this claim that shipped a bug, so each is stated precisely:
//
//   - dumpCharacter (the durable row + the Redis checkpoint) writes the anchor ZONE AND ROOM together.
//   - registerPlacement writes the anchor zone.
//   - clearPlacement STILL FIRES and carries the anchor zone. Dropping the tombstone would leave the record
//     naming a shard that is exiting; carrying the INSTANCE id would HSET an ephemeral zone into a record
//     that deliberately OUTLIVES the session (ClearPlayerShard preserves `zone` as the reconnect routing
//     key), and nothing would ever overwrite it.
//
// The anchorless sub-test is the FAIL-SAFE, not a supported state: entry sets the anchor before it releases
// the session. It is pinned because the two placement scripts disagree about what an empty zone means —
// `registerPlacement` HSETs the field unconditionally (so an empty ref CLOBBERS the stored zone), while
// `clearPlayerShard` guards on ARGV[2] ~= ” (so an empty ref preserves) — which is why one site skips and the
// other writes empty.
func TestDurableLocationGuardIsSymmetric(t *testing.T) {
	const (
		instZone   = "darkwood#deadbeef"
		anchorZone = "midgaard"
		anchorRoom = ProtoRef("midgaard:room:market")
	)

	// --- the durable character row -------------------------------------------------------------------------
	// durableLocation is the single producer of the pair. Both halves must come from the anchor: an anchor
	// zone paired with an INSTANCE room is the internally inconsistent row #411 could not avoid, and it
	// start-rooms the player on reconnect.
	inst := newInstanceZone(instZone, "darkwood")
	instRoom := inst.newEntity("darkwood:room:lair")
	Add(instRoom, &Room{})
	anchored := &session{character: "Hero", entity: inst.newEntity("Hero"), anchorZone: anchorZone, anchorRoom: anchorRoom}
	anchored.entity.zone = inst
	Move(anchored.entity, instRoom)
	if z, r := durableLocation(anchored); z != anchorZone || r != string(anchorRoom) {
		t.Fatalf("durableLocation inside an instance = (%q, %q), want the exit anchor (%q, %q). Persisting the "+
			"instance id dangles by construction (it is reaped in minutes) and aims a poisoned record straight "+
			"at the read guard; persisting the anchor ZONE with the INSTANCE room is the #411 shape that "+
			"start-rooms the player", z, r, anchorZone, anchorRoom)
	}
	// The CONTROL: an ordinary zone still persists its own live location, so this is not "we stopped saving".
	plain := newZone("darkwood")
	plainRoom := plain.newEntity("darkwood:room:grove")
	Add(plainRoom, &Room{})
	ordinary := &session{character: "Hero", entity: plain.newEntity("Hero")}
	ordinary.entity.zone = plain
	Move(ordinary.entity, plainRoom)
	if z, r := durableLocation(ordinary); z != "darkwood" || r != "darkwood:room:grove" {
		t.Fatalf("durableLocation in an ORDINARY zone = (%q, %q), want (darkwood, darkwood:room:grove)", z, r)
	}
	// The FAIL-SAFE: an instance occupant with no anchor preserves rather than persisting the ephemeral id.
	anchorless := &session{character: "Hero", entity: inst.newEntity("Ghost")}
	anchorless.entity.zone = inst
	Move(anchorless.entity, instRoom)
	if z, _ := durableLocation(anchorless); z != "" {
		t.Fatalf("durableLocation for an ANCHORLESS instance occupant = %q, want \"\" (preserve at the sink); "+
			"persisting an instance id is what the read guard exists to refuse", z)
	}

	// --- the directory placement record --------------------------------------------------------------------
	// The placement record is the reconnect-ROUTING spine since #320: the gate resolves a returning player by
	// asking ShardForZone for the recorded zone. An instance is unleased and in no directory at all, so a
	// record naming one resolves to no shard and dead-ends the reconnect. The anchor is an AUTHORED zone this
	// shard hosts, so it keeps the record's invariant ("the recorded zone is the zone that holds the session")
	// honest rather than bending it.
	sh := NewShardFromContent(nil, nil, "midgaard", "", newFakeLocator(), nil)
	sh.shardID = "shard-a"
	inst.shard = sh
	plain.shard = sh
	sess := &session{character: "Hero", epoch: 1, anchorZone: anchorZone, anchorRoom: anchorRoom}

	inst.registerPlacement(sess)
	ops, _ := sh.placement.take()
	if len(ops) != 1 || ops[0].zoneID != anchorZone {
		t.Fatalf("registerPlacement from inside an instance enqueued %+v, want one op naming the anchor %q. "+
			"An instance id resolves to no shard (the reconnect dead-ends); the anchor resolves to THIS shard, "+
			"which is the one holding the live instance", ops, anchorZone)
	}
	// The FAIL-SAFE, and the asymmetry with the tombstone below: with no anchor this site must write NOTHING.
	// The registerPlacement Lua HSETs `zone` unconditionally, so offering an empty ref would clobber the stored
	// anchor with "" — destroying exactly what the preserve is trying to keep.
	inst.registerPlacement(&session{character: "Hero", epoch: 1})
	if ops, _ := sh.placement.take(); len(ops) != 0 {
		t.Fatalf("registerPlacement enqueued %d op(s) for an ANCHORLESS instance occupant. The Lua HSETs the "+
			"zone field unconditionally, so this CLOBBERS the player's last good placement with an empty "+
			"string rather than preserving it", len(ops))
	}
	// The CONTROL: an ordinary zone still records itself.
	plain.registerPlacement(sess)
	if ops, _ := sh.placement.take(); len(ops) != 1 || ops[0].zoneID != "darkwood" {
		t.Fatalf("registerPlacement from an ORDINARY zone enqueued %+v, want one op naming darkwood — the "+
			"instance branch is too broad", ops)
	}

	// --- the clean-logout tombstone ------------------------------------------------------------------------
	// The one that OUTLIVES both the session and the instance. ClearPlayerShard deliberately preserves the
	// `zone` field across the tombstone because it is the reconnect routing key, so a quit from inside an
	// instance would HSET an ephemeral id into a durable record — and the instance is reaped seconds later.
	// Nothing self-heals it: no later write happens until the player's next login, and until then the gate
	// resolves them through a zone with no lease. The anchor is a durable authored ref, so it is safe to
	// outlive the instance in a way the instance id never was.
	inst.clearPlacement(sess)
	ops, _ = sh.placement.take()
	switch {
	case len(ops) != 1:
		t.Fatalf("clearPlacement enqueued %d op(s) from inside an instance, want 1: the tombstone must still "+
			"FIRE — dropping it leaves the placement record claiming an exiting shard still hosts the player",
			len(ops))
	case !ops[0].clear:
		t.Fatalf("clearPlacement enqueued a REGISTRATION (%+v), not a tombstone", ops[0])
	case ops[0].zoneID != anchorZone:
		t.Fatalf("clearPlacement carried zone %q into the tombstone, want the anchor %q. An instance id here "+
			"is the one write on this path that outlives both the session and the instance: it is reaped "+
			"seconds later, ShardForZone then finds no lease, and per #320 the gate will NOT fall back to "+
			"place.ShardID — the player routes by home zone until some future registerPlacement overwrites it",
			ops[0].zoneID, anchorZone)
	}
	// The FAIL-SAFE here goes the OTHER way from registerPlacement's: an empty zone is exactly "clear the
	// shard, leave the stored zone alone" (clearPlayerShard guards the field on ARGV[2] ~= ''), so the
	// tombstone fires and preserves.
	inst.clearPlacement(&session{character: "Hero", epoch: 1})
	ops, _ = sh.placement.take()
	if len(ops) != 1 || !ops[0].clear || ops[0].zoneID != "" {
		t.Fatalf("clearPlacement for an ANCHORLESS instance occupant enqueued %+v, want one tombstone with an "+
			"EMPTY zone (fire, and leave the stored zone untouched)", ops)
	}
	// The CONTROL: quitting an ORDINARY zone still carries that zone, or a logout that coalesced over a
	// pending zone-change registration would leave the record naming the zone the player walked out of.
	plain.clearPlacement(sess)
	ops, _ = sh.placement.take()
	if len(ops) != 1 || !ops[0].clear || ops[0].zoneID != "darkwood" {
		t.Fatalf("clearPlacement from an ORDINARY zone enqueued %+v, want one tombstone carrying darkwood — "+
			"the instance branch is too broad", ops)
	}
}

// TestSavingFromInsideAnInstancePreservesTheDurableAnchor is the PRODUCER-to-SINK half of the durable-row
// guard, and the reason durableZoneRef's "" cannot mean "fail closed to no location" (#411).
//
// SINCE #72 THIS IS THE FAIL-SAFE PATH, not the normal one. An instance occupant now carries an exit ANCHOR
// and durableLocation writes it positively as a (zone, room) pair, so the "" preserve is reached only by an
// anchorless occupant — which entry makes impossible. It is still pinned, and pinned here rather than merged
// into the anchor tests, because the SINK contract it describes ("" == leave the stored zone alone, in all
// three tiers) is what keeps that unreachable case from corrupting a row instead of merely degrading it. See
// TestDurableLocationInsideAnInstanceIsTheAnchor for the path a real player takes.
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

	if res, err := mem.SaveCharacter(ctx, snap); err != nil || res.Outcome != SaveApplied {
		t.Fatalf("SaveCharacter: outcome=%v err=%v", res.Outcome, err)
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
	if res, err := mem.SaveCharacter(ctx, moved); err != nil || res.Outcome != SaveApplied {
		t.Fatalf("SaveCharacter (ordinary zone): outcome=%v err=%v", res.Outcome, err)
	}
	if again, _, _ := mem.LoadCharacter(ctx, "Hero"); again.ZoneRef != "darkwood" {
		t.Fatalf("an ordinary zone change was not persisted: zone_ref = %q, want darkwood", again.ZoneRef)
	}
}
