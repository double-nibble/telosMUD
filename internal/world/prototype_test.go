package world

import (
	"context"
	"reflect"
	"testing"
)

// Tests for prototypes & instancing — flyweight + copy-on-write (docs/MUDLIB.md §5,
// prototype.go). The load-bearing properties:
//
//   - spawning N instances of one prototype SHARES the immutable fields by reference
//     (keywords slice backing array, the component pointer) until mutated;
//   - mutating one instance COWs only that field: the prototype AND every sibling are
//     untouched (no aliased slice/map/pointer);
//   - containment (location/contents) is always instance-local, never shared;
//   - a room spawned from a prototype renders identically to before.

// spawnTestZone builds a bare zone with its own prototype cache for direct spawn tests.
func spawnTestZone(t *testing.T) *Zone {
	t.Helper()
	return newZone("test")
}

// slicePtr returns the address of a slice's backing array (0 for an empty slice), so a
// test can assert two slices share — or do NOT share — storage.
func slicePtr(s []string) uintptr {
	if len(s) == 0 {
		return 0
	}
	return reflect.ValueOf(s).Pointer()
}

func TestSpawnSharesImmutableFields(t *testing.T) {
	z := spawnTestZone(t)
	z.protos.define("test:obj:torch", []string{"torch", "wooden"},
		"a wooden torch", "A torch lies here.",
		componentSet{reflect.TypeFor[*Physical](): &Physical{weight: 2, material: "wood"}})
	proto := z.protos.get("test:obj:torch")

	const n = 40
	insts := make([]*Entity, n)
	for i := range insts {
		insts[i] = z.spawn("test:obj:torch")
		if insts[i] == nil {
			t.Fatalf("spawn returned nil")
		}
	}

	for i, e := range insts {
		// Display data reads identically to the prototype.
		if e.Name() != "a wooden torch" || e.Long() != "A torch lies here." {
			t.Fatalf("inst %d: short/long not falling through to prototype: %q / %q", i, e.Name(), e.Long())
		}
		// keywords slice is SHARED by reference with the prototype (same backing array).
		if slicePtr(e.keywordList()) != slicePtr(proto.keywords) {
			t.Fatalf("inst %d: keywords not shared with prototype (COW too eager)", i)
		}
		// The Physical component is the SAME pointer as the prototype's template.
		phys, ok := Get[*Physical](e)
		if !ok {
			t.Fatalf("inst %d: missing Physical component", i)
		}
		if any(phys) != any(proto.comps[reflect.TypeFor[*Physical]()]) {
			t.Fatalf("inst %d: Physical component not shared with prototype", i)
		}
		// proto ref is recorded; runtime ids are distinct per instance.
		if e.proto != "test:obj:torch" {
			t.Fatalf("inst %d: proto ref = %q", i, e.proto)
		}
		for j := i + 1; j < len(insts); j++ {
			if e.rid == insts[j].rid {
				t.Fatalf("instances %d and %d share a RuntimeID %d", i, j, e.rid)
			}
		}
	}
}

func TestCOWKeywordsIsolatesInstance(t *testing.T) {
	z := spawnTestZone(t)
	z.protos.define("test:obj:torch", []string{"torch", "wooden"},
		"a wooden torch", "A torch lies here.", nil)
	proto := z.protos.get("test:obj:torch")

	a := z.spawn("test:obj:torch")
	b := z.spawn("test:obj:torch")

	// Mutate a's keywords: COW must give a its own backing array and leave proto + b alone.
	a.setKeywords([]string{"torch", "blazing"})

	if got := a.keywordList(); len(got) != 2 || got[1] != "blazing" {
		t.Fatalf("a keywords after COW = %v", got)
	}
	if !reflect.DeepEqual(proto.keywords, []string{"torch", "wooden"}) {
		t.Fatalf("prototype keywords mutated by instance COW: %v", proto.keywords)
	}
	if !reflect.DeepEqual(b.keywordList(), []string{"torch", "wooden"}) {
		t.Fatalf("sibling keywords mutated by other instance COW: %v", b.keywordList())
	}
	// a no longer aliases the prototype's slice.
	if slicePtr(a.keywordList()) == slicePtr(proto.keywords) {
		t.Fatalf("a keywords still alias the prototype after COW")
	}
	// b still aliases the prototype (it never wrote).
	if slicePtr(b.keywordList()) != slicePtr(proto.keywords) {
		t.Fatalf("b keywords stopped sharing without writing")
	}

	// mutableKeywords append path: must also COW, not extend the prototype's array.
	c := z.spawn("test:obj:torch")
	c.keywords = append(c.mutableKeywords(), "extra")
	if len(proto.keywords) != 2 {
		t.Fatalf("prototype keywords grew via mutableKeywords append: %v", proto.keywords)
	}
}

func TestCOWComponentIsolatesInstance(t *testing.T) {
	z := spawnTestZone(t)
	// A room prototype with a real exits map (the reference-typed component field).
	defineRoom(z.protos, "test:room:hall", "A Hall", "A long hall.")
	z.protos.get("test:room:hall").exits()["north"] = "test:room:north"
	proto := z.protos.get("test:room:hall")

	a := z.spawnRoom("test:room:hall")
	b := z.spawn("test:room:hall") // a second instance to prove sibling isolation

	// Before mutation: both share the prototype's *Room pointer.
	if a.room != proto.comps[reflect.TypeFor[*Room]()] {
		t.Fatalf("instance a does not share the prototype Room component")
	}

	// COW the room and add an exit on instance a only.
	mutableRoom(a).exits["south"] = "test:room:south"

	// a sees both exits; proto and sibling see only the original north exit.
	if _, ok := a.room.exits["south"]; !ok {
		t.Fatalf("a's COW'd room missing the new south exit")
	}
	if _, ok := proto.exits()["south"]; ok {
		t.Fatalf("prototype exits map mutated by instance COW: %v", proto.exits())
	}
	if _, ok := b.room.exits["south"]; ok {
		t.Fatalf("sibling exits map mutated by instance COW: %v", b.room.exits)
	}
	// a's Room pointer is now its OWN, not the prototype's.
	if a.room == proto.comps[reflect.TypeFor[*Room]()] {
		t.Fatalf("a's Room component still aliases the prototype after COW")
	}
	// b still shares the prototype's Room pointer.
	if b.room != proto.comps[reflect.TypeFor[*Room]()] {
		t.Fatalf("sibling Room component stopped sharing without writing")
	}
	// The exits MAP a owns shares no storage with the prototype's map.
	if reflect.ValueOf(a.room.exits).Pointer() == reflect.ValueOf(proto.exits()).Pointer() {
		t.Fatalf("a's exits map still aliases the prototype's map")
	}
}

func TestContainmentIsInstanceLocal(t *testing.T) {
	z := spawnTestZone(t)
	defineRoom(z.protos, "test:room:hall", "A Hall", "A long hall.")
	z.protos.define("test:obj:torch", []string{"torch"}, "a torch", "A torch.", nil)

	room := z.spawnRoom("test:room:hall")
	a := z.spawn("test:obj:torch")
	b := z.spawn("test:obj:torch")

	// Fresh instances have their own empty containment.
	if a.location != nil || b.location != nil || len(a.contents) != 0 {
		t.Fatalf("spawned instance has non-empty containment")
	}
	// Putting a in the room must not touch b.
	Move(a, room)
	if a.location != room {
		t.Fatalf("a not in room after Move")
	}
	if b.location != nil {
		t.Fatalf("sibling location changed by moving another instance: %v", b.location)
	}
	if len(room.contents) != 1 || room.contents[0] != a {
		t.Fatalf("room contents wrong after Move: %v", room.contents)
	}
}

func TestRoomPrototypeRendersSameAsBefore(t *testing.T) {
	// A room spawned from a prototype renders byte-for-byte the same room text a player
	// saw from the slice-1 inline room. Drive a real login through the demo shard and
	// assert the temple render is unchanged.
	z := NewDemoShard().Zone()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	s := newTestPlayerEntity(z, "Looker")
	z.post(joinMsg{s: s})

	got := nextOutput(t, s)
	want := "The Temple Square\n" +
		"A broad plaza of worn flagstones stretches before the great temple. " +
		"Pilgrims murmur in the shade of its columns.\n" +
		"Exits: north"
	if got != want {
		t.Fatalf("temple render changed:\n got: %q\nwant: %q", got, want)
	}
}
