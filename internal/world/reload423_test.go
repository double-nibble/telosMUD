package world

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// reload423_test.go — #423: the content-snapshot refresh must VALIDATE before it publishes.
//
// The hole: #418 made a shard re-read all its enabled packs from Postgres on any content event and publish
// the result as the live snapshot, with nothing checking it. Meanwhile the staff `reload` command's
// identical publish WAS gated by validatePacks. Rows are written by seed/import BEFORE any reload runs, so
// a reload the operator watched get REJECTED ("nothing propagated") went live anyway the moment any
// unrelated invalidation — another builder's reload, a bus reconnect, a version-complete sentinel — marked
// the snapshot stale. "Nothing propagated" stopped meaning "nothing applied".
//
// These tests pin both directions: broken content does not become the live snapshot, and the version cursor
// does not advance past a rejection (which would otherwise lock the rejection in as "already current" and
// make the shard stop even trying).

// brokenRefreshPack is refreshTestPack with a DANGLING EXIT — a room pointing at a ref that exists nowhere
// in the merged graph. validateRoomExits rejects exactly this, and it is a realistic authoring slip rather
// than a synthetic tripwire, so the test exercises the gate the way a bad deploy would.
func brokenRefreshPack() content.Pack {
	return refreshTestPack(content.RoomDTO{
		Ref: "ct:room:entry", Name: "The Entry", Long: "A plain stone entry.",
		Exits: map[string]string{"north": "ct:room:does-not-exist"},
	})
}

// TestRefreshRejectsBrokenContentAndKeepsThePreviousSnapshot is the headline. A pull deploys rows that fail
// validation; the shard must keep serving the last good snapshot rather than publish them.
func TestRefreshRejectsBrokenContentAndKeepsThePreviousSnapshot(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)
	before := sh.liveContent()
	require.NotNil(t, before.Zone("ct"), "fixture precondition: the good snapshot has zone ct")

	publishPull(t, src, bus, brokenRefreshPack(), 2)

	// The refresh runs on its own goroutine; wait for it to have RUN and rejected rather than to have not
	// happened yet. The rejection memo is the observable that distinguishes those two states.
	waitCond(t, "the refresh to run and reject the broken content", func() bool {
		return sh.reloader.rejectedContentVersion.Load() == 2
	})

	got := sh.liveContent()
	require.Same(t, before, got, "a rejected refresh must leave the PREVIOUS snapshot published, untouched")
	require.Empty(t, got.Zone("ct").Rooms[0].Exits, "the broken exit must not be live")

	// And the version cursor must NOT have moved. If a rejection advanced it, the version gate would skip
	// every later refresh of that version as "already current" — the shard would stop re-reading, and an
	// operator's fix landing at the same version would never be picked up.
	require.NotEqual(t, uint64(2), sh.reloader.snapshotContentVersion.Load(),
		"a rejection must not advance snapshotContentVersion")
}

// TestARejectedReloadDoesNotGoLiveOnAnUnrelatedInvalidation is the issue's scenario 1, end to end: the
// operator sees REJECTED, and it stays rejected even when an unrelated content event fires later.
//
// This is the test that would have caught the bug. Pre-#423 the `reload` gate and the refresh disagreed,
// so the rejection was real at publish time and meaningless thirty seconds later.
func TestARejectedReloadDoesNotGoLiveOnAnUnrelatedInvalidation(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)
	before := sh.liveContent()

	// The builder stages broken rows and reloads. The #192 gate rejects: nothing propagates.
	src.SetPack(brokenRefreshPack())
	src.SetContentVersion(2)
	out := sh.reloader.republish(context.Background(), []string{"refreshtest"}, false)
	require.NotEmpty(t, out.rejected, "precondition: the staff reload gate must reject this content")
	require.Zero(t, out.published, "a rejected reload publishes nothing")

	// Now ANY unrelated content event marks the snapshot stale and drives a re-read of those same rows.
	// Before #423 this is where the rejected content silently went live on every shard.
	require.NoError(t, contentbus.PublishVersionComplete(context.Background(), bus, 2))

	waitCond(t, "the refresh to run and reject the staged rows", func() bool {
		return sh.reloader.rejectedContentVersion.Load() == 2
	})
	require.Same(t, before, sh.liveContent(),
		"content a reload REJECTED must not reach the live snapshot via an unrelated invalidation")
}

// TestRefreshValidatesTheSAMEReadItPublishes is the mutation guard against the obvious wrong fix: validate
// one read, publish a second. That regression leaves every other test in this file green while
// reintroducing a TOCTOU — an import committing between the two reads deploys rows the gate never saw.
//
// One refresh must cost exactly ONE LoadPacks.
func TestRefreshValidatesTheSAMEReadItPublishes(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)
	// The versioner is wired through deliberately: without it the refresh has no version gate and re-reads
	// unconditionally, which would make the count measure the gate rather than the read/publish split.
	counted := &countingSource{Source: src, versioner: src}
	sh.reloader.src = counted

	// A good pull, so the refresh runs all the way through validate AND publish — the path where a second
	// read would be introduced. (On the rejection path a double read is unobservable.)
	added := refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "Entry.", Exits: map[string]string{"north": "ct:room:new"}},
		content.RoomDTO{Ref: "ct:room:new", Name: "New Room", Long: "New.", Exits: map[string]string{"south": "ct:room:entry"}},
	)
	publishPull(t, src, bus, added, 2)

	waitForSnapshot(t, sh, "the new room to reach the snapshot", func(lc *content.LoadedContent) bool {
		z := lc.Zone("ct")
		return z != nil && len(z.Rooms) == 2
	})
	require.EqualValues(t, 1, counted.loads.Load(),
		"a refresh must read the packs ONCE and validate the same slice it merges; a second read is a TOCTOU")
}

// TestRefreshStillPublishesWithCoreLayered guards the scope decision. The embedded core pack is layered
// under every load but is deliberately NOT in the validation scope: if it were, any core-namespace finding
// would reject every refresh on every shard forever, with no row an operator could edit to fix it.
func TestRefreshStillPublishesWithCoreLayered(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)
	scope := sh.reloader.snapshotScope()
	require.False(t, scope[content.CorePack], "the embedded core pack must never be in the validation scope")
	require.True(t, scope["refreshtest"], "every enabled real pack must be in scope")

	added := refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "Entry.", Exits: map[string]string{}},
		content.RoomDTO{Ref: "ct:room:new", Name: "New Room", Long: "New.", Exits: map[string]string{}},
	)
	publishPull(t, src, bus, added, 2)
	waitForSnapshot(t, sh, "a valid refresh to publish with core layered in", func(lc *content.LoadedContent) bool {
		z := lc.Zone("ct")
		return z != nil && len(z.Rooms) == 2
	})
}

// TestRefreshRecoversOnceTheBrokenRowsAreFixed proves the freeze is not a wedge. The rejection is a memo,
// never a gate: the shard keeps re-reading, so the operator's fix converges with no restart. Without this,
// "keep the previous snapshot" could quietly mean "keep it forever".
func TestRefreshRecoversOnceTheBrokenRowsAreFixed(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)

	publishPull(t, src, bus, brokenRefreshPack(), 2)
	waitCond(t, "the broken content to be rejected", func() bool {
		return sh.reloader.rejectedContentVersion.Load() == 2
	})

	fixed := refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "Entry.", Exits: map[string]string{"north": "ct:room:north"}},
		content.RoomDTO{Ref: "ct:room:north", Name: "North Room", Long: "North.", Exits: map[string]string{}},
	)
	publishPull(t, src, bus, fixed, 3)
	waitForSnapshot(t, sh, "the fixed content to publish", func(lc *content.LoadedContent) bool {
		z := lc.Zone("ct")
		return z != nil && len(z.Rooms) == 2
	})
	require.EqualValues(t, 3, sh.reloader.snapshotContentVersion.Load(),
		"the cursor advances once content actually publishes")
}

// TestBootReportsButDoesNotRefuseBrokenContent locks the deliberate asymmetry between the two gates.
//
// The runtime refresh refuses because it HAS a known-good previous snapshot to keep serving. Boot has none,
// so refusing there would turn a content defect into an outage — contradicting the whole reason the
// embedded core pack exists. Boot's job is to make the runtime freeze VISIBLE, not to add a second veto. A
// later "make these consistent" refactor that turns boot into a hard gate must fail here.
func TestBootReportsButDoesNotRefuseBrokenContent(t *testing.T) {
	packs := []content.Pack{brokenRefreshPack()}
	problems := ReportBootContentProblems(packs, []string{"refreshtest"})
	require.NotEmpty(t, problems, "boot must SEE the same problems the runtime gate rejects on")

	// ...and the content still builds a world. This is the "does not refuse" half.
	lc := content.Merge(packs)
	require.NotNil(t, lc.Zone("ct"), "boot must still assemble the content it reported problems about")
}

// TestReportBootContentProblemsIsSilentOnGoodContent keeps the boot Error meaningful — an alert that fires
// on every healthy boot is an alert nobody reads.
func TestReportBootContentProblemsIsSilentOnGoodContent(t *testing.T) {
	require.Empty(t, ReportBootContentProblems([]content.Pack{refreshTestPack()}, []string{"refreshtest"}))
}

// --- The NARROWED rejectable set -------------------------------------------------------------------
//
// The snapshot gate rejects only findings that can make the published snapshot unsafe to BUILD A ZONE
// FROM. The pack-global kinds (attributes, channels, trust ladder) are demoted to warnings there, because
// a snapshot refresh cannot deploy them at all: they are registered by defineGlobals at boot only, or
// hot-swap through their own path. Rejecting on them would be pure veto surface — no protective value, and
// the power to freeze every pack's zone graph fleet-wide over one pack's rows.

// trustLadderRejectPack is refreshTestPack plus a trust ladder that validatePacks HARD-REJECTS: a baseline
// tier granting a capability elevates every un-elevated account. It is the sharpest case for the narrowing,
// because it is the most security-flavored check in the gate — and the one with the least business being
// there, since a ladder only ever reaches a running world through a RESTART.
func trustLadderRejectPack() content.Pack {
	pk := refreshTestPack()
	pk.TrustTiers = []content.TrustTierDTO{
		{Name: "player", Rank: 0, Flags: []string{"admin"}},
		{Name: "admin", Rank: 1},
	}
	return pk
}

// TestSnapshotGateDoesNotRejectOnASharedDefFinding is the narrowing's headline. The content is genuinely
// bad and validatePacks says so — but the badness cannot ride the snapshot, so the snapshot publishes.
func TestSnapshotGateDoesNotRejectOnASharedDefFinding(t *testing.T) {
	pk := trustLadderRejectPack()
	scope := map[string]bool{"refreshtest": true}
	require.NotEmpty(t, validatePacks([]content.Pack{pk}, scope),
		"precondition: the FULL gate must still consider this content broken")

	reject, warn := validateSnapshotPacks([]content.Pack{pk}, scope)
	require.Empty(t, reject, "a trust-ladder finding must not veto the content snapshot")
	require.NotEmpty(t, warn, "...but it must still be surfaced, not silently dropped")
}

// TestSnapshotGateStillRejectsAZoneGraphFinding is the other half: the narrowing must not have gutted the
// gate. A dangling exit is a zone-graph defect and still blocks the publish.
func TestSnapshotGateStillRejectsAZoneGraphFinding(t *testing.T) {
	reject, _ := validateSnapshotPacks([]content.Pack{brokenRefreshPack()}, map[string]bool{"refreshtest": true})
	require.NotEmpty(t, reject, "a dangling exit is a zone-graph defect and must still veto the snapshot")
}

// TestSnapshotGatePartitionsValidatePacksExactly is the structural guard. Every finding validatePacks
// produces must land in exactly one of the two buckets — so a check added later cannot silently fall into
// NEITHER (invisible to the snapshot gate AND unreported) or BOTH (double-counted).
//
// This is what makes the split maintainable rather than a snapshot of today's opinion.
func TestSnapshotGatePartitionsValidatePacksExactly(t *testing.T) {
	// Content broken in both directions at once, so both buckets are non-empty.
	pk := trustLadderRejectPack()
	pk.Zones[0].Rooms[0].Exits = map[string]string{"north": "ct:room:does-not-exist"}
	scope := map[string]bool{"refreshtest": true}

	full := validatePacks([]content.Pack{pk}, scope)
	reject, warn := validateSnapshotPacks([]content.Pack{pk}, scope)
	require.NotEmpty(t, reject)
	require.NotEmpty(t, warn)
	require.ElementsMatch(t, full, append(append([]string{}, reject...), warn...),
		"validateSnapshotPacks must PARTITION validatePacks: every finding in exactly one bucket, none "+
			"invented, none lost. A newly added check must be consciously classified.")
}

// TestReloadStillRejectsWhatTheSnapshotGateOnlyWarnsAbout pins the deliberate asymmetry between the two
// gates. A staff `reload` is a human deliberately propagating an edit and can afford to be refused; the
// refresh is an automatic reaction to somebody else's content event and cannot. A later "make these
// consistent" refactor must fail here.
func TestReloadStillRejectsWhatTheSnapshotGateOnlyWarnsAbout(t *testing.T) {
	sh, src, _, _ := refreshShard(t)
	src.SetPack(trustLadderRejectPack())
	src.SetContentVersion(2)

	out := sh.reloader.republish(context.Background(), []string{"refreshtest"}, false)
	require.NotEmpty(t, out.rejected, "the staff reload gate keeps FULL strictness")
	require.Zero(t, out.published)
	require.NotEmpty(t, ReportBootContentProblems([]content.Pack{trustLadderRejectPack()}, []string{"refreshtest"}),
		"and boot still REPORTS the full finding set, so detection is not reduced by the narrowing")
}

// TestRejectedVersionIsNotRereadDuringItsCooldown covers the read-amplification bound. A rejection must not
// advance snapshotContentVersion (or the shard would stop retrying and an operator's fix would never land),
// but that leaves the version gate permanently open — so while rows are broken, every content event would
// otherwise cost every shard a full read of the content database. That is the exact lever the version gate
// exists to deny, and an attacker can create the precondition with the same one-row write.
func TestRejectedVersionIsNotRereadDuringItsCooldown(t *testing.T) {
	sh, src, bus, _ := refreshShard(t)
	counted := &countingSource{Source: src, versioner: src}
	sh.reloader.src = counted

	publishPull(t, src, bus, brokenRefreshPack(), 2)
	waitCond(t, "the broken content to be rejected", func() bool {
		return sh.reloader.rejectedContentVersion.Load() == 2
	})
	afterFirst := counted.loads.Load()
	require.EqualValues(t, 1, afterFirst, "the rejection itself costs exactly one read")

	// Ten more content events at the same (still broken) version must NOT each buy a full re-read.
	for i := 0; i < 10; i++ {
		require.NoError(t, contentbus.PublishVersionComplete(context.Background(), bus, 2))
	}
	settleRefresh(t, sh)
	require.EqualValues(t, afterFirst, counted.loads.Load(),
		"a version already rejected must not be re-read again inside its cooldown — otherwise one broken "+
			"row is an unbounded read multiplier against the content database, fleet-wide")
}

// TestRejectionLogMemoIsKeyedOnTheProblemSetNotTheVersion. A raw row edit does NOT bump content_version
// (only an import or a staff reload does), so a version-keyed memo reports the FIRST breakage at Error and
// silently downgrades every later, DIFFERENT breakage at that version to Debug. That is usable to hide a
// real problem behind a benign one.
func TestRejectionLogMemoIsKeyedOnTheProblemSetNotTheVersion(t *testing.T) {
	a := rejectionKey(2, []string{"problem A"})
	require.Equal(t, a, rejectionKey(2, []string{"problem A"}), "the same problem set at the same version memoizes")
	require.NotEqual(t, a, rejectionKey(2, []string{"problem B"}),
		"a DIFFERENT problem set at the SAME version must NOT be suppressed as a repeat")
	require.NotEqual(t, a, rejectionKey(3, []string{"problem A"}), "the version still participates")
	require.Equal(t, rejectionKey(2, []string{"A", "B"}), rejectionKey(2, []string{"B", "A"}),
		"order-independent, so a map-iteration reshuffle in a validator cannot masquerade as a new problem")
}

// settleRefresh waits for any in-flight snapshot refresh to finish, so a "no further reads happened"
// assertion is meaningful rather than merely early.
func settleRefresh(t *testing.T, sh *Shard) {
	t.Helper()
	require.Eventually(t, func() bool {
		return !sh.reloader.contentRefreshInFlight.Load() && !sh.reloader.contentStale.Load()
	}, 5*time.Second, time.Millisecond, "the content refresh never quiesced")
}

// --- Scenario 2 visibility (#423) --------------------------------------------------------------------
//
// The validate gate stops BROKEN content from auto-deploying. It does nothing about content that is merely
// VALID-but-unreviewed, because validatePacks is a HEALTH gate, not a PROVENANCE one — so the issue's
// second scenario stays live: stage rows in pack A, never reload it, let somebody's reload of pack B carry
// them fleet-wide. changedPacks does not stop that; it makes it legible, which is the difference between
// an unreviewed deploy that is silent and one an operator can find afterwards.

// TestChangedPacksSpotsAStagedZoneEdit is the core case: a zone whose room set moved between snapshots.
func TestChangedPacksSpotsAStagedZoneEdit(t *testing.T) {
	prev := content.Merge([]content.Pack{refreshTestPack()})
	next := content.Merge([]content.Pack{refreshTestPack(
		content.RoomDTO{Ref: "ct:room:entry", Name: "The Entry", Long: "Entry.", Exits: map[string]string{}},
		content.RoomDTO{Ref: "ct:room:added", Name: "Added", Long: "Added.", Exits: map[string]string{}},
	)})
	require.Equal(t, []string{"ct"}, changedPacks(prev, next))
	require.Empty(t, changedPacks(prev, prev), "an identical snapshot must report nothing")
}

// TestChangedPacksSpotsTheInstanceableFlag is the one that matters most, and the reason `instanceable` is
// in the fingerprint at all: it is the control that bounds the instance faucet (#72), and it is the exact
// flag the issue's cross-pack scenario turns on. A staged `instanceable: true` changes NO room and NO
// exit, so a room-set-only comparison would call the snapshot unchanged and the deploy would stay silent.
func TestChangedPacksSpotsTheInstanceableFlag(t *testing.T) {
	base := refreshTestPack()
	staged := refreshTestPack()
	require.True(t, base.Zones[0].Instanceable, "fixture precondition")
	base.Zones[0].Instanceable = false

	require.Equal(t, []string{"ct"}, changedPacks(content.Merge([]content.Pack{base}), content.Merge([]content.Pack{staged})),
		"an instanceable opt-in changes no room and no exit — it must still be reported")
}

// TestChangedPacksSpotsAnAddedOrRemovedZone covers both directions of the set difference, since a zone
// DELETED from a pack emits no invalidation of its own and is otherwise the hardest change to notice.
func TestChangedPacksSpotsAnAddedOrRemovedZone(t *testing.T) {
	one := content.Merge([]content.Pack{{Pack: "p", Zones: []content.ZoneDTO{{Ref: "a"}}}})
	two := content.Merge([]content.Pack{{Pack: "p", Zones: []content.ZoneDTO{{Ref: "a"}, {Ref: "b"}}}})
	require.Equal(t, []string{"b"}, changedPacks(one, two), "an added zone")
	require.Equal(t, []string{"b"}, changedPacks(two, one), "a removed zone")
}

// TestChangedPacksIsNilSafe — the first refresh after boot has no previous snapshot on some paths, and a
// diff that panicked there would take out the refresh goroutine.
func TestChangedPacksIsNilSafe(t *testing.T) {
	lc := content.Merge([]content.Pack{refreshTestPack()})
	require.Nil(t, changedPacks(nil, lc))
	require.Nil(t, changedPacks(lc, nil))
	require.Nil(t, changedPacks(nil, nil))
}
