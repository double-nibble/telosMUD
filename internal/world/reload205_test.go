package world

import (
	"context"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// reload205_test.go — #205 full merged-graph reload validation. validatePacks now resolves refs against the
// WHOLE enabled pack set (context) but REJECTS only findings a reloaded (in-scope) pack contributes
// (provenance-by-last-writer). These tests pin both invariants: a cross-pack/cross-zone defect the reloaded
// pack causes IS caught, and an unrelated pack's defect never blocks the reload (the migration trap).

func r205expr(e any) content.BaseSpecDTO    { return content.BaseSpecDTO{Expr: e} }
func r205lit(v float64) content.BaseSpecDTO { return content.BaseSpecDTO{Lit: &v} }
func r205room(ref string, exits map[string]string) content.RoomDTO {
	return content.RoomDTO{Ref: ref, Exits: exits}
}

// TestReloadValidateMigrationTrap is the headline invariant: reloading a CLEAN pack A while a BROKEN pack B is
// also enabled must produce ZERO rejections — B's defects are not attributed to A. (Widening the resolution
// context from scoped-only to the full graph WITHOUT the provenance gate would regress this, newly blocking
// `reload A` on B's pre-existing breakage.)
func TestReloadValidateMigrationTrap(t *testing.T) {
	packA := content.Pack{
		Pack:       "a",
		Attributes: []content.AttributeDTO{{Ref: "aclean", DefaultBase: r205lit(10)}},
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{
			r205room("az:room:1", map[string]string{"north": "az:room:2"}), r205room("az:room:2", nil),
		}}},
	}
	// B is broken four ways: a self-contained attribute cycle, a dangling exit, a proto collision, and a bad
	// reset. None involves A.
	packB := content.Pack{
		Pack: "b",
		Attributes: []content.AttributeDTO{
			{Ref: "b1", DefaultBase: r205expr([]any{"attr", "b2"})},
			{Ref: "b2", DefaultBase: r205expr([]any{"attr", "b1"})},
		},
		Zones: []content.ZoneDTO{{
			Ref: "bz",
			Rooms: []content.RoomDTO{
				r205room("bz:room:1", map[string]string{"south": "bz:room:gone"}), // dangling
				r205room("bz:room:dup", nil),
			},
			Items:  []content.ProtoDTO{{Ref: "bz:room:dup"}}, // ref collision with the room above
			Resets: []content.ResetDTO{{Op: "spawn_frog", Proto: "bz:room:1", Room: "bz:room:1"}},
		}},
	}
	full := []content.Pack{packA, packB}

	if p := validatePacks(full, map[string]bool{"a": true}); len(p) != 0 {
		t.Fatalf("reload A must NOT be blocked by broken pack B (the migration trap): %v", p)
	}
	// Sanity: reloading B DOES surface B's defects (the checks aren't just disabled).
	if p := validatePacks(full, map[string]bool{"b": true}); len(p) == 0 {
		t.Fatal("reload B must catch B's own defects")
	}
}

// TestReloadValidateCrossPackCycleCaught: a cycle spanning a reloaded pack A and a not-reloaded pack B IS
// caught on `reload A` (A participates) — the cross-pack blind spot #205 closes — but an unrelated `reload C`
// is not blocked by it.
func TestReloadValidateCrossPackCycleCaught(t *testing.T) {
	full := []content.Pack{
		{Pack: "a", Attributes: []content.AttributeDTO{{Ref: "sa", DefaultBase: r205expr([]any{"attr", "sb"})}}},
		{Pack: "b", Attributes: []content.AttributeDTO{{Ref: "sb", DefaultBase: r205expr([]any{"attr", "sa"})}}},
		{Pack: "c", Attributes: []content.AttributeDTO{{Ref: "sc", DefaultBase: r205lit(1)}}},
	}
	if p := validatePacks(full, map[string]bool{"a": true}); len(p) == 0 {
		t.Fatal("reload A must catch the cross-pack cycle sa<->sb (A contributes an edge)")
	}
	if p := validatePacks(full, map[string]bool{"c": true}); len(p) != 0 {
		t.Fatalf("reload C (unrelated) must not be blocked by the sa<->sb cycle: %v", p)
	}
}

// TestReloadValidateCrossZoneExitCaught: a cross-zone exit into another zone now resolves against the full
// graph — one to an EXISTING room passes; one to a nonexistent room in a not-reloaded zone is CAUGHT on the
// reload of the owning zone's pack (the cross-zone blind spot #205 closes).
func TestReloadValidateCrossZoneExitCaught(t *testing.T) {
	full := []content.Pack{
		{Pack: "a", Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{
			r205room("az:room:1", map[string]string{"north": "bz:room:real", "south": "bz:room:fake"}),
		}}}},
		{Pack: "b", Zones: []content.ZoneDTO{{Ref: "bz", Rooms: []content.RoomDTO{r205room("bz:room:real", nil)}}}},
	}
	p := validatePacks(full, map[string]bool{"a": true})
	if len(p) != 1 || !strings.Contains(p[0], "bz:room:fake") {
		t.Fatalf("reload A must catch ONLY the cross-zone exit to the nonexistent bz:room:fake: %v", p)
	}
	// reload B (which doesn't own the exit) is not blocked by A's dangling exit.
	if p := validatePacks(full, map[string]bool{"b": true}); len(p) != 0 {
		t.Fatalf("reload B must not be blocked by an exit A's zone owns: %v", p)
	}
}

// TestReloadValidateInertOverrideNotBlamed: a reloaded pack A defines an attribute a LATER pack B overrides —
// A's definition is INERT (never the live def), so a defect in A's dead copy must not block `reload A`.
func TestReloadValidateInertOverrideNotBlamed(t *testing.T) {
	full := []content.Pack{
		{Pack: "a", Attributes: []content.AttributeDTO{{Ref: "x", DefaultBase: r205expr("not-a-node")}}}, // bad, but dead
		{Pack: "b", Attributes: []content.AttributeDTO{{Ref: "x", DefaultBase: r205lit(5)}}},             // the LIVE def (last writer)
	}
	if p := validatePacks(full, map[string]bool{"a": true}); len(p) != 0 {
		t.Fatalf("reload A must not be blocked by a defect in A's INERT (overridden) attribute: %v", p)
	}
	// If B (the live owner) had the bad formula, it WOULD be caught.
	full[1].Attributes[0].DefaultBase = r205expr("not-a-node")
	if p := validatePacks(full, map[string]bool{"b": true}); len(p) == 0 {
		t.Fatal("reload B must catch the bad formula on the LIVE attribute it owns")
	}
}

// TestReloadRepublishScopedIgnoresBrokenOtherPack pins the reloadcmd WIRING (#205): a scoped `reload a` loads
// the full enabled set as context but validates only pack a's contribution, so a pre-broken not-reloaded pack
// b never blocks it — while `reload b` DOES reject b's defects.
func TestReloadRepublishScopedIgnoresBrokenOtherPack(t *testing.T) {
	packA := content.Pack{Pack: "a", Zones: []content.ZoneDTO{{
		Ref: "az", Name: "Zone A", StartRoom: "az:room:1",
		Rooms: []content.RoomDTO{r205room("az:room:1", map[string]string{"north": "az:room:2"}), r205room("az:room:2", nil)},
	}}}
	// Pack b boots (content is fail-safe) but is broken: a dangling intra-zone exit.
	packB := content.Pack{Pack: "b", Zones: []content.ZoneDTO{{
		Ref: "bz", Name: "Zone B", StartRoom: "bz:room:1",
		Rooms: []content.RoomDTO{r205room("bz:room:1", map[string]string{"south": "bz:room:gone"})},
	}}}
	src := content.NewMemSource()
	src.SetPack(packA)
	src.SetPack(packB)
	bus := contentbus.NewMemBus()
	defer func() { _ = bus.Close() }()

	lc, err := content.Load(context.Background(), src, []string{"a", "b"})
	if err != nil {
		t.Fatalf("boot load: %v", err)
	}
	s := NewShardFromContent(lc, []string{"az", "bz"}, "az", "", nil, nil).
		WithHotReload(src, bus, []string{"a", "b"}, 0)
	if s.reloader == nil {
		t.Fatal("hot reload not enabled")
	}

	// reload a: full context = {a,b}, scoped = {a}. b's dangling exit is not attributed to a.
	if out := s.reloader.republish(context.Background(), []string{"a"}, false); len(out.rejected) != 0 {
		t.Fatalf("scoped reload a must not be blocked by broken pack b: %v", out.rejected)
	}
	// reload b: b's own dangling exit is rejected.
	if out := s.reloader.republish(context.Background(), []string{"b"}, false); len(out.rejected) == 0 {
		t.Fatal("scoped reload b must reject b's own dangling exit")
	}
}

// TestReloadRepublishResolvesCoreRefs pins the #205 core-layering fix: a reloaded pack whose room exits into a
// CORE room must NOT be flagged dangling — republish layers the embedded core pack UNDER the enabled set (as
// boot does), so a cross-pack reference into core resolves in the validation graph.
func TestReloadRepublishResolvesCoreRefs(t *testing.T) {
	packA := content.Pack{Pack: "a", Zones: []content.ZoneDTO{{
		Ref: "az", Name: "Zone A", StartRoom: "az:room:1",
		Rooms: []content.RoomDTO{r205room("az:room:1", map[string]string{"up": "core:room:nexus"})},
	}}}
	src := content.NewMemSource()
	src.SetPack(packA)
	bus := contentbus.NewMemBus()
	defer func() { _ = bus.Close() }()
	lc, err := content.Load(context.Background(), src, []string{"a"})
	if err != nil {
		t.Fatalf("boot load: %v", err)
	}
	s := NewShardFromContent(lc, []string{"az"}, "az", "", nil, nil).
		WithHotReload(src, bus, []string{"a"}, 0)
	if out := s.reloader.republish(context.Background(), []string{"a"}, false); len(out.rejected) != 0 {
		t.Fatalf("an exit into a core room must resolve (core is layered into the validation graph #205): %v", out.rejected)
	}
}
