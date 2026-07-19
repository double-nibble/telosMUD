package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/world"
)

// ownerepoch_test.go is the REAL-TIER half of the #432 fence, and the parity net that keeps the
// hermetic half honest.
//
// Every world-package test of the ownership fence runs against world.MemStore. If MemStore and the
// pgx store disagree about what a given (stored epoch/version, snapshot epoch/version) means, those
// tests are vacuously green while the only tier that actually holds the line in production behaves
// differently. So this file drives ONE table of cases through BOTH implementations and demands
// identical verdicts — the same idea as internal/world/lua_parity_test.go, which proves the two Lua
// sandboxes AGREE rather than proving each is individually correct.
//
// The pgx half is gated on TELOS_TEST_DSN (testPool skips without it), exactly like the rest of this
// package. The MemStore half is NOT gated and runs in the default hermetic suite.

// ---------------------------------------------------------------------------------------------
// 4b. The mint, against real Postgres.
// ---------------------------------------------------------------------------------------------

// TestPgxClaimCharacterIsAtomic pins the property the whole fence rests on, at the tier that has to
// hold it: N concurrent claims on one row must return N DISTINCT, strictly increasing epochs.
//
// The pgx implementation gets this from Postgres serializing concurrent UPDATEs on the row lock
// inside ONE statement. Decompose it into a read then a bump — which is the natural refactor, and
// what a naive port to any other store would do — and two concurrent logins receive the SAME epoch;
// both then satisfy `owner_epoch <= $k` at save time and the fence silently degrades into the
// state_version ping-pong it was built to end. The bug would look fixed.
func TestPgxClaimCharacterIsAtomic(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	name := "GatedEpochAtomic-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE name = $1`, name)
	})
	pid, err := p.CreateCharacter(ctx, name, "midgaard", "midgaard:room:temple")
	require.NoError(t, err)

	const n = 32
	epochs := make([]uint64, n)
	errs := make([]error, n)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			epochs[i], errs[i] = p.ClaimCharacter(ctx, pid, 0)
		}()
	}
	start.Done()
	done.Wait()

	seen := map[uint64]bool{}
	for i, ep := range epochs {
		require.NoErrorf(t, errs[i], "claim %d", i)
		if seen[ep] {
			t.Fatalf("epoch %d was minted TWICE by concurrent claims (#432): the mint must be ONE statement "+
				"under the row lock. A read-then-bump hands two logins the same epoch, both pass the "+
				"`owner_epoch <= $k` predicate, and two live copies are back to force-writing over each other", ep)
		}
		seen[ep] = true
	}
	for want := uint64(1); want <= n; want++ {
		require.Truef(t, seen[want], "the %d concurrent claims must be exactly the contiguous run 1..%d; %d is missing", n, n, want)
	}

	t.Run("floor only ever RAISES the mint", func(t *testing.T) {
		high, err := p.ClaimCharacter(ctx, pid, 1000)
		require.NoError(t, err)
		require.EqualValues(t, 1001, high, "a floor above the row must raise the mint to floor+1")
		low, err := p.ClaimCharacter(ctx, pid, 5)
		require.NoError(t, err)
		require.EqualValues(t, 1002, low,
			"a floor BELOW the stored epoch must not lower the mint. The floor's source is the directory — "+
				"evictable Redis (#340) — and an assignment would hand a live character an epoch their own row "+
				"already outranks, wedging every save they make for the session's lifetime")
	})

	t.Run("a missing row reports ErrNoCharacterRow", func(t *testing.T) {
		_, err := p.ClaimCharacter(ctx, world.PersistID(uuid.NewString()), 0)
		require.ErrorIs(t, err, world.ErrNoCharacterRow,
			"a claim against a soft-deleted/nonexistent row must be distinguishable from an infrastructure "+
				"error, so the login path degrades instead of refusing")
	})

	t.Run("the ceiling value itself is ACCEPTED", func(t *testing.T) {
		// The guard is a ceiling, not an off-by-one fence: exactly maxOwnerEpoch must still mint. The
		// rejecting side is hermetic (TestClaimCharacterRefusesAnAbsurdFloor); this is the accepting side,
		// which needs a real row to write to.
		got, err := p.ClaimCharacter(ctx, pid, maxOwnerEpoch)
		require.NoError(t, err, "a floor AT the ceiling must be accepted; the guard rejects only what is above it")
		require.EqualValues(t, uint64(maxOwnerEpoch)+1, got)
		// Put the row back somewhere sane for any later subtest.
		_, err = p.pool.Exec(ctx, `UPDATE characters SET owner_epoch = 0 WHERE id = $1`, uuid.MustParse(string(pid)))
		require.NoError(t, err)
	})

	t.Run("a SOFT-DELETED row reports ErrNoCharacterRow", func(t *testing.T) {
		del := name + "-deleted"
		t.Cleanup(func() {
			_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE name = $1`, del)
		})
		dpid, err := p.CreateCharacter(ctx, del, "midgaard", "midgaard:room:temple")
		require.NoError(t, err)
		_, err = p.pool.Exec(ctx, `UPDATE characters SET deleted_at = now() WHERE id = $1`, uuid.MustParse(string(dpid)))
		require.NoError(t, err)
		_, err = p.ClaimCharacter(ctx, dpid, 0)
		require.ErrorIs(t, err, world.ErrNoCharacterRow,
			"the claim must carry the same `deleted_at IS NULL` predicate the load does, or a login could claim "+
				"ownership of a row no save will ever be allowed to touch")
	})
}

// ---------------------------------------------------------------------------------------------
// 5. The parity net.
// ---------------------------------------------------------------------------------------------

// saveOutcomeCase is one (stored row state, incoming snapshot) pair and the single verdict BOTH
// stores must return for it.
type saveOutcomeCase struct {
	name string
	// storedEpoch/storedVersion are driven into the row before the save under test. storedEpoch 0
	// means "never claimed" (a legacy row); it is set via ClaimCharacter, so it is >= 1 otherwise.
	storedEpoch   uint64
	storedVersion uint64
	snapEpoch     uint64
	snapVersion   uint64
	want          world.SaveOutcome
	why           string
}

func saveOutcomeCases() []saveOutcomeCase {
	return []saveOutcomeCase{
		{
			name: "the owner writes at the current version", storedEpoch: 3, storedVersion: 2,
			snapEpoch: 3, snapVersion: 2, want: world.SaveApplied,
			why: "the ordinary flush: same epoch, matching version",
		},
		{
			name: "a NEWER owner writes at the current version", storedEpoch: 3, storedVersion: 2,
			snapEpoch: 9, snapVersion: 2, want: world.SaveApplied,
			why: "the predicate is `owner_epoch <= $k`, so a fresher claim writes and RAISES the stored epoch",
		},
		{
			name: "an UNFENCED (legacy, epoch-0) row accepts an epoch-0 writer", storedEpoch: 0, storedVersion: 1,
			snapEpoch: 0, snapVersion: 1, want: world.SaveApplied,
			why: "a pre-migration row must keep working through a rolling deploy; the fence arms on the first claim",
		},
		{
			name: "a ZOMBIE at the current version loses on ownership", storedEpoch: 5, storedVersion: 2,
			snapEpoch: 4, snapVersion: 2, want: world.SaveNotOwner,
			why: "THE #432 case: the version matches perfectly, so only the epoch conjunct can refuse this write",
		},
		{
			name: "a zombie whose version is ALSO stale still reports NOT-OWNER", storedEpoch: 5, storedVersion: 4,
			snapEpoch: 4, snapVersion: 1, want: world.SaveNotOwner,
			why: "ownership DOMINATES contention: reporting `stale version` would send the caller into the rebase " +
				"loop the fence exists to forbid, and that loop can never succeed",
		},
		{
			name: "the owner at a STALE version is retryable contention", storedEpoch: 3, storedVersion: 4,
			snapEpoch: 3, snapVersion: 1, want: world.SaveStaleVersion,
			why: "this shard's own cadence racing its logout flush — rebase and retry is the CORRECT answer here",
		},
		{
			name: "a NEWER owner at a stale version is contention, not ownership", storedEpoch: 3, storedVersion: 4,
			snapEpoch: 7, snapVersion: 1, want: world.SaveStaleVersion,
			why: "the epoch conjunct is satisfied, so the only thing refusing this is the version",
		},
		{
			name: "an owner AHEAD of the stored version is contention", storedEpoch: 3, storedVersion: 1,
			snapEpoch: 3, snapVersion: 9, want: world.SaveStaleVersion,
			why: "unreachable in production, but the CAS is an equality: the two stores must not diverge on it",
		},
	}
}

// setupRow drives a store's row for `name` to (storedVersion, storedEpoch) and returns its PID.
// The version is advanced first, at epoch 0, so the saves used to advance it cannot themselves stamp
// an epoch; the claim then sets the epoch without touching the version.
func setupRow(t *testing.T, s world.CharacterStore, name string, storedVersion, storedEpoch uint64) world.PersistID {
	t.Helper()
	ctx := context.Background()
	pid, err := s.CreateCharacter(ctx, name, "midgaard", "midgaard:room:temple")
	require.NoError(t, err)

	for v := uint64(0); v < storedVersion; v++ {
		res, err := s.SaveCharacter(ctx, world.CharSnapshot{
			PID: pid, Name: name, ZoneRef: "midgaard", RoomRef: "midgaard:room:temple",
			StateVersion: v, OwnerEpoch: 0,
		})
		require.NoError(t, err)
		require.Equalf(t, world.SaveApplied, res.Outcome, "setup save at version %d", v)
	}
	if storedEpoch > 0 {
		// ClaimCharacter mints floor+1 when floor >= the row's epoch, so floor = storedEpoch-1 lands
		// exactly on storedEpoch.
		got, err := s.ClaimCharacter(ctx, pid, storedEpoch-1)
		require.NoError(t, err)
		require.EqualValues(t, storedEpoch, got, "setup claim must land exactly on the case's stored epoch")
	}
	return pid
}

// runSaveOutcome performs the case's save against one store, returns the result, and — on an APPLIED
// case — asserts the epoch actually LANDED in the row.
//
// That last assertion is not incidental. The write clause and the predicate are separate pieces of
// SQL: `owner_epoch = GREATEST(c.owner_epoch, $6)` alongside `AND c.owner_epoch <= $6`. Delete the
// assignment and every one of these cases still returns the right OUTCOME, because the predicate is
// untouched — the column simply stops advancing, and the fence quietly freezes at whatever value
// happened to land first, accepting every later writer. Reading the row back is what catches that.
func runSaveOutcome(t *testing.T, s world.CharacterStore, name string, tc saveOutcomeCase) world.SaveResult {
	t.Helper()
	ctx := context.Background()
	pid := setupRow(t, s, name, tc.storedVersion, tc.storedEpoch)
	res, err := s.SaveCharacter(ctx, world.CharSnapshot{
		PID: pid, Name: name, ZoneRef: "midgaard", RoomRef: "midgaard:room:market",
		StateVersion: tc.snapVersion, OwnerEpoch: tc.snapEpoch,
	})
	require.NoError(t, err)

	after, found, err := s.LoadCharacter(ctx, name)
	require.NoError(t, err)
	require.True(t, found, "the row must still exist after the save attempt")
	if res.Outcome == world.SaveApplied {
		require.EqualValuesf(t, tc.snapEpoch, after.OwnerEpoch,
			"an APPLIED save did not persist its owner_epoch (row holds %d, the writer claimed %d). The write "+
				"clause and the predicate are independent: dropping `owner_epoch = GREATEST(...)` leaves every "+
				"outcome in this table correct while the column stops advancing, so the fence freezes at its "+
				"first value and accepts every later writer (#432)", after.OwnerEpoch, tc.snapEpoch)
	} else {
		require.EqualValuesf(t, tc.storedEpoch, after.OwnerEpoch,
			"a REFUSED save must not move owner_epoch at all; row holds %d, want the untouched %d",
			after.OwnerEpoch, tc.storedEpoch)
	}
	return res
}

// TestMemStoreAndPgxAgreeOnSaveOutcomes is the parity net. Without it, every #432 test in
// internal/world is an assertion about a test double.
//
// GATING, and why it is structured this way: the MemStore half is HERMETIC and always runs, so the
// table is exercised on every push; only the pgx half skips without TELOS_TEST_DSN. An earlier cut
// opened the pool at the top of the function, which made the whole test — including the MemStore
// half its own comment claimed ran hermetically — skip as one unit. A test whose coverage claim is
// larger than what it runs is worse than no test, so the two halves are separate subtests and the
// skip is scoped to the one that needs a database.
func TestMemStoreAndPgxAgreeOnSaveOutcomes(t *testing.T) {
	stamp := time.Now().Format("150405.000000")
	cases := saveOutcomeCases()

	// --- Half 1: MemStore. Hermetic. Records each verdict for the pgx half to compare against.
	mem := world.NewMemStore()
	memResults := make([]world.SaveResult, len(cases))
	t.Run("memstore (hermetic)", func(t *testing.T) {
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				memResults[i] = runSaveOutcome(t, mem, fmt.Sprintf("ParityMem-%s-%d", stamp, i), tc)
				if memResults[i].Outcome != tc.want {
					t.Fatalf("MemStore outcome = %v, want %v: %s", memResults[i].Outcome, tc.want, tc.why)
				}
			})
		}
		t.Run("a snapshot with no live row reports SaveNoRow", func(t *testing.T) {
			res, err := mem.SaveCharacter(context.Background(), world.CharSnapshot{
				PID: world.PersistID(uuid.NewString()), Name: "ParityMemGhost-" + stamp,
				ZoneRef: "midgaard", RoomRef: "midgaard:room:temple",
			})
			require.NoError(t, err)
			require.Equal(t, world.SaveNoRow, res.Outcome)
		})
		t.Run("the zero SaveResult is the INVALID outcome", func(t *testing.T) {
			// SaveOutcomeUnset must never read as success: a CharacterStore double that forgets to set
			// Outcome has to trip the caller's default branch. A bool that meant three things is how the
			// force-write shipped in the first place.
			var zero world.SaveResult
			require.Equal(t, world.SaveOutcomeUnset, zero.Outcome)
			require.Equal(t, "unset", world.SaveOutcomeUnset.String())
			require.NotEqual(t, world.SaveOutcomeUnset, world.SaveApplied)
		})
	})

	// --- Half 2: pgx. Gated. Skips HERE, not above, so half 1 still ran.
	t.Run("pgx (gated on TELOS_TEST_DSN)", func(t *testing.T) {
		p := testPool(t) // t.Skip lands on this subtest only
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				pgName := fmt.Sprintf("ParityPg-%s-%d", stamp, i)
				t.Cleanup(func() {
					_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE name = $1`, pgName)
				})
				pgRes := runSaveOutcome(t, p, pgName, tc)
				if pgRes.Outcome != tc.want {
					t.Fatalf("pgx outcome = %v, want %v: %s", pgRes.Outcome, tc.want, tc.why)
				}
				memRes := memResults[i]
				require.NotEqualf(t, world.SaveOutcomeUnset, memRes.Outcome,
					"the MemStore half did not run for case %q, so there is nothing to compare against", tc.name)
				if memRes.Outcome != pgRes.Outcome {
					t.Fatalf("PARITY BREAK (#432): MemStore says %v and pgx says %v for stored(epoch=%d ver=%d) "+
						"snapshot(epoch=%d ver=%d). Every world-package test of the fence runs against MemStore, so a "+
						"divergence here makes all of them vacuously green while the real tier behaves differently",
						memRes.Outcome, pgRes.Outcome, tc.storedEpoch, tc.storedVersion, tc.snapEpoch, tc.snapVersion)
				}
				// The DIAGNOSTIC columns must agree too: the caller rebases onto CurVersion and logs/decides
				// on CurOwnerEpoch, so a store that returned the right verdict with the wrong observed row
				// would send the reconcile to the wrong place.
				require.Equal(t, memRes.CurVersion, pgRes.CurVersion,
					"the observed state_version must agree — it is the rebase target on a contention loss")
				require.Equal(t, memRes.CurOwnerEpoch, pgRes.CurOwnerEpoch,
					"the observed owner_epoch must agree — it is the alertable 'who beat us' diagnostic")
				if tc.want == world.SaveApplied {
					require.Equal(t, memRes.NewVersion, pgRes.NewVersion, "the bumped state_version must agree")
				}
			})
		}

		t.Run("a snapshot with no live row reports SaveNoRow", func(t *testing.T) {
			res, err := p.SaveCharacter(context.Background(), world.CharSnapshot{
				PID: world.PersistID(uuid.NewString()), Name: "ParityPgGhost-" + stamp,
				ZoneRef: "midgaard", RoomRef: "midgaard:room:temple",
			})
			require.NoError(t, err)
			require.Equal(t, world.SaveNoRow, res.Outcome,
				"a save against a deleted/nonexistent row must be its own terminal outcome; reported as a version "+
					"loss it would drive the final-flush retry ladder against a row that can never exist")
		})

		t.Run("a SOFT-DELETED row is SaveNoRow, never a resurrection", func(t *testing.T) {
			name := "ParityDeleted-" + stamp
			t.Cleanup(func() {
				_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE name = $1`, name)
			})
			ctx := context.Background()
			pid, err := p.CreateCharacter(ctx, name, "midgaard", "midgaard:room:temple")
			require.NoError(t, err)
			_, err = p.pool.Exec(ctx, `UPDATE characters SET deleted_at = now() WHERE id = $1`, uuid.MustParse(string(pid)))
			require.NoError(t, err)

			res, err := p.SaveCharacter(ctx, world.CharSnapshot{
				PID: pid, Name: name, ZoneRef: "midgaard", RoomRef: "midgaard:room:market", StateVersion: 0,
			})
			require.NoError(t, err)
			require.Equal(t, world.SaveNoRow, res.Outcome,
				"the pre-#432 UPDATE omitted `deleted_at IS NULL` even though LoadCharacter had it, so a save could "+
					"write state back onto a soft-deleted row")
		})
	})
}

// TestClaimCharacterRefusesAnAbsurdFloor pins the boundary guard on the claim's floor.
//
// The floor's outside source is the DIRECTORY — evictable Redis, and writable by anything with access
// to it. `GREATEST(owner_epoch, $2) + 1` would store whatever it is handed, and a corrupted or hostile
// value near the top of the range pins the row's epoch there PERMANENTLY: every subsequent `+1`
// overflows BIGINT, every claim errors, and the character can never log in again. There is no
// in-code recovery, so the boundary refuses the value instead of storing it.
//
// HERMETIC: the ceiling check runs before any query is issued, so this needs no database — which is
// the point. A guard that only ran under TELOS_TEST_DSN would be unverified on most pushes.
func TestClaimCharacterRefusesAnAbsurdFloor(t *testing.T) {
	p := &Pool{} // no pool needed: the guard must reject before touching the database
	pid := world.PersistID(uuid.NewString())

	// The guard must fire BEFORE any query is issued. Reaching the (nil) pool is itself the failure, so
	// a panic is converted into the diagnosis rather than a stack trace.
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ClaimCharacter reached the database with a floor of %d instead of refusing it at the "+
					"boundary (panicked on the nil pool: %v). An absurd floor must never be handed to "+
					"GREATEST(owner_epoch, $2): stored, it pins the row's epoch there forever and the character "+
					"becomes permanently unloggable (#432)", uint64(maxOwnerEpoch)+1, r)
			}
		}()
		_, err = p.ClaimCharacter(context.Background(), pid, maxOwnerEpoch+1)
	}()
	require.Error(t, err,
		"a floor above the sane ceiling must be REFUSED at the boundary (#432). Stored, it pins owner_epoch "+
			"there forever: the next +1 overflows BIGINT, every claim fails, and the character is permanently "+
			"unloggable with no in-code recovery")
	require.ErrorContains(t, err, "ceiling",
		"the refusal must name the ceiling so an operator can tell a corrupted directory epoch from a real outage")
	require.NotErrorIs(t, err, world.ErrNoCharacterRow,
		"an absurd floor is a bad REQUEST, not a missing row; conflating them would send the login path down the "+
			"degrade-gracefully branch for what is actually corrupt input")
}
