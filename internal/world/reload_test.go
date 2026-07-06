package world

import (
	"context"
	"sync"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// reload_test.go is slice 4.3's hot-reload coverage (docs/PHASE4-PLAN.md §5). It drives the whole
// path with an in-memory bus + in-memory content source — NO live NATS or Postgres — exactly as
// the durability-ladder tests drive MemStore. The load-bearing assertions:
//
//   - a published invalidation that changes a prototype makes the NEXT spawn reflect the edit;
//   - a live instance spawned BEFORE the reload is UNCHANGED (it keeps the old prototype, which
//     stays alive via GC) — the documented MUD semantics, not a bug;
//   - reloads running CONCURRENTLY with spawns on multiple zone goroutines are data-race-free
//     under -race (the proof the atomic cache-swap is correct).
//
// A gated real-NATS integration test (TestHotReloadOverRealNATS) skips unless TELOS_NATS_URL is set.

// reloadTestPack builds a tiny two-prototype pack: one room and one item, so a test can edit either
// and assert the reload. It is deliberately minimal (not the demo pack) so the assertions are about
// the reload mechanism, not demo content.
func reloadTestPack() content.Pack {
	return content.Pack{
		Pack: "reloadtest",
		Zones: []content.ZoneDTO{{
			Ref:       "rt",
			Name:      "Reload Test Zone",
			StartRoom: "rt:room:hall",
			Rooms: []content.RoomDTO{{
				Ref:   "rt:room:hall",
				Name:  "The Hall",
				Long:  "An old stone hall.",
				Exits: map[string]string{},
			}},
			Items: []content.ProtoDTO{{
				Ref:      "rt:obj:torch",
				Keywords: []string{"torch"},
				Short:    "a torch",
				Long:     "A torch lies here.",
			}},
		}},
	}
}

// newReloadShard builds a shard from src (boot-loaded) and wires hot reload over bus. It does NOT
// Run the shard: the reloader subscribes at WithHotReload time (the MemBus delivery goroutine
// starts at Subscribe), so an invalidation reloads without the zone loops running — which lets the
// test inspect the cache and call spawn directly, the same way the prototype tests do.
func newReloadShard(t *testing.T, src *content.MemSource, bus contentbus.Bus) *Shard {
	t.Helper()
	lc, err := content.Load(context.Background(), src, []string{"reloadtest"})
	if err != nil {
		t.Fatalf("boot load: %v", err)
	}
	s := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).
		WithHotReload(src, bus, []string{"reloadtest"})
	if s.reloader == nil {
		t.Fatal("hot reload not enabled (reloader nil)")
	}
	return s
}

// waitForProto polls the shard's cache until pred(proto) holds or the deadline passes. The reload
// is delivered on the bus's subscription goroutine, so the swap is observed asynchronously; this is
// the deterministic synchronization point (poll the atomic table the same way spawn reads it).
func waitForProto(t *testing.T, s *Shard, ref ProtoRef, pred func(*Prototype) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p := s.protos.get(ref); p != nil && pred(p) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for reload of %q", ref)
}

// TestHotReloadRoomLongDesc edits a room's long description, publishes an invalidation, and asserts
// the NEXT spawn renders the new long while a PRE-EXISTING live instance keeps the old long.
func TestHotReloadRoomLongDesc(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	z := s.Zone()

	// A live room instance spawned BEFORE the reload (a singleton, as a real zone holds).
	before := z.spawn("rt:room:hall")
	if got := before.Long(); got != "An old stone hall." {
		t.Fatalf("pre-reload long = %q", got)
	}

	// Edit the source and publish the invalidation (the writer/seed trigger).
	const newLong = "A torch-lit hall, freshly renovated."
	if err := src.EditRoomLong("reloadtest", "rt:room:hall", newLong); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), contentbus.Invalidation{
		Kind: content.KindRoom, Ref: "rt:room:hall", Pack: "reloadtest",
	}); err != nil {
		t.Fatal(err)
	}

	// The cache entry swaps asynchronously on the subscription goroutine.
	waitForProto(t, s, "rt:room:hall", func(p *Prototype) bool { return p.long == newLong })

	// A NEW spawn uses the new prototype...
	after := z.spawn("rt:room:hall")
	if got := after.Long(); got != newLong {
		t.Fatalf("post-reload spawn long = %q, want %q", got, newLong)
	}
	// ...and the PRE-EXISTING instance is unchanged (it still aliases the old prototype, GC-kept).
	if got := before.Long(); got != "An old stone hall." {
		t.Fatalf("pre-existing instance long changed to %q; live instances must NOT be retroactively reloaded", got)
	}
}

// TestHotReloadItemKeywords edits an item prototype's keywords (a targeting-relevant field) and
// asserts the next spawn carries the new keywords while a live instance keeps the old ones.
func TestHotReloadItemKeywords(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	z := s.Zone()

	before := z.spawn("rt:obj:torch")
	if !hasKeyword(before, "torch") || hasKeyword(before, "brand") {
		t.Fatalf("pre-reload keywords unexpected: %v", before.keywords)
	}

	if err := src.EditItemKeywords("reloadtest", "rt:obj:torch", []string{"torch", "brand"}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), contentbus.Invalidation{
		Kind: content.KindItem, Ref: "rt:obj:torch", Pack: "reloadtest",
	}); err != nil {
		t.Fatal(err)
	}
	waitForProto(t, s, "rt:obj:torch", func(p *Prototype) bool {
		for _, k := range p.keywords {
			if k == "brand" {
				return true
			}
		}
		return false
	})

	after := z.spawn("rt:obj:torch")
	if !hasKeyword(after, "brand") {
		t.Fatalf("post-reload spawn missing new keyword: %v", after.keywords)
	}
	if hasKeyword(before, "brand") {
		t.Fatal("pre-existing instance gained the new keyword; live instances must not be reloaded")
	}
}

// hasKeyword reports whether e's keyword list contains kw.
func hasKeyword(e *Entity, kw string) bool {
	for _, k := range e.keywords {
		if k == kw {
			return true
		}
	}
	return false
}

// TestHotReloadIgnoresForeignPack asserts an invalidation for a pack this shard does not load is a
// no-op (the prototype is untouched), so a multi-pack deploy doesn't cross-reload.
func TestHotReloadIgnoresForeignPack(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)

	if err := src.EditRoomLong("reloadtest", "rt:room:hall", "should not apply"); err != nil {
		t.Fatal(err)
	}
	// Publish under a DIFFERENT pack name: the shard filters it out before re-reading.
	if err := bus.Publish(context.Background(), contentbus.Invalidation{
		Kind: content.KindRoom, Ref: "rt:room:hall", Pack: "some-other-pack",
	}); err != nil {
		t.Fatal(err)
	}
	// Give the (filtered) delivery a beat; the long must remain the original.
	time.Sleep(50 * time.Millisecond)
	if got := s.protos.get("rt:room:hall").long; got != "An old stone hall." {
		t.Fatalf("foreign-pack invalidation reloaded the prototype: %q", got)
	}
}

// TestHotReloadConcurrentWithSpawns is the CONCURRENCY proof for the atomic cache-swap: it runs
// repeated reloads of one ref CONCURRENTLY with many spawns of that ref across MULTIPLE zone
// goroutines. Under -race this is the standing guard that the per-shard cache swap is data-race-
// free — spawn reads the table locklessly (atomic Load) while the applier swaps it (atomic Store),
// and a sibling instance must never see a half-applied map or a torn prototype. It interleaves for
// real: spawners spin in a tight loop while a publisher fires invalidations in parallel.
func TestHotReloadConcurrentWithSpawns(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)

	// Two zones sharing the one per-shard cache (the real flyweight-across-zones shape). Both
	// spawn the same ref concurrently while the cache is being swapped under them.
	zA := newZone("rtA")
	zA.protos = s.protos
	zB := newZone("rtB")
	zB.protos = s.protos

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// Spawner goroutines on two distinct zone goroutines, hammering get->spawn (the lock-free read
	// path) for the whole test window.
	spawnLoop := func(z *Zone) {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if e := z.spawn("rt:obj:torch"); e != nil {
				// Touch a shared-immutable field so a torn read would be caught.
				_ = e.Long()
				_ = len(e.keywords)
			}
		}
	}
	wg.Add(2)
	go spawnLoop(zA)
	go spawnLoop(zB)

	// Publisher: fire many invalidations, each editing the prototype, so the applier rebuilds and
	// swaps the table repeatedly while the spawners read it. Editing the source between publishes
	// makes every reload a genuinely new *Prototype (a fresh map, a fresh entry).
	const rounds = 200
	for i := 0; i < rounds; i++ {
		_ = src.EditItemKeywords("reloadtest", "rt:obj:torch", []string{"torch", "round"})
		if err := bus.Publish(context.Background(), contentbus.Invalidation{
			Kind: content.KindItem, Ref: "rt:obj:torch", Pack: "reloadtest",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Let the reloads drain and the spawners keep reading through them.
	time.Sleep(100 * time.Millisecond)
	cancel()
	wg.Wait()

	// Sanity: after all reloads the cache still serves a usable prototype.
	if s.protos.get("rt:obj:torch") == nil {
		t.Fatal("prototype lost after concurrent reloads")
	}
}

// TestHotReloadDeletedDefinition asserts a not-found re-read (the row was deleted) REMOVES the
// prototype, so a later spawn returns nil rather than serving a stale prototype.
func TestHotReloadDeletedDefinition(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	z := s.Zone()
	if z.spawn("rt:obj:torch") == nil {
		t.Fatal("torch should spawn before deletion")
	}

	// Remove the item from the source, then invalidate: the re-read finds nothing => entry removed.
	pack := reloadTestPack()
	pack.Zones[0].Items = nil
	src.SetPack(pack)
	if err := bus.Publish(context.Background(), contentbus.Invalidation{
		Kind: content.KindItem, Ref: "rt:obj:torch", Pack: "reloadtest",
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.protos.get("rt:obj:torch") != nil {
		time.Sleep(time.Millisecond)
	}
	if s.protos.get("rt:obj:torch") != nil {
		t.Fatal("deleted definition still in cache after invalidation")
	}
	if z.spawn("rt:obj:torch") != nil {
		t.Fatal("spawn of a deleted prototype should return nil")
	}
}

// TestBuslessShardHasNoReloader asserts the disabled-fallback: a shard with no bus (or no source)
// has a nil reloader and is byte-identical to a pre-4.3 shard.
func TestBuslessShardHasNoReloader(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	lc, _ := content.Load(context.Background(), src, []string{"reloadtest"})

	// No bus => disabled.
	s := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).
		WithHotReload(src, nil, []string{"reloadtest"})
	if s.reloader != nil {
		t.Fatal("nil bus should disable hot reload")
	}
	// No source => disabled.
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s2 := NewShardFromContent(lc, []string{"rt"}, "rt", "", nil, nil).
		WithHotReload(nil, bus, []string{"reloadtest"})
	if s2.reloader != nil {
		t.Fatal("nil source should disable hot reload")
	}
}

// TestHotReloadRoomSingletonResync proves the #191 fix: a ROOM is a singleton spawned once at boot and
// never re-spawned, so the applier's "next spawn uses the edit" path never reaches the live room a player
// stands in. Editing the room and reloading must update that singleton IN PLACE. (Contrast
// TestHotReloadRoomLongDesc, which pins the pre-existing NON-singleton instance's old semantics.)
func TestHotReloadRoomSingletonResync(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	z := s.Zone()

	room := z.rooms["rt:room:hall"] // the live singleton (what `look` renders)
	if room == nil {
		t.Fatal("zone singleton room not built")
	}
	if got := room.Long(); got != "An old stone hall." {
		t.Fatalf("boot long = %q", got)
	}

	const newLong = "A torch-lit hall, freshly renovated."
	if err := src.EditRoomLong("reloadtest", "rt:room:hall", newLong); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), contentbus.Invalidation{
		Kind: content.KindRoom, Ref: "rt:room:hall", Pack: "reloadtest",
	}); err != nil {
		t.Fatal(err)
	}
	// The prototype cache swaps asynchronously on the subscription goroutine.
	waitForProto(t, s, "rt:room:hall", func(p *Prototype) bool { return p.long == newLong })

	// The applier posts a reloadLuaMsg to the zone inbox; this harness doesn't run the loop, so drive the
	// handler directly (it runs on the zone goroutine in production).
	z.handle(reloadLuaMsg{kind: content.KindRoom, ref: "rt:room:hall"})

	if got := room.Long(); got != newLong {
		t.Fatalf("live singleton room NOT resynced: long = %q, want %q", got, newLong)
	}
}

// TestResyncRoomRepointsWholeComponent proves resyncRoom re-points the ENTIRE room from the new prototype
// — long, exits, and named flags (the whole *Room component), plus e.prototype (the COW-consistency fix
// from review) — not just the description. Deterministic: it drives the applier's two on-zone-goroutine
// steps (cache swap + resync) directly, no running loop.
func TestResyncRoomRepointsWholeComponent(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s := newReloadShard(t, src, bus)
	z := s.Zone()

	room := z.rooms["rt:room:hall"]
	if room == nil {
		t.Fatal("zone singleton room not built")
	}
	oldProto := room.prototype

	// Author a new version of the room: new long, a fresh exit, and a named flag.
	edited := reloadTestPack()
	for i := range edited.Zones[0].Rooms {
		if edited.Zones[0].Rooms[i].Ref == "rt:room:hall" {
			edited.Zones[0].Rooms[i].Long = "A renovated hall."
			edited.Zones[0].Rooms[i].Exits = map[string]string{"north": "rt:room:hall"}
			edited.Zones[0].Rooms[i].Flags = []string{"safe"}
		}
	}
	src.SetPack(edited)

	// The applier's two zone-goroutine steps: swap the prototype, then resync the live singleton.
	def, err := src.LoadDefinition(context.Background(), content.KindRoom, "rt:room:hall", "reloadtest")
	if err != nil || !def.Found {
		t.Fatalf("re-read edited room: err=%v found=%v", err, def.Found)
	}
	z.protos.reload(ProtoRef("rt:room:hall"), buildPrototype(def))
	z.resyncRoom(ProtoRef("rt:room:hall"))

	if got := room.Long(); got != "A renovated hall." {
		t.Fatalf("long not resynced: %q", got)
	}
	if got := room.room.exits["north"]; got != ProtoRef("rt:room:hall") {
		t.Fatalf("exits not resynced: %v", room.room.exits)
	}
	if !room.room.namedFlags["safe"] {
		t.Fatalf("named flags not resynced: %v", room.room.namedFlags)
	}
	if room.prototype == oldProto {
		t.Fatal("e.prototype still points at the OLD prototype (the COW-consistency footgun)")
	}
}

// TestReloadCommandResyncsLiveRoomEndToEnd is the full-path proof: a builder standing in a room edits the
// content source and runs the in-game `reload` command; the whole chain (command → republish → content
// bus → applier cache swap → reloadLuaMsg → resyncRoom) runs under a LIVE zone loop, and the live room the
// builder is standing in renders the new description on the next look. All observation is via the session
// output channel, so it is race-free under -race.
func TestReloadCommandResyncsLiveRoomEndToEnd(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s := newReloadShard(t, src, bus)
	z := s.Zone()

	// A builder in the hall. The verb has two gates: the dispatch MinRank gate resolves rank from the
	// SESSION tier (so set tierBuilder → rank >= rankStaff, else the verb is hidden as "Huh?"), and the
	// handler's capability gate reads flagBuilder off the ENTITY (production reconciles the tier into this
	// flag; the test sets it directly).
	builder := &session{character: "Builder", tier: tierBuilder, out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(builder, "Builder")
	z.players["Builder"] = builder
	Move(builder.entity, z.rooms["rt:room:hall"])
	setFlag(builder.entity, flagBuilder, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	const newLong = "A torch-lit hall, freshly renovated."
	if err := src.EditRoomLong("reloadtest", "rt:room:hall", newLong); err != nil {
		t.Fatal(err)
	}
	z.post(inputMsg{id: "Builder", line: "reload", seq: 1})

	// Poll: re-look until the singleton renders the reloaded long (the async chain has landed). Reads are
	// only through the out channel, never the live entity, so the running loop's writes never race the test.
	seq := uint64(2)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		z.post(inputMsg{id: "Builder", line: "look", seq: seq})
		seq++
		if drainContains(t, builder, newLong) {
			return // the live room the builder stands in reflects the reload — end to end
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("live room never rendered the reloaded long after `reload`")
}

// TestResyncRoomAddsNewRoom proves the #191 slice-2 ADD path: a room NEW to a zone's graph (present in the
// reloaded content but not yet in z.rooms) is spawned live when resync sees its swapped prototype — but
// ONLY for a room this zone owns (by the ref's zone prefix), since the invalidation fans out to every
// hosted zone. Deterministic: drives the applier's cache-swap + resync steps directly.
func TestResyncRoomAddsNewRoom(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s := newReloadShard(t, src, bus)
	z := s.Zone()

	// Author a new room in THIS zone (rt:).
	edited := reloadTestPack()
	edited.Zones[0].Rooms = append(edited.Zones[0].Rooms, content.RoomDTO{
		Ref: "rt:room:annex", Name: "The Annex", Long: "A freshly built annex.",
	})
	src.SetPack(edited)

	def, err := src.LoadDefinition(context.Background(), content.KindRoom, "rt:room:annex", "reloadtest")
	if err != nil || !def.Found {
		t.Fatalf("re-read new room: err=%v found=%v", err, def.Found)
	}
	z.protos.reload(ProtoRef("rt:room:annex"), buildPrototype(def))
	if z.rooms["rt:room:annex"] != nil {
		t.Fatal("precondition: annex must not exist before resync")
	}
	z.resyncRoom(ProtoRef("rt:room:annex"))
	if z.rooms["rt:room:annex"] == nil {
		t.Fatal("new same-zone room was not added on resync")
	}
	if got := z.rooms["rt:room:annex"].Long(); got != "A freshly built annex." {
		t.Fatalf("added room long = %q", got)
	}

	// A new room belonging to ANOTHER zone must be ignored by this zone (the fan-out reaches every zone).
	foreign := content.Definition{
		Kind: content.KindRoom, Ref: "other:room:x", Found: true,
		Room: content.RoomDTO{Ref: "other:room:x", Name: "Elsewhere", Long: "Not here."},
	}
	z.protos.reload(ProtoRef("other:room:x"), buildPrototype(foreign))
	z.resyncRoom(ProtoRef("other:room:x"))
	if z.rooms["other:room:x"] != nil {
		t.Fatal("a foreign-zone room was wrongly added to this zone")
	}
}

// TestReloadCommandAddsReachableRoomEndToEnd is the full-path proof of the ADD path: a builder edits the
// source to add a new room AND an exit into it, runs `reload`, and can then WALK into the new room — the
// whole chain under a live zone loop, observed only via the output channel (race-free).
func TestReloadCommandAddsReachableRoomEndToEnd(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s := newReloadShard(t, src, bus)
	z := s.Zone()

	builder := &session{character: "Builder", tier: tierBuilder, out: make(chan *playv1.ServerFrame, 512), epoch: 1}
	z.newPlayerEntity(builder, "Builder")
	z.players["Builder"] = builder
	Move(builder.entity, z.rooms["rt:room:hall"])
	setFlag(builder.entity, flagBuilder, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	// Edit the source: add the annex + a north exit from the hall into it.
	edited := reloadTestPack()
	edited.Zones[0].Rooms[0].Exits = map[string]string{"north": "rt:room:annex"}
	edited.Zones[0].Rooms = append(edited.Zones[0].Rooms, content.RoomDTO{
		Ref: "rt:room:annex", Name: "The Annex", Long: "A freshly built annex.",
	})
	src.SetPack(edited)

	z.post(inputMsg{id: "Builder", line: "reload", seq: 1})

	// Poll: once the reload lands, `north` reaches the new room and `look` renders it. Retrying north+look
	// tolerates the async landing (before it lands, north fails and the annex desc never appears).
	seq := uint64(2)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		z.post(inputMsg{id: "Builder", line: "north", seq: seq})
		z.post(inputMsg{id: "Builder", line: "look", seq: seq + 1})
		seq += 2
		if drainContains(t, builder, "A freshly built annex.") {
			return // the builder walked into the newly-added, reloaded room — end to end
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("builder never reached the newly-added room after `reload`")
}
