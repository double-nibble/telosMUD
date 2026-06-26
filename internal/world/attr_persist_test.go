package world

import (
	"reflect"
	"testing"
)

// attr_persist_test.go asserts attribute BASE OVERRIDES + resource CURRENTS round-trip through
// dumpCharacter/loadCharacter, that DERIVED values are recomputed (never stored), and that a COW of
// a Living deep-copies the new reference-typed instance state.

// TestAttributeAndResourceRoundTrip dumps a demo player with custom bases + a wounded hp, then loads
// the snapshot into a fresh entity and checks the bases + clamped current came back and the derived
// max recomputed.
func TestAttributeAndResourceRoundTrip(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	// Source player: bump constitution+level (raising the derived max_hp) and wound hp.
	src := &session{character: "Wynne"}
	e := z.newPlayerEntity(src, "Wynne")
	setAttrBase(e, "constitution", 14)
	setAttrBase(e, "level", 5)
	// max_hp now = 14*10 + 5*5 = 165. Wound to 100.
	if got := e.MaxHP(); got != 165 {
		t.Fatalf("source MaxHP() = %d, want 165", got)
	}
	e.SetHP(100)

	snap := dumpCharacter(src)

	// The dump stores BASES + CURRENT only; never the derived max.
	if snap.State.Attributes["constitution"] != 14 || snap.State.Attributes["level"] != 5 {
		t.Fatalf("dumped attributes = %v, want con=14 level=5", snap.State.Attributes)
	}
	if _, stored := snap.State.Attributes["max_hp"]; stored {
		t.Fatal("derived max_hp must NOT be stored (recomputed on load)")
	}
	if snap.State.Resources["hp"].Cur != 100 {
		t.Fatalf("dumped hp cur = %d, want 100", snap.State.Resources["hp"].Cur)
	}

	// Load into a fresh entity (a different session/login).
	dst := &session{character: "Wynne"}
	z.newPlayerEntity(dst, "Wynne")
	loadCharacter(z, dst, snap)
	de := dst.entity

	if got := de.Attr("constitution"); got != 14 {
		t.Fatalf("loaded constitution = %v, want 14", got)
	}
	// Derived max recomputed from the loaded bases.
	if got := de.MaxHP(); got != 165 {
		t.Fatalf("loaded MaxHP() = %d, want recomputed 165", got)
	}
	if got := de.HP(); got != 100 {
		t.Fatalf("loaded HP() = %d, want 100", got)
	}
}

// TestLoadContentlessSnapshotSane: a snapshot with NO attributes/resources subtrees (a pre-5.1 save
// or a contentless character) loads without error and reads sane defaults.
func TestLoadContentlessSnapshotSane(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := &session{character: "Old"}
	z.newPlayerEntity(s, "Old")
	// A bare snapshot: no Attributes / Resources maps at all.
	loadCharacter(z, s, CharSnapshot{Name: "Old", State: StateJSON{}})
	// No overrides installed -> defaults from the demo defs; full hp.
	if got := s.entity.MaxHP(); got != 105 {
		t.Fatalf("contentless-load MaxHP() = %d, want default 105", got)
	}
	if got := s.entity.HP(); got != 105 {
		t.Fatalf("contentless-load HP() = %d, want full 105", got)
	}
}

// TestLivingCOWDeepCopiesStatState arms a prototype-backed Living, COWs it, and asserts the clone's
// attrBase/resCur maps do not alias the prototype's (mutating one never touches the other).
func TestLivingCOWDeepCopiesStatState(t *testing.T) {
	// Build a prototype carrying a Living with seeded base/current state.
	c := newProtoCache()
	protoLiving := &Living{
		attrBase: map[string]float64{"strength": 12},
		resCur:   map[string]int{"hp": 50},
	}
	c.define("mob:test", []string{"test"}, "a test mob", "A test mob stands here.",
		componentSet{reflect.TypeFor[*Living](): protoLiving})

	z := newZone("test")
	z.protos = c
	inst := z.spawn("mob:test")
	if inst == nil {
		t.Fatal("spawn returned nil")
	}
	// The instance shares the prototype's Living pointer until COW.
	owned := mutableComponent[*Living](inst)
	if owned == protoLiving {
		t.Fatal("mutableComponent did not COW the Living")
	}
	// Mutate the clone's maps; the prototype's must be untouched.
	owned.attrBase["strength"] = 99
	owned.resCur["hp"] = 1
	if protoLiving.attrBase["strength"] != 12 {
		t.Fatalf("prototype attrBase aliased: %v", protoLiving.attrBase)
	}
	if protoLiving.resCur["hp"] != 50 {
		t.Fatalf("prototype resCur aliased: %v", protoLiving.resCur)
	}
}
