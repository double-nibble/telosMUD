package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/store"
	"github.com/double-nibble/telosmud/tests/dblock"
	"github.com/double-nibble/telosmud/tests/helpers"
)

// store_seedpull_cutover_test.go — #366: switching a deployment from telos-seed to telos-pull.
//
// Definition refs are GLOBAL (`zones.ref` and most `*_defs.ref` are bare primary keys; `pack` is a plain
// column), and telos-seed does not register: it calls ImportPacks, which never touches content_version or
// content_pack_registry. So after `make seed` the demo pack's rows exist while the registry is empty, and
// ImportVersion's prune — driven by that registry — has no idea the pack is there. The first pulled pack
// shipping the same zone ref died on:
//
//	import version: store: insert zone midgaard: duplicate key ... "zones_pkey" (SQLSTATE 23505)
//
// This tier is GATED on TELOS_TEST_DSN. `make verify` skips it, so it proves nothing locally there — but
// CI's `integration` job stands up postgres:16-alpine and runs exactly this path, which is the tier that
// exists because a deletePack idempotency bug once shipped through the hermetic one.

// cutoverPack builds a pack with a given name whose zone/room/attribute refs are parameterised, so a test
// can construct a deliberate collision or a deliberate near-miss.
func cutoverPack(name, zoneRef, attrRef string) content.Pack {
	return content.Pack{
		Pack: name,
		Zones: []content.ZoneDTO{{
			Ref: zoneRef, Name: "Cutover Zone", StartRoom: zoneRef + ":room:a",
			Rooms: []content.RoomDTO{{Ref: zoneRef + ":room:a", Name: "A", Long: "A room."}},
		}},
		Attributes: []content.AttributeDTO{{Ref: attrRef, DisplayName: "Attr"}},
	}
}

// uniq keeps refs distinct per test run so tests do not collide with each other's leftovers in a shared DB.
func uniq(t *testing.T, s string) string {
	t.Helper()
	return fmt.Sprintf("%s-%s", s, strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")))
}

// TestImportVersionRefusesForeignPackRefs is the headline: the collision is refused with a message naming
// the owning pack and the remedy, instead of a raw SQLSTATE, and the whole import rolls back.
func TestImportVersionRefusesForeignPackRefs(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := helpers.OpenTestPool(t)
	ctx := context.Background()
	seeded, incoming := uniq(t, "seedpack"), uniq(t, "pullpack")
	zoneRef, attrRef := uniq(t, "z"), uniq(t, "attr")
	t.Cleanup(func() { cleanupPacks(ctx, p, seeded, incoming) })

	// The registry-LESS seed import: exactly what `make seed` leaves behind.
	require.NoError(t, p.ImportPacks(ctx, []content.Pack{cutoverPack(seeded, zoneRef, attrRef)}))

	// The pull, shipping the same zone ref under a different pack name.
	_, _, _, err := p.ImportVersion(ctx, []content.Pack{cutoverPack(incoming, zoneRef, attrRef)},
		store.VersionMeta{ContentSHA: uniq(t, "sha"), ManifestVersion: "v1"})
	require.Error(t, err)

	var foreign *store.ErrForeignPackRefs
	require.ErrorAs(t, err, &foreign, "the refusal must be a typed error callers can branch on")
	require.Contains(t, foreign.Packs, seeded, "the error must NAME the pack that owns the colliding refs")

	msg := err.Error()
	require.Contains(t, msg, seeded)
	require.Contains(t, msg, "--purge-pack", "the message must carry the actual remedy command")
	// THE MUTATION TRAP. Without this, deleting the entire check still passes every assertion above via the
	// raw primary-key violation — the exact false-green this repo has shipped before.
	require.NotContains(t, msg, "23505", "this must be OUR refusal, not a duplicate-key error leaking through")
	require.NotContains(t, msg, "_pkey", "this must be OUR refusal, not a duplicate-key error leaking through")

	// And the import must have rolled back whole: the seeded rows survive, no version was minted.
	zones, zerr := p.PackZones(ctx, seeded)
	require.NoError(t, zerr)
	require.Contains(t, zones, zoneRef, "a refused import must not have disturbed the existing pack")
	cur, cerr := p.CurrentContentVersion(ctx)
	require.NoError(t, cerr)
	require.NotContains(t, cur.Packs, incoming, "a refused import must not enter the registry")
}

// TestPurgePackThenImportSucceeds is the documented cutover, end to end: the refusal names a command, the
// command works, and the pull then imports cleanly. A remedy nobody has verified is not a remedy.
func TestPurgePackThenImportSucceeds(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := helpers.OpenTestPool(t)
	ctx := context.Background()
	seeded, incoming := uniq(t, "seedpack"), uniq(t, "pullpack")
	zoneRef, attrRef := uniq(t, "z"), uniq(t, "attr")
	t.Cleanup(func() { cleanupPacks(ctx, p, seeded, incoming) })

	require.NoError(t, p.ImportPacks(ctx, []content.Pack{cutoverPack(seeded, zoneRef, attrRef)}))
	require.NoError(t, p.PurgePack(ctx, seeded))

	zones, err := p.PackZones(ctx, seeded)
	require.NoError(t, err)
	require.Empty(t, zones, "the purge must actually remove the pack's zones")

	version, _, changed, err := p.ImportVersion(ctx, []content.Pack{cutoverPack(incoming, zoneRef, attrRef)},
		store.VersionMeta{ContentSHA: uniq(t, "sha"), ManifestVersion: "v1"})
	require.NoError(t, err, "after the purge, the pull must import cleanly")
	require.True(t, changed)
	require.NotZero(t, version)

	cur, err := p.CurrentContentVersion(ctx)
	require.NoError(t, err)
	require.Contains(t, cur.Packs, incoming)
}

// TestPurgePackRefusesARegisteredPack keeps the recovery tool from becoming a footgun. Purging a REGISTERED
// pack would leave content_pack_registry describing rows that are gone — and it would bypass the
// live-hosted-pack prune guard entirely, since an orphan pack is by definition not in the registry and so
// is structurally invisible to that guard. The supported way to drop a registered pack is a manifest that
// omits it, which prunes it UNDER the guard.
func TestPurgePackRefusesARegisteredPack(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := helpers.OpenTestPool(t)
	ctx := context.Background()
	registered := uniq(t, "registeredpack")
	zoneRef, attrRef := uniq(t, "z"), uniq(t, "attr")
	t.Cleanup(func() { cleanupPacks(ctx, p, registered) })

	_, _, _, err := p.ImportVersion(ctx, []content.Pack{cutoverPack(registered, zoneRef, attrRef)},
		store.VersionMeta{ContentSHA: uniq(t, "sha"), ManifestVersion: "v1"})
	require.NoError(t, err)

	err = p.PurgePack(ctx, registered)
	require.Error(t, err, "purging a pack that is part of the installed content version must be refused")
	require.Contains(t, err.Error(), "content registry")

	zones, zerr := p.PackZones(ctx, registered)
	require.NoError(t, zerr)
	require.Contains(t, zones, zoneRef, "a refused purge must not have deleted anything")
}

// TestForeignRefDetectionCoversNonZoneTables is the case a zones-only implementation would miss, and the
// reason the check is table-driven over every global-ref-PK table rather than just the zone tree. Two packs
// with entirely DISJOINT zones can still collide on a shared attribute ref.
func TestForeignRefDetectionCoversNonZoneTables(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := helpers.OpenTestPool(t)
	ctx := context.Background()
	seeded, incoming := uniq(t, "seedpack"), uniq(t, "pullpack")
	attrRef := uniq(t, "sharedattr")
	t.Cleanup(func() { cleanupPacks(ctx, p, seeded, incoming) })

	require.NoError(t, p.ImportPacks(ctx, []content.Pack{cutoverPack(seeded, uniq(t, "za"), attrRef)}))

	// Different zone ref, SAME attribute ref.
	_, _, _, err := p.ImportVersion(ctx, []content.Pack{cutoverPack(incoming, uniq(t, "zb"), attrRef)},
		store.VersionMeta{ContentSHA: uniq(t, "sha"), ManifestVersion: "v1"})
	require.Error(t, err, "a pack-global def collision must be caught even when the zones are disjoint")

	var foreign *store.ErrForeignPackRefs
	require.ErrorAs(t, err, &foreign)
	require.Contains(t, foreign.Packs, seeded)
}

// TestDisjointSeedPackDoesNotBlockAPull is the no-false-positive invariant, and it is what keeps this from
// being a destructive behavior change. A seed-imported pack whose refs do NOT collide is harmless — the
// world's enabled set comes from the registry, so its rows are inert — and refusing it would break
// environments that work today. The check must fire on an ACTUAL collision, never on mere unregisteredness.
func TestDisjointSeedPackDoesNotBlockAPull(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := helpers.OpenTestPool(t)
	ctx := context.Background()
	seeded, incoming := uniq(t, "seedpack"), uniq(t, "pullpack")
	t.Cleanup(func() { cleanupPacks(ctx, p, seeded, incoming) })

	require.NoError(t, p.ImportPacks(ctx, []content.Pack{cutoverPack(seeded, uniq(t, "za"), uniq(t, "attra"))}))
	_, _, _, err := p.ImportVersion(ctx, []content.Pack{cutoverPack(incoming, uniq(t, "zb"), uniq(t, "attrb"))},
		store.VersionMeta{ContentSHA: uniq(t, "sha"), ManifestVersion: "v1"})
	require.NoError(t, err, "an unregistered pack with DISJOINT refs must not block a pull")
}

// TestReSeedingTheSamePackStillStripReplaces guards the other no-false-positive direction: a pack may
// always overwrite its OWN refs. `make seed` twice, or a re-import of the same pack, must keep working —
// the check's `allowed` set is the batch's own names precisely so this stays a strip-replace.
func TestReSeedingTheSamePackStillStripReplaces(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := helpers.OpenTestPool(t)
	ctx := context.Background()
	pack := uniq(t, "seedpack")
	zoneRef, attrRef := uniq(t, "z"), uniq(t, "attr")
	t.Cleanup(func() { cleanupPacks(ctx, p, pack) })

	pk := cutoverPack(pack, zoneRef, attrRef)
	require.NoError(t, p.ImportPacks(ctx, []content.Pack{pk}))
	require.NoError(t, p.ImportPacks(ctx, []content.Pack{pk}), "a re-seed of the same pack must strip-replace")
}

// TestImportVersionStillPrunesAPackItLegitimatelyDrops proves the check runs AFTER the prune. A pack the
// new version drops has already had its rows deleted by the time the check looks, so handing its refs to
// another pack in the same version is legitimate and must not be mistaken for a foreign collision. Getting
// this ordering wrong would break the ordinary rename-a-pack deploy.
func TestImportVersionStillPrunesAPackItLegitimatelyDrops(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := helpers.OpenTestPool(t)
	ctx := context.Background()
	oldPack, newPack := uniq(t, "oldpack"), uniq(t, "newpack")
	zoneRef, attrRef := uniq(t, "z"), uniq(t, "attr")
	t.Cleanup(func() { cleanupPacks(ctx, p, oldPack, newPack) })

	// v1 registers oldPack (so it IS in the registry and therefore prunable).
	_, _, _, err := p.ImportVersion(ctx, []content.Pack{cutoverPack(oldPack, zoneRef, attrRef)},
		store.VersionMeta{ContentSHA: uniq(t, "sha1"), ManifestVersion: "v1"})
	require.NoError(t, err)

	// v2 drops oldPack and hands its refs to newPack — a pack rename, which must just work.
	_, pruned, changed, err := p.ImportVersion(ctx, []content.Pack{cutoverPack(newPack, zoneRef, attrRef)},
		store.VersionMeta{ContentSHA: uniq(t, "sha2"), ManifestVersion: "v2"})
	require.NoError(t, err, "the check must run AFTER the prune, or every pack rename would be refused")
	require.True(t, changed)
	require.Contains(t, pruned, oldPack)
}

// TestImportPacksRefusesForeignPackRefs covers the REVERSE direction — pull first, then re-run `make seed`
// with a colliding pack. A mutation proved this call site had no coverage at all: deleting the check from
// ImportPacks left the entire gated suite green, and it was that gap which hid the wrong remedy below.
//
// It also pins the remedy TEXT, because the message must differ by whether the owner is registered. Telling
// an operator to `--purge-pack` a registered pack prints a command PurgePack is guaranteed to refuse, which
// costs them the time to try it and teaches them the tool is broken.
func TestImportPacksRefusesForeignPackRefs(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := helpers.OpenTestPool(t)
	ctx := context.Background()
	pulled, reseeded := uniq(t, "pulledpack"), uniq(t, "seedpack")
	zoneRef, attrRef := uniq(t, "z"), uniq(t, "attr")
	t.Cleanup(func() { cleanupPacks(ctx, p, pulled, reseeded) })

	// A REGISTERED pack, installed the way a real pull installs one.
	_, _, _, err := p.ImportVersion(ctx, []content.Pack{cutoverPack(pulled, zoneRef, attrRef)},
		store.VersionMeta{ContentSHA: uniq(t, "sha"), ManifestVersion: "v1"})
	require.NoError(t, err)

	// Now `make seed` a colliding pack.
	err = p.ImportPacks(ctx, []content.Pack{cutoverPack(reseeded, zoneRef, attrRef)})
	require.Error(t, err, "a re-seed colliding with installed content must be refused")

	var foreign *store.ErrForeignPackRefs
	require.ErrorAs(t, err, &foreign)
	require.Contains(t, foreign.Packs, pulled)
	require.Contains(t, foreign.Registered, pulled,
		"the owner IS part of the installed content version, and the error must know that")

	msg := err.Error()
	require.NotContains(t, msg, "--purge-pack",
		"a registered owner must NOT be offered the purge remedy — PurgePack refuses it, so the message "+
			"would send the operator down a dead end")
	require.Contains(t, msg, "INSTALLED content version")
	require.NotContains(t, msg, "23505", "must be OUR refusal, not a duplicate-key error leaking through")

	// The refused re-seed must have rolled back whole.
	zones, zerr := p.PackZones(ctx, reseeded)
	require.NoError(t, zerr)
	require.Empty(t, zones)
}

// TestUnregisteredOwnerStillGetsThePurgeRemedy is the other half: the seed→pull case DOES get the purge
// command, and following it must actually work. Together these pin that the remedy tracks the registry.
func TestUnregisteredOwnerStillGetsThePurgeRemedy(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := helpers.OpenTestPool(t)
	ctx := context.Background()
	seeded, incoming := uniq(t, "seedpack"), uniq(t, "pullpack")
	zoneRef, attrRef := uniq(t, "z"), uniq(t, "attr")
	t.Cleanup(func() { cleanupPacks(ctx, p, seeded, incoming) })

	require.NoError(t, p.ImportPacks(ctx, []content.Pack{cutoverPack(seeded, zoneRef, attrRef)}))
	_, _, _, err := p.ImportVersion(ctx, []content.Pack{cutoverPack(incoming, zoneRef, attrRef)},
		store.VersionMeta{ContentSHA: uniq(t, "sha"), ManifestVersion: "v1"})
	require.Error(t, err)

	var foreign *store.ErrForeignPackRefs
	require.ErrorAs(t, err, &foreign)
	require.Empty(t, foreign.Registered, "a seed-imported leftover is not in the registry")
	require.Contains(t, err.Error(), "--purge-pack")

	// FOLLOW the remedy: it must succeed, not dead-end.
	require.NoError(t, p.PurgePack(ctx, seeded), "the remedy the message prints must actually work")
}

// cleanupPacks removes test packs, REGISTERED ONES INCLUDED.
//
// A plain PurgePack is not enough and silently was not: three of these tests register their pack via
// ImportVersion, so PurgePack correctly refuses (it will not desync the registry from the rows) and the
// discarded error left synthetic packs permanently in a shared dev database's content registry — pointing
// a local stack at content that no longer exists. So this first publishes an EMPTY content version, which
// prunes every registered pack through the supported path, and only then purges whatever is left over.
func cleanupPacks(ctx context.Context, p *store.Pool, packs ...string) {
	cur, err := p.CurrentContentVersion(ctx)
	if err == nil {
		for _, want := range packs {
			for _, have := range cur.Packs {
				if have != want {
					continue
				}
				// A version with no packs prunes everything the registry lists. Safe ONLY because the
				// caller holds the content-registry advisory lock (helpers.LockContentRegistry) for the
				// whole test — without it this wipes the registry out from under a concurrently running
				// test in another package, which is exactly the interference that turned CI red.
				_, _, _, _ = p.ImportVersion(ctx, nil, store.VersionMeta{
					ContentSHA: "cleanup-" + want, ManifestVersion: "cleanup",
				})
				break
			}
		}
	}
	for _, pack := range packs {
		_ = p.PurgePack(ctx, pack)
	}
}
