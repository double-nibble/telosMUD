package world

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// Tests for the Phase-3 milestone (container.go): get / drop / put / wear / wield / hold /
// remove / inventory / equipment with correct scopes and act() perspective messages, plus
// open/close on a prototype-spawned container exercising the slice-3 copy-on-write path, and
// the concurrent-COW race guard the architect asked for (Finding 6).
//
// These drive the parser end to end against zone-owned data WITHOUT the zone goroutine
// running: dispatch is called directly (it is the same code path inputMsg takes), so each
// assertion reads the actor's and an observer's out channel synchronously. That keeps the
// act() actor-vs-observer split deterministic.

// cmdEnv is a player, a bystander, and the room they share, wired for direct dispatch.
type cmdEnv struct {
	z        *Zone
	room     *Entity
	actor    *session
	observer *session
}

func newCmdEnv(t *testing.T) *cmdEnv {
	t.Helper()
	z := NewDemoShard().Zone()
	// Use the temple (start room): it has NO demo ground items, so the only items in scope
	// are the ones each test adds — keeping target resolution and counts explicit.
	room := z.rooms["midgaard:room:temple"]
	actor := newTestPlayerEntity(z, "Alice")
	observer := newTestPlayerEntity(z, "Bob")
	actor.out = make(chan *playv1.ServerFrame, 64)
	observer.out = make(chan *playv1.ServerFrame, 64)
	z.players["Alice"] = actor
	z.players["Bob"] = observer
	Move(actor.entity, room)
	Move(observer.entity, room)
	return &cmdEnv{z: z, room: room, actor: actor, observer: observer}
}

// run dispatches a line for the actor and returns the actor's and observer's new outputs.
func (e *cmdEnv) run(line string) (actorOut, obsOut []string) {
	drain(e.actor)
	drain(e.observer)
	e.z.dispatch(e.actor, line)
	return drainOutputs(e.actor), drainOutputs(e.observer)
}

// has reports whether any line in out contains substr.
func has(out []string, substr string) bool {
	for _, s := range out {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// addTestItem spawns a bare item into dest with keywords/short and optional components.
func addTestItem(z *Zone, dest *Entity, short string, keywords []string, comps ...Component) *Entity {
	e := z.newEntity(ProtoRef("test:obj:" + short))
	e.short = short
	e.keywords = keywords
	for _, c := range comps {
		addAny(e, c)
	}
	Move(e, dest)
	return e
}

// addAny installs a component by its dynamic type (test convenience over the generic Add).
func addAny(e *Entity, c Component) {
	if e.comps == nil {
		e.comps = componentSet{}
	}
	e.comps[reflect.TypeOf(c)] = c
}

func TestGetAndDrop(t *testing.T) {
	e := newCmdEnv(t)
	sword := addTestItem(e.z, e.room, "a long sword", []string{"long", "sword"})

	// get: item moves from floor to inventory; actor and observer see the right lines.
	aout, oout := e.run("get sword")
	if sword.location != e.actor.entity {
		t.Fatalf("sword not in inventory after get: %v", sword.location)
	}
	if !has(aout, "You get a long sword.") {
		t.Errorf("actor get output = %v", aout)
	}
	if !has(oout, "Alice gets a long sword.") {
		t.Errorf("observer get output = %v", oout)
	}

	// inventory lists it.
	inv, _ := e.run("inventory")
	if !has(inv, "a long sword") {
		t.Errorf("inventory missing sword: %v", inv)
	}

	// drop: back to the floor.
	aout, oout = e.run("drop sword")
	if sword.location != e.room {
		t.Fatalf("sword not on floor after drop: %v", sword.location)
	}
	if !has(aout, "You drop a long sword.") {
		t.Errorf("actor drop output = %v", aout)
	}
	if !has(oout, "Alice drops a long sword.") {
		t.Errorf("observer drop output = %v", oout)
	}
}

func TestGetNothingThere(t *testing.T) {
	e := newCmdEnv(t)
	aout, _ := e.run("get dragon")
	if !has(aout, "You don't see that here.") {
		t.Errorf("get missing item output = %v", aout)
	}
}

func TestWearAndRemove(t *testing.T) {
	e := newCmdEnv(t)
	helm := addTestItem(e.z, e.actor.entity, "an iron helmet", []string{"helmet", "iron"},
		wearableFor(WearLocHead))

	aout, oout := e.run("wear helmet")
	if !has(aout, "You wear an iron helmet on your head.") {
		t.Errorf("actor wear output = %v", aout)
	}
	if !has(oout, "Alice wears an iron helmet.") {
		t.Errorf("observer wear output = %v", oout)
	}
	wr, ok := Get[*Wearer](e.actor.entity)
	if !ok || wr.worn[WearLocHead] != helm {
		t.Fatalf("helmet not in head slot after wear")
	}

	// equipment lists it under the head slot.
	eq, _ := e.run("equipment")
	if !has(eq, "head") || !has(eq, "an iron helmet") {
		t.Errorf("equipment output = %v", eq)
	}
	// inventory no longer lists the worn item (it's shown by equipment).
	inv, _ := e.run("inventory")
	if has(inv, "an iron helmet") {
		t.Errorf("worn item still in inventory listing: %v", inv)
	}

	// remove returns it to inventory.
	aout, oout = e.run("remove helmet")
	if !has(aout, "You stop using an iron helmet.") {
		t.Errorf("actor remove output = %v", aout)
	}
	if !has(oout, "Alice stops using an iron helmet.") {
		t.Errorf("observer remove output = %v", oout)
	}
	if wr.slotOf(helm) != WearLocNone {
		t.Fatalf("helmet still worn after remove")
	}
	inv, _ = e.run("inventory")
	if !has(inv, "an iron helmet") {
		t.Errorf("removed item not back in inventory: %v", inv)
	}
}

func TestWieldWeapon(t *testing.T) {
	e := newCmdEnv(t)
	sword := addTestItem(e.z, e.actor.entity, "a steel sword", []string{"sword", "steel"},
		wearableFor(WearLocWield), &Weapon{diceNum: 2, diceSize: 6, damageType: "slash"})

	aout, oout := e.run("wield sword")
	if !has(aout, "You wield a steel sword.") {
		t.Errorf("actor wield output = %v", aout)
	}
	if !has(oout, "Alice wields a steel sword.") {
		t.Errorf("observer wield output = %v", oout)
	}
	wr, _ := Get[*Wearer](e.actor.entity)
	if wr.worn[WearLocWield] != sword {
		t.Fatalf("sword not wielded")
	}

	// A non-weapon (no wield slot) is rejected.
	addTestItem(e.z, e.actor.entity, "a flower", []string{"flower"})
	aout, _ = e.run("wield flower")
	if !has(aout, "You can't wield a flower.") {
		t.Errorf("wield non-weapon output = %v", aout)
	}
}

func TestPutAndGetFromContainer(t *testing.T) {
	e := newCmdEnv(t)
	// An OPEN bag in inventory (not prototype-backed, so no COW needed for these moves).
	bag := addTestItem(e.z, e.actor.entity, "a leather bag", []string{"bag", "leather"},
		&Container{capacity: 5, closed: false})
	coin := addTestItem(e.z, e.actor.entity, "a gold coin", []string{"gold", "coin"})

	aout, oout := e.run("put coin in bag")
	if coin.location != bag {
		t.Fatalf("coin not in bag after put: %v", coin.location)
	}
	if !has(aout, "You put a gold coin in a leather bag.") {
		t.Errorf("actor put output = %v", aout)
	}
	if !has(oout, "Alice puts a gold coin in a leather bag.") {
		t.Errorf("observer put output = %v", oout)
	}

	// get from container.
	aout, oout = e.run("get coin from bag")
	if coin.location != e.actor.entity {
		t.Fatalf("coin not back in inventory after get-from: %v", coin.location)
	}
	if !has(aout, "You get a gold coin from a leather bag.") {
		t.Errorf("actor get-from output = %v", aout)
	}
	if !has(oout, "Alice gets a gold coin from a leather bag.") {
		t.Errorf("observer get-from output = %v", oout)
	}
}

func TestClosedContainerRejects(t *testing.T) {
	e := newCmdEnv(t)
	bag := addTestItem(e.z, e.actor.entity, "a leather bag", []string{"bag"},
		&Container{capacity: 5, closed: true})
	addTestItem(e.z, e.actor.entity, "a gold coin", []string{"coin"})

	aout, _ := e.run("put coin in bag")
	if !has(aout, "a leather bag is closed.") {
		t.Errorf("put into closed container = %v", aout)
	}
	// nothing entered the bag.
	if len(bag.contents) != 0 {
		t.Fatalf("closed container accepted an item: %v", bag.contents)
	}
}

func TestContainerCapacity(t *testing.T) {
	e := newCmdEnv(t)
	bag := addTestItem(e.z, e.actor.entity, "a tiny pouch", []string{"pouch"},
		&Container{capacity: 1, closed: false})
	addTestItem(e.z, e.actor.entity, "coin one", []string{"one", "coin"})
	addTestItem(e.z, e.actor.entity, "coin two", []string{"two", "coin"})

	e.run("put one in pouch")
	aout, _ := e.run("put two in pouch")
	if !has(aout, "a tiny pouch can't hold any more.") {
		t.Errorf("over-capacity put = %v", aout)
	}
	if len(bag.contents) != 1 {
		t.Fatalf("pouch over capacity: %d items", len(bag.contents))
	}
}

// TestOpenCloseCOW is the COW-arming test (Finding 6): open/close on a prototype-spawned
// chest must mutate the INSTANCE's Container, never the shared prototype or a sibling.
func TestOpenCloseCOW(t *testing.T) {
	z := NewDemoShard().Zone()
	room := z.rooms["midgaard:room:temple"] // no demo ground items to collide with
	actor := newTestPlayerEntity(z, "Alice")
	actor.out = make(chan *playv1.ServerFrame, 64)
	z.players["Alice"] = actor
	Move(actor.entity, room)

	// Two chest instances over the one prototype; both share the Container component pointer.
	chestA := z.spawn("midgaard:obj:chest")
	chestB := z.spawn("midgaard:obj:chest")
	Move(chestA, room)
	Move(chestB, room)
	proto := z.protos.get("midgaard:obj:chest")
	protoC := proto.comps[reflect.TypeFor[*Container]()].(*Container)

	// Sanity: chest starts closed and shares the prototype's Container pointer.
	cA, _ := Get[*Container](chestA)
	if any(cA) != any(protoC) {
		t.Fatal("chestA Container not shared with prototype at spawn")
	}
	if !protoC.closed {
		t.Fatal("chest prototype should start closed")
	}

	// Open chestA. This MUST COW: chestA gets its own Container, prototype + chestB untouched.
	drain(actor)
	z.dispatch(actor, "open chest")
	out := drainOutputs(actor)
	if !has(out, "You open a wooden chest.") {
		t.Errorf("open output = %v", out)
	}
	cAafter, _ := Get[*Container](chestA)
	if any(cAafter) == any(protoC) {
		t.Fatal("open did not COW: chestA still aliases the prototype Container")
	}
	if cAafter.closed {
		t.Fatal("chestA not open after open command")
	}
	// The prototype and the sibling are NOT mutated.
	if protoC.closed != true {
		t.Fatal("prototype Container.closed flipped by instance open (write-through!)")
	}
	cB, _ := Get[*Container](chestB)
	if any(cB) != any(protoC) {
		t.Fatal("sibling chestB Container stopped sharing without being touched")
	}
	if cB.closed != true {
		t.Fatal("sibling chestB opened by chestA's open (aliased mutation!)")
	}

	// Closing chestA again uses the already-owned Container (no second COW needed).
	drain(actor)
	z.dispatch(actor, "close chest")
	if !cAafter.closed {
		t.Fatal("chestA not closed after close command")
	}
	// chestA's Container pointer is unchanged by the second mutation (it was already owned).
	cAfinal, _ := Get[*Container](chestA)
	if any(cAfinal) != any(cAafter) {
		t.Fatal("close re-COW'd an already-owned Container")
	}
}

// TestConcurrentOpenCloseCOWRace drives the real open/close COW on ONE prototype from TWO
// zones on TWO goroutines in a tight loop and asserts the prototype is never mutated. Run
// under -race, this is the guard that proves no write-through-to-shared-prototype regression
// (Finding 6): two zone goroutines each own a sibling instance of the same prototype; if
// open/close wrote through the shared Container, the race detector would fire and/or the
// prototype's closed flag would flip.
func TestConcurrentOpenCloseCOWRace(t *testing.T) {
	// One shared prototype cache across two zones (mirrors a real shard).
	protos := newProtoCache()
	defineChest(protos)
	proto := protos.get("midgaard:obj:chest")
	protoC := proto.comps[reflect.TypeFor[*Container]()].(*Container)

	newZ := func(id string) (*Zone, *session, *Entity) {
		z := newZone(id)
		z.protos = protos
		// A bare room to hold the chest and actor.
		defineRoom(protos, ProtoRef(id+":room:hall"), "Hall", "A hall.")
		room := z.spawnRoom(ProtoRef(id + ":room:hall"))
		z.startRoom = ProtoRef(id + ":room:hall")
		s := newTestPlayerEntity(z, "P_"+id)
		s.out = make(chan *playv1.ServerFrame, 1024)
		z.players[s.character] = s
		Move(s.entity, room)
		chest := z.spawn("midgaard:obj:chest")
		Move(chest, room)
		return z, s, chest
	}

	z1, s1, chest1 := newZ("z1")
	z2, s2, chest2 := newZ("z2")

	const iters = 500
	var wg sync.WaitGroup
	wg.Add(2)
	drive := func(z *Zone, s *session) {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			z.dispatch(s, "open chest")
			z.dispatch(s, "close chest")
			drain(s) // keep the out buffer from filling
		}
	}
	go drive(z1, s1)
	go drive(z2, s2)
	wg.Wait()

	// The prototype's Container must be byte-for-byte its authored state: still closed,
	// never flipped by either instance's COW'd open/close.
	if !protoC.closed {
		t.Fatal("prototype Container.closed mutated by concurrent instance open/close")
	}
	// Each instance actually COW'd off the prototype (proves the open/close path mutates an
	// instance-owned Container, so the no-write-through assertion above is meaningful and not
	// vacuous — the loop did exercise the real COW).
	c1, _ := Get[*Container](chest1)
	c2, _ := Get[*Container](chest2)
	if any(c1) == any(protoC) || any(c2) == any(protoC) {
		t.Fatal("an instance never COW'd its Container — open/close did not exercise COW")
	}
	if any(c1) == any(c2) {
		t.Fatal("the two instances share a Container after COW (cross-instance aliasing)")
	}
}
