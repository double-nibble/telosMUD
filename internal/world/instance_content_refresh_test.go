package world

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// instance_content_refresh_test.go — #418: what a RUNTIME-built zone is assembled from.
//
// The bug: s.content was written once at construction and never again, so every zone built after boot — a
// HostZone after a rebalance, and every single instance mint — used the content the process happened to
// start with, while its prototypes came from the live reloaded cache. Two halves of one zone, from two
// different versions of the content. A long-lived process drifted further with every mint, and a template
// a reload had DELETED still minted.
//
// These tests pin the fixed behavior from both ends: the snapshot converges on the paths that change
// content, and the things that must NOT move (a live instance's pinned content, the previous snapshot on a
// read failure) still do not.

// refreshTestPack is an instance-capable one-zone pack. Distinct from reloadTestPack because these tests
// mutate the ROOM SET and the `instanceable` opt-in — neither of which the per-ref reload tests touch — and
// a shared fixture that both mutate would couple them.
func refreshTestPack(rooms ...content.RoomDTO) content.Pack {
	if len(rooms) == 0 {
		rooms = []content.RoomDTO{{
			Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.", Exits: map[string]string{},
		}}
	}
	return content.Pack{
		Pack: "refreshtest",
		Zones: []content.ZoneDTO{
			{
				Ref:          "ct",
				Name:         "Refresh Test Zone",
				StartRoom:    "ct:room:entry",
				Instanceable: true,
				Rooms:        rooms,
			},
			survivingZone(),
		},
	}
}

// survivingZone is a second zone every fixture pack carries, and it is not decoration.
//
// The deletion tests below retire zone `ct`. With a ONE-zone pack, retiring it leaves the pack with no
// zones at all — which is not what a zone deletion looks like, it is what a PRUNED OR RENAMED PACK looks
// like, and the refresh deliberately refuses to publish that (it would empty the snapshot and make every
// runtime zone build on the shard refuse). A sibling that survives keeps the fixture on the real-deployment
// path: one zone retired out of several.
func survivingZone() content.ZoneDTO {
	return content.ZoneDTO{
		Ref: "ct-keep", Name: "Surviving Zone", StartRoom: "ct-keep:room:hall",
		Rooms: []content.RoomDTO{{Ref: "ct-keep:room:hall", Name: "Hall", Long: "A hall.", Exits: map[string]string{}}},
	}
}

// fastContentRefresh shrinks the refresh debounce for the duration of a test.
//
// Production waits seconds before re-reading, deliberately (see contentRefreshDebounce: it lets a pull's
// per-ref fan-out drain before the snapshot read, and spreads the fleet's reads). A test must not race that
// window — with the production values a 5s poll deadline and a 2–5s debounce are the same order of
// magnitude, which is how a suite becomes intermittently red for reasons that have nothing to do with the
// behavior under test. Same pattern as reconcileRetryBackoff.
func fastContentRefresh(t *testing.T) {
	t.Helper()
	debounce, jitter := contentRefreshDebounce, contentRefreshJitter
	contentRefreshDebounce, contentRefreshJitter = time.Millisecond, time.Millisecond
	t.Cleanup(func() { contentRefreshDebounce, contentRefreshJitter = debounce, jitter })
}

// refreshShard boots a RUNNING shard on refreshTestPack with hot reload wired over a MemBus, and returns
// the shard, its source, its bus and a cancel. Running (not the bare newReloadShard) because MintInstance
// requires a published run context.
func refreshShard(t *testing.T) (*Shard, *content.MemSource, *contentbus.MemBus, context.CancelFunc) {
	t.Helper()
	fastContentRefresh(t)
	src := content.NewMemSource()
	src.SetPack(refreshTestPack())
	src.SetContentVersion(1)
	bus := contentbus.NewMemBus()

	lc, err := content.Load(context.Background(), src, []string{"refreshtest"})
	if err != nil {
		t.Fatalf("boot load: %v", err)
	}
	sh := NewShardFromContent(lc, []string{"ct"}, "ct", "", nil, nil).
		WithHotReload(src, bus, []string{"refreshtest"}, 1)
	if sh.reloader == nil {
		t.Fatal("hot reload not enabled (reloader nil)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go sh.Run(ctx)
	waitCond(t, "shard Run to publish its run context", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && sh.runWG != nil
	})
	t.Cleanup(func() { cancel(); bus.Close() })
	return sh, src, bus, cancel
}

// publishPull replaces the pack in src and drives the FULL production pull shape over the bus: every
// per-ref invalidation, the trailing per-zone KindZone, then the version-complete sentinel. Using the real
// publisher rather than a hand-rolled invalidation is deliberate — the refresh hangs off signals only the
// publisher emits, and a hand-rolled one would let the test pass against a shard that never hears them.
func publishPull(t *testing.T, src *content.MemSource, bus contentbus.Bus, pk content.Pack, version uint64) {
	t.Helper()
	src.SetPack(pk)
	src.SetContentVersion(version)
	if _, err := contentbus.PublishPack(context.Background(), bus, pk, version); err != nil {
		t.Fatalf("publish pack: %v", err)
	}
	if err := contentbus.PublishVersionComplete(context.Background(), bus, version); err != nil {
		t.Fatalf("publish version-complete: %v", err)
	}
}

// waitForSnapshot blocks until the shard's live content snapshot satisfies cond. The refresh runs on its
// own goroutine off the bus handler, so every assertion about it is necessarily a poll.
func waitForSnapshot(t *testing.T, sh *Shard, what string, cond func(*content.LoadedContent) bool) {
	t.Helper()
	waitCond(t, what, func() bool { return cond(sh.liveContent()) })
}

// TestMintBuildsFromTheREFRESHEDSnapshotNotBootContent is #418's headline: a room ADDED by a content pull
// exists in an instance minted after that pull.
//
// Before the fix the mint read the boot snapshot, so the new room was simply absent from every copy — and
// silently so, because buildZone spawns whatever room set it is handed without complaint. The prototype for
// the new room was already in the live cache (the per-ref swap landed), which is what made the split so
// hard to see: the zone was half-current.
func TestMintBuildsFromTheREFRESHEDSnapshotNotBootContent(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)

	if inst, err := sh.MintInstance(context.Background(), "ct", "acct-1"); err != nil {
		t.Fatalf("precondition mint: %v", err)
	} else if got := len(inst.rooms); got != 1 {
		t.Fatalf("precondition: boot instance has %d rooms, want 1", got)
	}

	publishPull(t, src, bus, refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.", Exits: map[string]string{"north": "ct:room:vault"}},
		content.RoomDTO{Ref: "ct:room:vault", Name: "The Vault", Long: "A newly excavated vault.", Exits: map[string]string{}},
	), 2)

	waitForSnapshot(t, sh, "the content snapshot to pick up the new room", func(lc *content.LoadedContent) bool {
		zd := lc.Zone("ct")
		return zd != nil && len(zd.Rooms) == 2
	})

	inst, err := sh.MintInstance(context.Background(), "ct", "acct-2")
	if err != nil {
		t.Fatalf("mint after the pull: %v", err)
	}
	if _, ok := inst.rooms["ct:room:vault"]; !ok {
		t.Fatalf("the instance minted after the pull has rooms %v; it must contain the room the pull ADDED "+
			"(#418: the mint was building from boot content)", roomRefsOf(inst))
	}
}

// TestMintRefusesATemplateAReloadDELETED. The deletion case is the one a per-invalidation patch could never
// cover — PublishPack loops over the zones that are PRESENT, so a zone dropped from a pack emits no
// invalidation naming it. Only the trailing version-complete sentinel says "a pull finished", which is why
// the refresh hangs off it.
//
// Before the fix this minted happily from the deleted template, so a builder retiring a dungeon left every
// shard still handing out copies of it until the next rolling reboot.
func TestMintRefusesATemplateAReloadDELETED(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)

	if _, err := sh.MintInstance(context.Background(), "ct", "acct-1"); err != nil {
		t.Fatalf("precondition mint: %v", err)
	}

	// The pull ships the pack with the zone GONE. Nothing on the wire names `ct`.
	publishPull(t, src, bus, content.Pack{Pack: "refreshtest", Zones: []content.ZoneDTO{survivingZone()}}, 2)
	waitForSnapshot(t, sh, "the content snapshot to drop the deleted zone", func(lc *content.LoadedContent) bool {
		return lc != nil && lc.Zone("ct") == nil
	})

	_, err := sh.MintInstance(context.Background(), "ct", "acct-2")
	if err == nil {
		t.Fatal("minted an instance of a template the reload deleted; the mint must refuse it")
	}
	if !strings.Contains(err.Error(), "no such zone in loaded content") {
		t.Fatalf("refusal = %v, want the no-such-zone refusal", err)
	}
}

// TestMintRefusesAfterTheInstanceableOptInIsWithdrawn. `instanceable` is the #72 opt-in that bounds the
// mint's blast radius — without it a mint is an uncapped item faucet that routes around every in-world
// access gate. A builder who withdraws it is making a SECURITY decision, and before #418 that decision did
// not take effect on any running shard: the flag was read off boot content.
func TestMintRefusesAfterTheInstanceableOptInIsWithdrawn(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)

	if _, err := sh.MintInstance(context.Background(), "ct", "acct-1"); err != nil {
		t.Fatalf("precondition mint: %v", err)
	}

	pk := refreshTestPack()
	pk.Zones[0].Instanceable = false
	publishPull(t, src, bus, pk, 2)
	waitForSnapshot(t, sh, "the content snapshot to drop the instanceable opt-in", func(lc *content.LoadedContent) bool {
		zd := lc.Zone("ct")
		return zd != nil && !zd.Instanceable
	})

	_, err := sh.MintInstance(context.Background(), "ct", "acct-2")
	if err == nil {
		t.Fatal("minted an instance after the content withdrew the instanceable opt-in")
	}
	if !strings.Contains(err.Error(), "not declared instanceable") {
		t.Fatalf("refusal = %v, want the opt-in refusal", err)
	}
}

// TestALiveInstanceStaysPinnedAcrossAContentRefresh guards #411's reload FREEZE against this change.
//
// The refresh must move what the NEXT mint builds from and nothing else. A live instance is one bounded run
// over a fixed room graph with a party inside it; converging it would delete the room they are standing in
// mid-run. This test would fail if a future refactor ever "helpfully" re-applied the snapshot to live
// instances — which is exactly the kind of change that looks like a bug fix.
func TestALiveInstanceStaysPinnedAcrossAContentRefresh(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)

	inst, err := sh.MintInstance(context.Background(), "ct", "acct-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	before := roomRefsOf(inst)

	publishPull(t, src, bus, refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.", Exits: map[string]string{}},
		content.RoomDTO{Ref: "ct:room:vault", Name: "The Vault", Long: "A newly excavated vault.", Exits: map[string]string{}},
	), 2)
	waitForSnapshot(t, sh, "the content snapshot to pick up the new room", func(lc *content.LoadedContent) bool {
		zd := lc.Zone("ct")
		return zd != nil && len(zd.Rooms) == 2
	})

	// A fresh mint proves the refresh landed, so the pin below is a real assertion and not just a race the
	// test won by arriving early.
	fresh, err := sh.MintInstance(context.Background(), "ct", "acct-2")
	if err != nil {
		t.Fatalf("mint after the pull: %v", err)
	}
	if len(fresh.rooms) != 2 {
		t.Fatalf("the post-pull mint has %d rooms, want 2 — the refresh did not land, so the pin below proves nothing", len(fresh.rooms))
	}

	// Reading a live instance's room map from the test goroutine is safe HERE for the reason this test is
	// about: buildZone fills it before the actor starts, and the shape reconcile — the only other writer —
	// is frozen for an instance (#411). If that freeze ever goes away this read becomes a race, and the
	// assertion below is what should stop that change first.
	//
	// Compare the REFS, not the count: a reconcile that swapped one room for another would keep the
	// cardinality identical while corrupting the run of whoever is inside.
	after := roomRefsOf(inst)
	sort.Strings(before)
	sort.Strings(after)
	if !slices.Equal(before, after) {
		t.Fatalf("the LIVE instance's rooms moved from %v to %v across a content refresh; a live instance "+
			"is PINNED to the content it was minted from (#411)", before, after)
	}
}

// TestHostZoneBuildsFromTheRefreshedSnapshot. The same staleness lived on the rebalance path, where it was
// rarer (once per rebalance rather than once per dungeon run) but had the identical shape: a standby
// adopting a drained peer's zone rebuilt it from ITS boot content, so a rebalance could silently roll a
// zone's room graph back to whatever version that process started with.
func TestHostZoneBuildsFromTheRefreshedSnapshot(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)

	publishPull(t, src, bus, refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.", Exits: map[string]string{}},
		content.RoomDTO{Ref: "ct:room:vault", Name: "The Vault", Long: "A newly excavated vault.", Exits: map[string]string{}},
	), 2)
	waitForSnapshot(t, sh, "the content snapshot to pick up the new room", func(lc *content.LoadedContent) bool {
		zd := lc.Zone("ct")
		return zd != nil && len(zd.Rooms) == 2
	})

	// A zone this shard does NOT host at boot, built at runtime — the standby-adoption shape. `ct` is
	// already hosted (HostZone would take its idempotent early return), so build the pull's own new zone.
	pk := refreshTestPack()
	pk.Zones = append(pk.Zones, content.ZoneDTO{
		Ref: "ct2", Name: "Second Zone", StartRoom: "ct2:room:hall",
		Rooms: []content.RoomDTO{{Ref: "ct2:room:hall", Name: "Hall", Long: "A hall.", Exits: map[string]string{}}},
	})
	publishPull(t, src, bus, pk, 3)
	waitForSnapshot(t, sh, "the content snapshot to pick up the new zone", func(lc *content.LoadedContent) bool {
		return lc != nil && lc.Zone("ct2") != nil
	})

	z, err := sh.HostZone(context.Background(), "ct2")
	if err != nil {
		t.Fatalf("HostZone of a zone the pull ADDED: %v", err)
	}
	if _, ok := z.rooms["ct2:room:hall"]; !ok {
		t.Fatalf("HostZone built %q with rooms %v; it must build from the LIVE snapshot, not boot content", "ct2", roomRefsOf(z))
	}
}

// TestContentRefreshKeepsThePreviousSnapshotOnAReadFailure. Serving slightly stale content is the bug being
// fixed; serving NO content would break HostZone and every mint outright. So a Postgres blip during the
// re-read must leave the previous snapshot published, not clear it.
func TestContentRefreshKeepsThePreviousSnapshotOnAReadFailure(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(refreshTestPack())
	lc, err := content.Load(context.Background(), src, []string{"refreshtest"})
	if err != nil {
		t.Fatal(err)
	}
	bus := contentbus.NewMemBus()
	defer bus.Close()
	failing := &failingSource{MemSource: src}
	sh := NewShardFromContent(lc, []string{"ct"}, "ct", "", nil, nil).
		WithHotReload(failing, bus, []string{"refreshtest"}, 1)
	if sh.reloader == nil {
		t.Fatal("reloader nil")
	}

	failing.fail.Store(true)
	sh.reloader.refreshContentSnapshot()

	if n := failing.attempts.Load(); n == 0 {
		t.Fatal("the refresh never attempted the read at all — this test's assertions below would pass for a " +
			"refresh that silently does nothing, which is exactly what a source decorator dropping LoadPacks " +
			"would produce")
	}
	got := sh.liveContent()
	if got == nil || got.Zone("ct") == nil {
		t.Fatal("a failed re-read cleared the content snapshot; it must keep the previous one")
	}
	if err := validateMintTemplate(got, "ct"); err != nil {
		t.Fatalf("the retained snapshot no longer validates a mint: %v", err)
	}
}

// TestContentRefreshKeepsThePreviousSnapshotOnAnEmptyRead. LoadPacks matches `pack = ANY($1)` and returns no
// rows and NO ERROR for a pack that has been pruned or renamed, so an empty result is indistinguishable from
// a successful read unless it is checked for. Publishing it would empty the snapshot, and an empty snapshot
// makes every runtime zone build refuse — a content-side accident escalating into a fleet-wide outage.
func TestContentRefreshKeepsThePreviousSnapshotOnAnEmptyRead(t *testing.T) {
	fastContentRefresh(t)
	src := content.NewMemSource()
	src.SetPack(refreshTestPack())
	lc, err := content.Load(context.Background(), src, []string{"refreshtest"})
	if err != nil {
		t.Fatal(err)
	}
	bus := contentbus.NewMemBus()
	defer bus.Close()
	sh := NewShardFromContent(lc, []string{"ct"}, "ct", "", nil, nil).
		WithHotReload(src, bus, []string{"refreshtest"}, 1)
	if sh.reloader == nil {
		t.Fatal("reloader nil")
	}

	// The pack vanishes from the source entirely — no rows, no error.
	src.SetPack(content.Pack{Pack: "refreshtest"})
	sh.reloader.refreshContentSnapshot()

	got := sh.liveContent()
	if got.Zone("ct") == nil {
		t.Fatal("a read returning zero real zones replaced a good snapshot; that makes every runtime zone " +
			"build refuse, so the previous snapshot must be kept instead")
	}
	// The guard must count NON-CORE zones. LoadWithCore layers the embedded bootstrap pack under every read,
	// so a snapshot that lost every authored zone still reports one and a plain Empty() check would be dead
	// code in production while passing a test that skipped the core layering.
	if realZones(got) == 0 {
		t.Fatal("the retained snapshot has no real zones")
	}
}

// TestContentRefreshConvergesOnAMarkThatLandsMidRead is the lost-wakeup test, and it is deterministic.
//
// The naive single-flight — claim a slot, do the work, release the slot — silently drops a mark that lands
// between the worker's last check and its release. The snapshot then sits stale until the NEXT content
// deploy, which may be days later, and nothing anywhere reports it. That window is microseconds wide in
// production, so a test that just fires marks in a loop and hopes cannot pin it: I verified that a version
// of this test without the gate below stays green against the broken implementation.
//
// So the read is GATED. The sequence forces the exact interleaving the re-check exists for: mark, block
// inside the re-read, mutate the content and mark AGAIN while the worker is provably mid-read, release.
// A worker that does not re-check after releasing its slot never sees the second mark and never converges.
func TestContentRefreshConvergesOnAMarkThatLandsMidRead(t *testing.T) {
	fastContentRefresh(t)
	src := content.NewMemSource()
	src.SetPack(refreshTestPack())
	lc, err := content.Load(context.Background(), src, []string{"refreshtest"})
	if err != nil {
		t.Fatal(err)
	}
	bus := contentbus.NewMemBus()
	defer bus.Close()
	gated := &gatedSource{MemSource: src, entered: make(chan struct{}, 8), release: make(chan struct{})}
	sh := NewShardFromContent(lc, []string{"ct"}, "ct", "", nil, nil).
		WithHotReload(gated, bus, []string{"refreshtest"}, 1)
	if sh.reloader == nil {
		t.Fatal("reloader nil")
	}

	sh.reloader.markContentStale()
	select { // wait until the worker is provably INSIDE the read
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh never entered the content read")
	}

	// The mark that must not be lost: published while the worker holds the in-flight slot.
	gated.SetPack(refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.", Exits: map[string]string{}},
		content.RoomDTO{Ref: "ct:room:vault", Name: "The Vault", Long: "A newly excavated vault.", Exits: map[string]string{}},
	))
	sh.reloader.markContentStale()
	close(gated.release)

	waitForSnapshot(t, sh, "the snapshot to converge on the mark that landed mid-read", func(lc *content.LoadedContent) bool {
		zd := lc.Zone("ct")
		return zd != nil && len(zd.Rooms) == 2
	})
}

// TestContentRefreshCoalescesAStorm. A pull of a 40-zone pack marks once per zone, and the re-read
// materializes the entire deployed world from Postgres. Re-reading per mark would turn every routine
// content deploy into a load event on the one database the whole fleet shares — multiplied by shard count.
// The bound here is tight (not `< marks`) because the mechanism should produce a small constant.
func TestContentRefreshCoalescesAStorm(t *testing.T) {
	fastContentRefresh(t)
	src := content.NewMemSource()
	src.SetPack(refreshTestPack())
	lc, err := content.Load(context.Background(), src, []string{"refreshtest"})
	if err != nil {
		t.Fatal(err)
	}
	bus := contentbus.NewMemBus()
	defer bus.Close()
	// No version authority: this source is a bare content.Source, so the version gate cannot short-circuit
	// the reads and the count below measures the MARKER's coalescing rather than the gate's deduplication.
	counting := &countingSource{Source: src}
	sh := NewShardFromContent(lc, []string{"ct"}, "ct", "", nil, nil).
		WithHotReload(counting, bus, []string{"refreshtest"}, 1)
	if sh.reloader == nil {
		t.Fatal("reloader nil")
	}

	const marks = 50
	for i := 0; i < marks; i++ {
		sh.reloader.markContentStale()
	}
	counting.SetPack(refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.", Exits: map[string]string{}},
		content.RoomDTO{Ref: "ct:room:vault", Name: "The Vault", Long: "A newly excavated vault.", Exits: map[string]string{}},
	))
	sh.reloader.markContentStale()

	waitForSnapshot(t, sh, "the snapshot to converge on the LAST marked state", func(lc *content.LoadedContent) bool {
		zd := lc.Zone("ct")
		return zd != nil && len(zd.Rooms) == 2
	})
	// The debounce absorbs the storm into one pass; the trailing mark can legitimately buy a second. Anything
	// beyond a small constant means the marks are no longer coalescing.
	if n := counting.loads.Load(); n > 4 {
		t.Fatalf("%d marks produced %d whole-pack re-reads; the marker must COALESCE a storm into a small "+
			"constant, not re-read per mark", marks+1, n)
	}
}

// TestContentRefreshSkipsTheReadWhenTheVersionHasNotMoved. The content bus carries unsigned JSON, and the
// pack filter accepts an empty pack from anybody. Without a version gate, a single small forged or replayed
// invalidation would drive every shard in the fleet into back-to-back full-content reads against the
// content database, indefinitely — the refresh turned a cheap message into an expensive one. The gate makes
// a replay inert: the shard checks the version authority first and does nothing when it has not moved.
func TestContentRefreshSkipsTheReadWhenTheVersionHasNotMoved(t *testing.T) {
	fastContentRefresh(t)
	src := content.NewMemSource()
	src.SetPack(refreshTestPack())
	src.SetContentVersion(4)
	lc, err := content.Load(context.Background(), src, []string{"refreshtest"})
	if err != nil {
		t.Fatal(err)
	}
	bus := contentbus.NewMemBus()
	defer bus.Close()
	counting := &countingSource{Source: src, versioner: src}
	sh := NewShardFromContent(lc, []string{"ct"}, "ct", "", nil, nil).
		WithHotReload(counting, bus, []string{"refreshtest"}, 4) // booted AT version 4
	if sh.reloader == nil {
		t.Fatal("reloader nil")
	}

	// A replayed sentinel, ten times over. The version authority still says 4.
	for i := 0; i < 10; i++ {
		if err := bus.Publish(context.Background(), contentbus.Invalidation{Kind: content.KindVersionComplete, Version: 4}); err != nil {
			t.Fatal(err)
		}
	}
	waitCond(t, "the refresh worker to run and decide", func() bool {
		return !sh.reloader.contentRefreshInFlight.Load() && !sh.reloader.contentStale.Load()
	})
	if n := counting.loads.Load(); n != 0 {
		t.Fatalf("a replayed invalidation caused %d whole-content re-reads; the version gate must make it inert", n)
	}

	// A REAL deploy still gets through — the gate must not be a permanent off switch.
	counting.SetPack(refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.", Exits: map[string]string{}},
		content.RoomDTO{Ref: "ct:room:vault", Name: "The Vault", Long: "A newly excavated vault.", Exits: map[string]string{}},
	))
	src.SetContentVersion(5)
	if err := bus.Publish(context.Background(), contentbus.Invalidation{Kind: content.KindVersionComplete, Version: 5}); err != nil {
		t.Fatal(err)
	}
	waitForSnapshot(t, sh, "a genuine version bump to refresh the snapshot", func(lc *content.LoadedContent) bool {
		zd := lc.Zone("ct")
		return zd != nil && len(zd.Rooms) == 2
	})
}

// TestRuntimeZoneBuildsRefuseAnIncompleteBuild covers the state #418 newly made reachable: the snapshot and
// the prototype cache converge on independent paths, so a snapshot can name a room whose prototype has not
// swapped in yet.
//
// Both runtime builders must REFUSE rather than publish the half-built zone, and the reason is the same for
// each: the zone is already somebody's destination by the time it is built. A roomless instance disconnects
// the entrant mid-entry, after transferOut has released them. A roomless HostZone adopts, arms lease
// renewal, and then drops every player a drain hands over — while the directory reports a healthy claimed
// zone that nothing self-heals. Refusing leaves a retryable failure instead of a durable black hole.
func TestRuntimeZoneBuildsRefuseAnIncompleteBuild(t *testing.T) {
	sh, _, _, _ := refreshShard(t)

	// A snapshot naming a room the prototype cache has never heard of — exactly the mid-pull window.
	stale := sh.liveContent()
	ahead, err := content.Load(context.Background(), func() content.Source {
		s := content.NewMemSource()
		pk := refreshTestPack(
			content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.", Exits: map[string]string{}},
			content.RoomDTO{Ref: "ct:room:ghost", Name: "The Ghost", Long: "A room with no prototype.", Exits: map[string]string{}},
		)
		pk.Zones = append(pk.Zones, content.ZoneDTO{
			Ref: "ct3", Name: "Third", StartRoom: "ct3:room:ghost",
			Rooms: []content.RoomDTO{{Ref: "ct3:room:ghost", Name: "Ghost", Long: "No prototype.", Exits: map[string]string{}}},
		})
		s.SetPack(pk)
		return s
	}(), []string{"refreshtest"})
	if err != nil {
		t.Fatal(err)
	}
	sh.setContent(ahead)
	t.Cleanup(func() { sh.setContent(stale) })

	if _, err := sh.MintInstance(context.Background(), "ct", "acct-1"); err == nil {
		t.Fatal("minted an instance whose snapshot names a room with no prototype; the mint must refuse an " +
			"incomplete build rather than hand a player a zone that will drop them")
	}
	if _, err := sh.HostZone(context.Background(), "ct3"); err == nil {
		t.Fatal("HostZone adopted a zone whose snapshot names a room with no prototype; it must refuse rather " +
			"than claim a lease on a zone that disconnects everyone handed into it")
	}
	if z := sh.zoneByID("ct3"); z != nil {
		t.Fatal("the refused zone was published into s.zones anyway")
	}
}

// TestHostZoneRefusesAZoneMissingFromTheSnapshot is the deletion half of the same asymmetry. Before #418
// the snapshot was frozen at boot, so it always contained every zone this shard could be asked to build and
// this branch was unreachable. Now a builder retiring a zone makes it reachable on the very next rebalance.
func TestHostZoneRefusesAZoneMissingFromTheSnapshot(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)

	publishPull(t, src, bus, content.Pack{Pack: "refreshtest", Zones: []content.ZoneDTO{survivingZone()}}, 2) // `ct` retired
	waitForSnapshot(t, sh, "the content snapshot to drop the deleted zone", func(lc *content.LoadedContent) bool {
		return lc != nil && lc.Zone("ct") == nil
	})

	if _, err := sh.HostZone(context.Background(), "ct-gone"); err == nil {
		t.Fatal("HostZone built and adopted a zone absent from the content snapshot; it must refuse, so the " +
			"zone stays unowned and re-claimable rather than becoming a leased black hole")
	}
}

// TestVersionCompleteSentinelAndZoneInvalidationBothMarkTheSnapshot pins the two wire signals the refresh
// hangs off, independently of what any one of them happens to change.
//
// They are BOTH needed and neither is redundant: a shard-local staff `reload` emits per-ref invalidations
// and a trailing KindZone but NO sentinel, while a zone DELETED by a coordinated pull emits no KindZone at
// all and is visible only on the sentinel. Drop either hook and one real deployment path silently stops
// converging.
func TestVersionCompleteSentinelAndZoneInvalidationBothMarkTheSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		inv  contentbus.Invalidation
	}{
		{
			"version-complete sentinel (a coordinated pull; the only signal a DELETION reaches us on)",
			contentbus.Invalidation{Kind: content.KindVersionComplete, Version: 7},
		},
		{
			"zone-shape invalidation (a shard-local staff reload, which emits no sentinel)",
			contentbus.Invalidation{Kind: content.KindZone, Ref: "ct", Pack: "refreshtest", Version: 7, Rooms: []string{"ct:room:entry"}, StartRoom: "ct:room:entry"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := content.NewMemSource()
			src.SetPack(refreshTestPack())
			lc, err := content.Load(context.Background(), src, []string{"refreshtest"})
			if err != nil {
				t.Fatal(err)
			}
			bus := contentbus.NewMemBus()
			defer bus.Close()
			sh := NewShardFromContent(lc, []string{"ct"}, "ct", "", nil, nil).
				WithHotReload(src, bus, []string{"refreshtest"}, 1)
			if sh.reloader == nil {
				t.Fatal("reloader nil")
			}

			src.SetPack(refreshTestPack(
				content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.", Exits: map[string]string{}},
				content.RoomDTO{Ref: "ct:room:vault", Name: "The Vault", Long: "A newly excavated vault.", Exits: map[string]string{}},
			))
			if err := bus.Publish(context.Background(), tc.inv); err != nil {
				t.Fatal(err)
			}

			waitForSnapshot(t, sh, "the snapshot to refresh off this invalidation alone", func(lc *content.LoadedContent) bool {
				zd := lc.Zone("ct")
				return zd != nil && len(zd.Rooms) == 2
			})
		})
	}
}

// roomRefsOf lists a zone's live room refs for a failure message. See the note in the pinning test for why
// reading the map off the actor is safe for the zones these tests build.
func roomRefsOf(z *Zone) []string {
	out := make([]string, 0, len(z.rooms))
	for ref := range z.rooms {
		out = append(out, string(ref))
	}
	return out
}

// failingSource is a content.Source whose whole-pack read can be switched to an error, to prove the refresh
// degrades to "keep the previous snapshot" rather than clearing it. It COUNTS the attempts so the test can
// tell a genuine keep-the-previous from a refresh that never ran at all — the latter is what a source
// decorator dropping LoadPacks would produce, and it would otherwise pass the same assertions.
type failingSource struct {
	*content.MemSource
	fail     atomic.Bool
	attempts atomic.Int64
}

func (f *failingSource) LoadPacks(ctx context.Context, enabled []string) ([]content.Pack, error) {
	f.attempts.Add(1)
	if f.fail.Load() {
		return nil, errors.New("simulated content read failure")
	}
	return f.MemSource.LoadPacks(ctx, enabled)
}

// countingSource counts whole-pack reads. It embeds the Source INTERFACE rather than *MemSource so a test
// can choose whether the source also carries a version authority: with one, the refresh's version gate
// short-circuits the read, which is the wrong thing to measure when the subject is the marker's coalescing.
type countingSource struct {
	content.Source
	versioner interface {
		ContentVersion(context.Context) (uint64, error)
	}
	loads atomic.Int64
}

func (c *countingSource) LoadPacks(ctx context.Context, enabled []string) ([]content.Pack, error) {
	c.loads.Add(1)
	return c.Source.LoadPacks(ctx, enabled)
}

// SetPack forwards to the underlying MemSource so a test can mutate content through the wrapper.
func (c *countingSource) SetPack(p content.Pack) { c.Source.(*content.MemSource).SetPack(p) }

// LoadDefinition forwards the single-ref re-read, so a countingSource is a full content.DefinitionSource
// (what WithHotReload takes) and not only a content.Source.
func (c *countingSource) LoadDefinition(ctx context.Context, kind, ref, pack string) (content.Definition, error) {
	return c.Source.(*content.MemSource).LoadDefinition(ctx, kind, ref, pack)
}

// ContentVersion is present only when the test supplied a versioner, so a countingSource without one does
// NOT satisfy the reloader's contentVersioner and the version gate stays out of the way.
func (c *countingSource) ContentVersion(ctx context.Context) (uint64, error) {
	if c.versioner == nil {
		return 0, errors.New("no version authority")
	}
	return c.versioner.ContentVersion(ctx)
}

// gatedSource blocks inside the whole-pack read until the test releases it, so a test can force a mark to
// land while a refresh is provably in flight. A channel, not a sleep: the interleaving the lost-wakeup
// re-check exists for is microseconds wide, and a timing-based test of it is a coin flip that reports
// "pass" for a broken implementation most of the time.
type gatedSource struct {
	*content.MemSource
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedSource) LoadPacks(ctx context.Context, enabled []string) ([]content.Pack, error) {
	// Only the FIRST read is gated; the re-read the dropped mark should trigger must run to completion.
	g.once.Do(func() {
		g.entered <- struct{}{}
		<-g.release
	})
	return g.MemSource.LoadPacks(ctx, enabled)
}
