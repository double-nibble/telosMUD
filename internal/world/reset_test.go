package world

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
)

// Tests for the zone-reset / repop interpreter (reset.go, slice 4.4). The load-bearing
// properties, proven DETERMINISTICALLY by driving the reset interpreter / pulse tick directly
// (no real timers, like the pulse tests):
//
//   - repop tops a depleted op back up to max, and NEVER exceeds it (a full room is a no-op);
//   - the boot reset places the exact demo set (the parity test guards this too);
//   - the player-took-it case: an instance carried OUT of the room is replaced on repop;
//   - the persistent-flag op does NOT spawn a duplicate on repop (gate stays once-only).

// marketTorches counts live torch instances on the demo market floor.
func marketTorches(z *Zone) int {
	return countProto(z.rooms["midgaard:room:market"], "midgaard:obj:torch")
}

// torchReset is a max-bearing reset op the repop tests drive directly (the demo's own op uses
// count, not max; a max op is what a timed repop tops up to).
func torchReset(maxN int) content.ResetDTO {
	return content.ResetDTO{Op: "spawn_item", Proto: "midgaard:obj:torch", Room: "midgaard:room:market", Max: maxN}
}

// TestRepopTopsUpToMax removes some instances and re-runs the reset, asserting the count returns
// to max — the core "restock what was taken" behavior.
func TestRepopTopsUpToMax(t *testing.T) {
	z := NewDemoShard().Zone()
	op := torchReset(5)

	// First run on a market that already holds the 5 boot torches: a no-op (already at max).
	z.applyReset(&op)
	if got := marketTorches(z); got != 5 {
		t.Fatalf("after no-op repop: %d torches, want 5", got)
	}

	// Remove two torches (e.g. they decayed / were destroyed in the room).
	market := z.rooms["midgaard:room:market"]
	removed := 0
	for _, e := range append([]*Entity(nil), market.contents...) {
		if e.proto == "midgaard:obj:torch" && removed < 2 {
			Move(e, nil)
			removed++
		}
	}
	if got := marketTorches(z); got != 3 {
		t.Fatalf("after removing 2: %d torches, want 3", got)
	}

	// Repop tops up the difference (2), back to max (5) — never more.
	z.applyReset(&op)
	if got := marketTorches(z); got != 5 {
		t.Fatalf("after top-up repop: %d torches, want 5 (no leak/no short)", got)
	}
}

// TestRepopFullRoomIsNoOp proves repop on a room already at max adds nothing (idempotent: no
// duplicates, count stays at max no matter how many times it runs).
func TestRepopFullRoomIsNoOp(t *testing.T) {
	z := NewDemoShard().Zone()
	op := torchReset(5)
	for i := 0; i < 4; i++ {
		z.applyReset(&op)
		if got := marketTorches(z); got != 5 {
			t.Fatalf("repop iteration %d: %d torches, want 5 (no duplication)", i, got)
		}
	}
}

// TestRepopReplacesTakenItem is the player-took-it semantic: an item CARRIED OUT of the room no
// longer counts toward max (room-scoped count), so the next repop replaces it — the floor refills.
func TestRepopReplacesTakenItem(t *testing.T) {
	z := NewDemoShard().Zone()
	op := torchReset(5)

	// A player carries a torch out of the market (into their own contents).
	player := newTestPlayerEntity(z, "Thief").entity
	market := z.rooms["midgaard:room:market"]
	var taken *Entity
	for _, e := range market.contents {
		if e.proto == "midgaard:obj:torch" {
			taken = e
			break
		}
	}
	if taken == nil {
		t.Fatal("no torch on the market floor to take")
	}
	Move(taken, player) // carried out of the room
	if got := marketTorches(z); got != 4 {
		t.Fatalf("after take: %d torches on floor, want 4", got)
	}

	// Repop: the carried torch is no longer on the floor, so it does not count — the floor
	// refills to max. (The classic Diku global count would NOT restock here; we restock, the
	// documented room-scoped choice in reset.go.)
	z.applyReset(&op)
	if got := marketTorches(z); got != 5 {
		t.Fatalf("after repop following a take: %d torches, want 5 (restock-on-take)", got)
	}
	// The taken torch is still carried (repop never touches what left the room).
	if countProto(player, "midgaard:obj:torch") != 1 {
		t.Fatal("the carried torch was disturbed by repop")
	}
}

// TestRepopPulseTick drives the repop CADENCE through the pulse scheduler (reset.go startRepop),
// proving reset_secs -> pulse wiring tops up on a tick — deterministically, by ticking the
// scheduler directly rather than waiting on a real timer.
func TestRepopPulseTick(t *testing.T) {
	// The demo midgaard zone now authors reset_secs (so the world repops), which means build() ALREADY
	// registered its repop callback at the demo cadence — startRepop is idempotent, so the zone drives
	// its OWN cadence (a second startRepop is a no-op). This test proves the reset_secs -> pulse wiring
	// by depleting the floor and ticking the demo zone's REAL stride, asserting the repop tops it back up.
	z := NewDemoShard().Zone()
	const demoResetSecs = 90 // matches packs/demo.yaml midgaard reset_secs
	stride := uint64(demoResetSecs) * pulsesPerSecond

	// Deplete the floor.
	market := z.rooms["midgaard:room:market"]
	for _, e := range append([]*Entity(nil), market.contents...) {
		if e.proto == "midgaard:obj:torch" {
			Move(e, nil)
		}
	}
	if got := marketTorches(z); got != 0 {
		t.Fatalf("after clearing: %d torches, want 0", got)
	}

	// Tick the scheduler one full stride to fire the repop callback once (single-writer: the
	// callback runs inline in tick, exactly as Zone.Run would call it).
	for i := uint64(0); i < stride; i++ {
		z.pulses.tick()
	}
	if got := marketTorches(z); got != 5 {
		t.Fatalf("after one repop stride: %d torches, want 5", got)
	}
}

// TestRepopCadenceZeroNoTimer proves reset_secs==0 registers NO repop callback (no timed reset),
// so a demo-style zone's floor is only filled by the boot reset and never re-topped on a timer.
func TestRepopCadenceZeroNoTimer(t *testing.T) {
	z := newZone("test")
	z.startRepop([]content.ResetDTO{torchReset(5)}, 0)
	if z.repopPulse != nil {
		t.Fatal("reset_secs==0 must register no repop pulse")
	}
}

// --- persistent-flag gate -------------------------------------------------------------------

// stubObjectLoader is a minimal ObjectLoader returning a fixed durable object set, so the
// persistent-gate test exercises the load path with no Postgres. calls is guarded so the test can
// read it race-free after the load goroutine signals done.
type stubObjectLoader struct {
	objects []PersistentObject
	done    chan struct{} // closed/sent after the (single) load
	calls   atomic.Int64
}

func (s *stubObjectLoader) LoadObjects(_ context.Context, _, _ string) ([]PersistentObject, error) {
	s.calls.Add(1)
	if s.done != nil {
		s.done <- struct{}{}
	}
	return s.objects, nil
}

// TestPersistentResetNoDuplicateOnRepop proves a persistent-flagged op loads its durable objects
// at MOST once and is a no-op on every subsequent repop — so a persistent object is never
// duplicated by the repop timer. The test goroutine acts as the single-writer zone goroutine
// (calling applyReset / rehydrateObjects directly, exactly as Zone.handle would); only the durable
// LOAD is off-goroutine, and we wait on the loader's done signal for it. A second applyReset is the
// repop tick: the once-only guard must make it spawn nothing and never re-load.
func TestPersistentResetNoDuplicateOnRepop(t *testing.T) {
	z := newZone("test")
	r := &Entity{rid: z.rids.alloc(), proto: "test:room:vault", zone: z, comps: componentSet{}}
	z.rooms["test:room:vault"] = r
	z.protos.define("midgaard:obj:helmet", []string{"helmet"}, "an iron helmet", "An iron helmet rests here.", componentSet{})

	loader := &stubObjectLoader{
		objects: []PersistentObject{{ProtoRef: "midgaard:obj:helmet"}},
		done:    make(chan struct{}, 4),
	}
	z.objects = loader
	op := content.ResetDTO{Op: "spawn_item", Proto: "midgaard:obj:helmet", Room: "test:room:vault", Persistent: true}

	// Boot reset: marks the op done and kicks the off-goroutine load. Wait for the load to run,
	// then apply the loadObjectsMsg it posted (drain the inbox the zone would have drained).
	z.applyReset(&op)
	select {
	case <-loader.done:
	case <-time.After(2 * time.Second):
		t.Fatal("persistent load never ran")
	}
	drainObjectLoads(t, z)

	// Repop tick: the once-only guard makes this a no-op — no second load, no second spawn.
	z.applyReset(&op)
	// Give a (non-existent) second load goroutine no chance to have fired: applyReset returned
	// before touching z.objects on the repop, so calls stays 1.
	if c := loader.calls.Load(); c != 1 {
		t.Fatalf("loader called %d times, want exactly 1 (repop must not reload)", c)
	}
	if got := countProto(r, "midgaard:obj:helmet"); got != 1 {
		t.Fatalf("persistent object count = %d, want exactly 1 (no repop duplication)", got)
	}
}

// drainObjectLoads pulls the loadObjectsMsg the off-goroutine persistent load posted and applies
// it on (this) goroutine, standing in for the zone loop's dispatch.
func drainObjectLoads(t *testing.T, z *Zone) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-z.inbox:
			if lo, ok := m.(loadObjectsMsg); ok {
				z.rehydrateObjects(lo)
				return
			}
		case <-deadline:
			t.Fatal("no loadObjectsMsg posted by the persistent load")
		}
	}
}

// TestPersistentResetNilLoaderGracefulNoOp proves the gate degrades cleanly with no loader wired
// (the storeless/bare boot): a persistent op spawns nothing and does not panic.
func TestPersistentResetNilLoaderGracefulNoOp(t *testing.T) {
	z := newZone("test")
	r := &Entity{rid: z.rids.alloc(), proto: "test:room:vault", zone: z, comps: componentSet{}}
	z.rooms["test:room:vault"] = r
	op := content.ResetDTO{Op: "spawn_item", Proto: "midgaard:obj:helmet", Room: "test:room:vault", Persistent: true}
	z.applyReset(&op) // z.objects == nil: logged no-op, no spawn, no panic
	if got := len(r.contents); got != 0 {
		t.Fatalf("nil-loader persistent op placed %d objects, want 0", got)
	}
}
