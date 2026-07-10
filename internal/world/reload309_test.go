package world

import (
	"context"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// reload309_test.go — #309. An ADVISORY (non-blocking) heads-up when a reload REMOVES a room/proto that a
// NOT-reloaded pack still references. #205 correctly does not hard-block the reload for an out-of-scope pack's
// dependency; #309 surfaces it so the operator isn't blindsided. Precision needs a pre-vs-post diff: only a
// reference whose target USED TO resolve (wasLive) is warned — a pre-existing dangler in the depending pack is
// not this reload's doing and stays silent.

// livingSet builds a wasLive predicate from a fixed set of refs that were live before the reload.
func livingSet(refs ...string) func(string) bool {
	live := map[string]bool{}
	for _, r := range refs {
		live[r] = true
	}
	return func(ref string) bool { return live[ref] }
}

// TestAdvisoryWarnsWhenReloadRemovesADependedRoom is the headline. Pack A (reloaded) drops az:room:5; pack B
// (not reloaded) still has an exit into it. The post-reload graph has no az:room:5, but it WAS live — so the
// reload removed it and B's exit now dead-ends. Advisory, not rejection.
func TestAdvisoryWarnsWhenReloadRemovesADependedRoom(t *testing.T) {
	// Post-reload graph: A's zone az NO LONGER contains az:room:5 (removed); B's exit still points at it.
	packA := content.Pack{
		Pack:  "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{r205room("az:room:1", nil)}}},
	}
	packB := content.Pack{
		Pack: "b",
		Zones: []content.ZoneDTO{{Ref: "bz", Rooms: []content.RoomDTO{
			r205room("bz:room:9", map[string]string{"east": "az:room:5"}),
		}}},
	}
	scope := newReloadScope([]content.Pack{packA, packB}, map[string]bool{"a": true})

	// az:room:5 was live before the reload; the reload (of A) removed it.
	adv := advisoryReloadRemovals(scope, livingSet("az:room:5", "az:room:1", "bz:room:9"))
	if len(adv) != 1 {
		t.Fatalf("expected exactly one advisory for the removed room B depends on, got %d: %v", len(adv), adv)
	}
	if !strings.Contains(adv[0], "bz:room:9") || !strings.Contains(adv[0], "az:room:5") {
		t.Fatalf("advisory should name the dependent exit and the removed room, got %q", adv[0])
	}

	// #205 must still NOT hard-reject A for B's now-dangling exit (the migration trap holds alongside #309).
	if p := validatePacks([]content.Pack{packA, packB}, map[string]bool{"a": true}); len(p) != 0 {
		t.Fatalf("the reload must not be BLOCKED by the removal — #309 is advisory only, got rejections: %v", p)
	}
}

// TestAdvisoryIgnoresPreExistingDangler is the no-false-positive invariant, and the reason the check needs a
// pre-reload diff rather than a snapshot. B has an exit into a room that NEVER existed — a pre-existing defect
// in B, not caused by this reload. wasLive is false for it, so no advisory.
func TestAdvisoryIgnoresPreExistingDangler(t *testing.T) {
	packA := content.Pack{
		Pack:  "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{r205room("az:room:1", nil)}}},
	}
	packB := content.Pack{
		Pack: "b",
		Zones: []content.ZoneDTO{{Ref: "bz", Rooms: []content.RoomDTO{
			r205room("bz:room:9", map[string]string{"east": "bz:room:ghost"}), // never existed
		}}},
	}
	scope := newReloadScope([]content.Pack{packA, packB}, map[string]bool{"a": true})

	// bz:room:ghost was never live — the dangler predates this reload.
	adv := advisoryReloadRemovals(scope, livingSet("az:room:1", "bz:room:9"))
	if len(adv) != 0 {
		t.Fatalf("a pre-existing dangler (target never live) must NOT be advised — it is not this reload's "+
			"doing: %v", adv)
	}
}

// TestAdvisorySilentWhenTargetStillPresent: if the reload does NOT actually remove the room (it still exists in
// the post-reload graph), there is nothing to warn about, even though the target was live and referenced.
func TestAdvisorySilentWhenTargetStillPresent(t *testing.T) {
	packA := content.Pack{
		Pack: "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{
			r205room("az:room:1", nil), r205room("az:room:5", nil), // az:room:5 still present
		}}},
	}
	packB := content.Pack{
		Pack: "b",
		Zones: []content.ZoneDTO{{Ref: "bz", Rooms: []content.RoomDTO{
			r205room("bz:room:9", map[string]string{"east": "az:room:5"}),
		}}},
	}
	scope := newReloadScope([]content.Pack{packA, packB}, map[string]bool{"a": true})

	adv := advisoryReloadRemovals(scope, livingSet("az:room:5", "az:room:1", "bz:room:9"))
	if len(adv) != 0 {
		t.Fatalf("the room still resolves post-reload, so nothing is removed — no advisory expected: %v", adv)
	}
}

// TestAdvisoryScopedToOutOfScopeDependers: an IN-SCOPE pack's own reference to something it removed is a HARD
// rejection (validateRoomExits), not an advisory — advisoryReloadRemovals must not double-report it. Here A
// (in scope) both defines and dangles: its own exit into the room it dropped is #205's business, not #309's.
func TestAdvisoryScopedToOutOfScopeDependers(t *testing.T) {
	packA := content.Pack{
		Pack: "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{
			r205room("az:room:1", map[string]string{"north": "az:room:5"}), // A's OWN exit into the removed room
		}}},
	}
	scope := newReloadScope([]content.Pack{packA}, map[string]bool{"a": true})

	adv := advisoryReloadRemovals(scope, livingSet("az:room:5", "az:room:1"))
	if len(adv) != 0 {
		t.Fatalf("an in-scope pack's own dangler is a hard rejection, not a #309 advisory: %v", adv)
	}
	// And #205 DOES hard-reject it, so it is not silently lost.
	if p := validatePacks([]content.Pack{packA}, map[string]bool{"a": true}); len(p) == 0 {
		t.Fatal("in-scope dangling exit into a removed room must be a hard rejection (#205)")
	}
}

// TestAdvisoryWarnsWhenReloadRemovesADependedProto covers the reset variant: a not-reloaded zone's reset spawns
// a mob the reload removes.
func TestAdvisoryWarnsWhenReloadRemovesADependedProto(t *testing.T) {
	packA := content.Pack{
		Pack:  "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{r205room("az:room:1", nil)}}}, // az:mob:goblin dropped
	}
	packB := content.Pack{
		Pack: "b",
		Zones: []content.ZoneDTO{{
			Ref:    "bz",
			Rooms:  []content.RoomDTO{r205room("bz:room:1", nil)},
			Resets: []content.ResetDTO{{Op: "spawn_mob", Proto: "az:mob:goblin", Room: "bz:room:1"}},
		}},
	}
	scope := newReloadScope([]content.Pack{packA, packB}, map[string]bool{"a": true})

	adv := advisoryReloadRemovals(scope, livingSet("az:mob:goblin", "az:room:1", "bz:room:1"))
	if len(adv) != 1 || !strings.Contains(adv[0], "az:mob:goblin") {
		t.Fatalf("expected a reset advisory naming the removed proto, got %v", adv)
	}
}

// TestReloadSummaryRendersAdvisories pins that the advisory reaches the operator's readout (not just the log),
// and only on a non-rejected reload.
func TestReloadSummaryRendersAdvisories(t *testing.T) {
	out := reloadOutcome{
		published:  3,
		advisories: []string{`room "bz:room:9" exit "east" leads to "az:room:5", which this reload REMOVES`},
	}
	s := reloadSummary("a", out)
	if !strings.Contains(s, "Heads-up") || !strings.Contains(s, "az:room:5") {
		t.Fatalf("the readout must surface the #309 advisory, got:\n%s", s)
	}

	// On a hard rejection the advisories are moot (nothing was applied) — not shown.
	rejected := reloadSummary("a", reloadOutcome{
		rejected:   []string{"attribute \"x\": bad base formula"},
		advisories: []string{"room \"bz:room:9\" ..."},
	})
	if strings.Contains(rejected, "Heads-up") {
		t.Fatalf("advisories must not be shown on a hard rejection (nothing was applied):\n%s", rejected)
	}
}

// TestAdvisoryEndToEndThroughRepublish drives the real republish path so the wasLive predicate — the shard's
// live proto cache at validate time — is exercised, not a fake. Boot two packs (A owns az:room:5, B has an
// exit into it), then edit the source to REMOVE az:room:5 from A and reload A. The advisory must fire because
// the cache still holds the pre-reload az:room:5 while the re-read no longer defines it.
func TestAdvisoryEndToEndThroughRepublish(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(content.Pack{Pack: "a", Zones: []content.ZoneDTO{{
		Ref: "az", Name: "AZ", StartRoom: "az:room:1",
		Rooms: []content.RoomDTO{
			{Ref: "az:room:1", Name: "One", Long: "r1"},
			{Ref: "az:room:5", Name: "Five", Long: "r5"},
		},
	}}})
	src.SetPack(content.Pack{Pack: "b", Zones: []content.ZoneDTO{{
		Ref: "bz", Name: "BZ", StartRoom: "bz:room:9",
		Rooms: []content.RoomDTO{{
			Ref: "bz:room:9", Name: "Nine", Long: "r9", Exits: map[string]string{"east": "az:room:5"},
		}},
	}}})

	lc, err := content.Load(context.Background(), src, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	bus := contentbus.NewMemBus()
	t.Cleanup(func() { _ = bus.Close() })
	sh := NewShardFromContent(lc, []string{"az", "bz"}, "az", "", nil, nil).
		WithHotReload(src, bus, []string{"a", "b"}, 0)
	if sh.reloader == nil {
		t.Fatal("hot reload not enabled")
	}
	if sh.reloader.cache.get("az:room:5") == nil {
		t.Fatal("precondition: az:room:5 must be live in the cache before the reload")
	}

	// Edit A to REMOVE az:room:5.
	src.SetPack(content.Pack{Pack: "a", Zones: []content.ZoneDTO{{
		Ref: "az", Name: "AZ", StartRoom: "az:room:1",
		Rooms: []content.RoomDTO{{Ref: "az:room:1", Name: "One", Long: "r1"}}, // room 5 dropped
	}}})

	// --check so nothing is published (the cache stays the pre-reload snapshot); the advisory is still computed.
	out := sh.reloader.republish(context.Background(), []string{"a"}, true)
	if len(out.rejected) != 0 {
		t.Fatalf("removing a room another pack references must NOT hard-reject the reload: %v", out.rejected)
	}
	if len(out.advisories) != 1 || !strings.Contains(out.advisories[0], "az:room:5") {
		t.Fatalf("expected one advisory naming the removed room, got %v", out.advisories)
	}
	if !strings.Contains(out.advisories[0], "bz:room:9") {
		t.Fatalf("the advisory should name the dependent room, got %q", out.advisories[0])
	}
}

// TestAdvisoryWarnsWhenReloadRemovesADependedStartRoom: a not-reloaded zone whose START ROOM the reload
// removes — entering that zone (login/recall) would dead-end. Same room-ref mechanism as an exit.
func TestAdvisoryWarnsWhenReloadRemovesADependedStartRoom(t *testing.T) {
	packA := content.Pack{
		Pack:  "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{r205room("az:room:1", nil)}}}, // az:room:hub dropped
	}
	packB := content.Pack{
		Pack: "b",
		Zones: []content.ZoneDTO{{
			Ref: "bz", StartRoom: "az:room:hub", // B's zone starts in a room A owns and now removes
			Rooms: []content.RoomDTO{r205room("bz:room:1", nil)},
		}},
	}
	scope := newReloadScope([]content.Pack{packA, packB}, map[string]bool{"a": true})

	adv := advisoryReloadRemovals(scope, livingSet("az:room:hub", "az:room:1", "bz:room:1"))
	if len(adv) != 1 || !strings.Contains(adv[0], "az:room:hub") || !strings.Contains(adv[0], "start room") {
		t.Fatalf("expected a start-room advisory naming the removed room, got %v", adv)
	}
}

// TestAdvisoryDedupesIdenticalResetLines: two resets in one not-reloaded zone spawning the same removed proto
// must produce ONE advisory line, not two.
func TestAdvisoryDedupesIdenticalResetLines(t *testing.T) {
	packA := content.Pack{
		Pack:  "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{r205room("az:room:1", nil)}}},
	}
	packB := content.Pack{
		Pack: "b",
		Zones: []content.ZoneDTO{{
			Ref:   "bz",
			Rooms: []content.RoomDTO{r205room("bz:room:1", nil)},
			Resets: []content.ResetDTO{
				{Op: "spawn_mob", Proto: "az:mob:goblin", Room: "bz:room:1"},
				{Op: "spawn_mob", Proto: "az:mob:goblin", Room: "bz:room:1"}, // same removed proto
			},
		}},
	}
	scope := newReloadScope([]content.Pack{packA, packB}, map[string]bool{"a": true})

	adv := advisoryReloadRemovals(scope, livingSet("az:mob:goblin", "az:room:1", "bz:room:1"))
	if len(adv) != 1 {
		t.Fatalf("two identical reset advisories must collapse to one, got %d: %v", len(adv), adv)
	}
}

// TestAdvisoryNilWasLiveIsNoOp: a nil predicate (no cache wired, a bare test shard) yields no advisories
// rather than panicking — the fail-safe for a reloader without a live cache to diff against.
func TestAdvisoryNilWasLiveIsNoOp(t *testing.T) {
	packA := content.Pack{
		Pack: "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{
			r205room("az:room:1", map[string]string{"north": "az:room:gone"}),
		}}},
	}
	if adv := advisoryReloadRemovals(newReloadScope([]content.Pack{packA}, map[string]bool{"b": true}), nil); adv != nil {
		t.Fatalf("a nil wasLive predicate must produce no advisories, got %v", adv)
	}
}
