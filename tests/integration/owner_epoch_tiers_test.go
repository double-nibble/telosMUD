package integration

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/checkpoint"
	"github.com/double-nibble/telosmud/internal/world"
	"github.com/double-nibble/telosmud/tests/helpers"
)

// owner_epoch_tiers_test.go is the field-drop net for `owner_epoch` (#432), one case PER DURABILITY
// TIER.
//
// This repo has shipped a silently-dropped field through a store round trip THREE times (Rounds 11,
// 35, 36), and owner_epoch is unusually exposed to it: unlike almost everything else on a
// CharSnapshot it is NOT inside the `state` JSONB, so it does not free-ride on the blob. It is a
// dedicated Postgres COLUMN (every SELECT list that omits it silently returns 0), a dedicated Redis
// HASH FIELD, and a plain struct field in MemStore. Three hand-written mappings, three independent
// chances to drop it — and a tier that reads 0 back is not "slightly wrong", it is UNFENCED, because
// every comparison treats 0 as "loses to any real claim".
//
// The table is per-tier ON PURPOSE. Adding a fourth durable tier without adding a case here should be
// visibly missing, not silently uncovered.
//
// GATING: the Postgres tier goes through helpers.OpenTestPool, which t.Skip's unless TELOS_TEST_DSN
// is set — so `go test ./...` stays dep-free while `make test-integration` and the CI `integration`
// job (which stands up postgres:16-alpine and exports TELOS_TEST_DSN) actually RUN it. The Redis and
// MemStore tiers are NOT gated: they run in the hermetic job too, so at least two of the three
// mappings are covered on every push.

// TestOwnerEpochRoundTripsEveryTier drives one epoch through save/load at each tier.
func TestOwnerEpochRoundTripsEveryTier(t *testing.T) {
	// A distinctive value: not 0 (which every tier defaults to, so a drop would be invisible) and not 1
	// (which a fresh claim would coincidentally mint).
	const epoch = uint64(7)
	stamp := time.Now().Format("150405.000000000")

	tiers := []struct {
		name string
		// open returns the tier under test, plus the character name to use. It may t.Skip.
		open func(t *testing.T) (saveLoad, string)
		why  string
	}{
		{
			name: "postgres row (characters.owner_epoch)",
			why: "a dedicated COLUMN, not part of the `state` JSONB: any SELECT list that omits it returns 0, " +
				"and a 0 read makes the very next save claim to be unfenced",
			open: func(t *testing.T) (saveLoad, string) {
				pool := helpers.OpenTestPool(t) // t.Skip when TELOS_TEST_DSN is unset
				return pgTier{pool}, "EpochTierPg-" + stamp
			},
		},
		{
			name: "redis checkpoint (the hash's `epoch` field + the value blob)",
			why: "the checkpoint's `value` struct is a hand-written serialization distinct from CharSnapshot; " +
				"the guard script also reads the epoch as its own hash field rather than decoding the blob",
			open: func(t *testing.T) (saveLoad, string) {
				mr := miniredis.RunT(t)
				rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				t.Cleanup(func() { _ = rdb.Close() })
				return ckptTier{checkpoint.NewRedis(rdb, "telos")}, "EpochTierRedis-" + stamp
			},
		},
		{
			name: "memstore (the tier every hermetic world test runs against)",
			why: "if MemStore drops it, every #432 test in internal/world is vacuously green while the real " +
				"tiers are the only thing holding the line",
			open: func(_ *testing.T) (saveLoad, string) {
				return memTier{world.NewMemStore()}, "EpochTierMem-" + stamp
			},
		},
	}

	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			s, name := tier.open(t)

			pid := s.create(t, name)
			res := s.save(t, world.CharSnapshot{
				PID: pid, Name: name, ZoneRef: "midgaard", RoomRef: "midgaard:room:market",
				StateVersion: 0, OwnerEpoch: epoch,
				State: world.StateJSON{Inventory: []world.ItemJSON{{ProtoRef: "midgaard:obj:torch"}}},
			})
			require.NotEqual(t, world.SaveOutcomeUnset, res, "premise: the tier accepted the write")

			got := s.load(t, name)
			require.EqualValues(t, epoch, got.OwnerEpoch,
				"FIELD DROP (#432): owner_epoch did not survive this tier's round trip. It read back as %d, which "+
					"every comparison in the codebase treats as UNFENCED — so this tier silently accepts a zombie's "+
					"write while the others refuse it. %s", got.OwnerEpoch, tier.why)

			// And the epoch must not be a write-once artefact: a later claim's higher value has to land too.
			res = s.save(t, world.CharSnapshot{
				PID: pid, Name: name, ZoneRef: "midgaard", RoomRef: "midgaard:room:temple",
				StateVersion: got.StateVersion, OwnerEpoch: epoch + 5,
				State: got.State,
			})
			require.NotEqual(t, world.SaveOutcomeUnset, res)
			got = s.load(t, name)
			require.EqualValues(t, epoch+5, got.OwnerEpoch,
				"a newer owner's epoch must be persisted, or the fence freezes at whatever value happened to land first")
		})
	}
}

// saveLoad is the one seam every durable tier shares: mint an identity, write a snapshot, read it
// back. Keeping it an interface is what makes the table per-tier rather than three copy-pasted tests.
type saveLoad interface {
	create(t *testing.T, name string) world.PersistID
	save(t *testing.T, snap world.CharSnapshot) world.SaveOutcome
	load(t *testing.T, name string) world.CharSnapshot
}

type pgTier struct{ p pgPool }

// pgPool is the subset of *store.Pool this test uses (DI, per the TEST STANDARD).
type pgPool interface {
	CreateCharacter(ctx context.Context, name, zoneRef, roomRef string) (world.PersistID, error)
	SaveCharacter(ctx context.Context, snap world.CharSnapshot) (world.SaveResult, error)
	LoadCharacter(ctx context.Context, name string) (world.CharSnapshot, bool, error)
}

func (x pgTier) create(t *testing.T, name string) world.PersistID {
	t.Helper()
	pid, err := x.p.CreateCharacter(context.Background(), name, "midgaard", "midgaard:room:temple")
	require.NoError(t, err)
	return pid
}

func (x pgTier) save(t *testing.T, snap world.CharSnapshot) world.SaveOutcome {
	t.Helper()
	res, err := x.p.SaveCharacter(context.Background(), snap)
	require.NoError(t, err)
	require.Equal(t, world.SaveApplied, res.Outcome, "the tier must accept this write")
	return res.Outcome
}

func (x pgTier) load(t *testing.T, name string) world.CharSnapshot {
	t.Helper()
	snap, found, err := x.p.LoadCharacter(context.Background(), name)
	require.NoError(t, err)
	require.True(t, found)
	return snap
}

type ckptTier struct{ r *checkpoint.Redis }

func (ckptTier) create(_ *testing.T, _ string) world.PersistID {
	return world.PersistID("11111111-2222-3333-4444-555555555555") // the checkpoint tier mints nothing
}

func (x ckptTier) save(t *testing.T, snap world.CharSnapshot) world.SaveOutcome {
	t.Helper()
	require.NoError(t, x.r.Checkpoint(context.Background(), snap))
	return world.SaveApplied
}

func (x ckptTier) load(t *testing.T, name string) world.CharSnapshot {
	t.Helper()
	snap, found, err := x.r.LoadCheckpoint(context.Background(), name)
	require.NoError(t, err)
	require.True(t, found)
	return snap
}

type memTier struct{ m *world.MemStore }

func (x memTier) create(t *testing.T, name string) world.PersistID {
	t.Helper()
	pid, err := x.m.CreateCharacter(context.Background(), name, "midgaard", "midgaard:room:temple")
	require.NoError(t, err)
	return pid
}

func (x memTier) save(t *testing.T, snap world.CharSnapshot) world.SaveOutcome {
	t.Helper()
	res, err := x.m.SaveCharacter(context.Background(), snap)
	require.NoError(t, err)
	require.Equal(t, world.SaveApplied, res.Outcome, "the tier must accept this write")
	return res.Outcome
}

func (x memTier) load(t *testing.T, name string) world.CharSnapshot {
	t.Helper()
	snap, found, err := x.m.LoadCharacter(context.Background(), name)
	require.NoError(t, err)
	require.True(t, found)
	return snap
}
