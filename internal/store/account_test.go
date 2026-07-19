package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/world"
)

// account_test.go is the gated (TELOS_TEST_DSN) Postgres test for the Phase-14 account/character store
// methods: the account-scoped character list, name availability, and account-owned character creation with
// the unique-name guard. testPool migrates 00015 (the account tables) before the test runs.

func TestAccountCharactersAndCreate(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	// A fresh account to own the characters.
	acct := uuid.NewString()
	_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status) VALUES ($1, 'active')`, acct)
	require.NoError(t, err)

	name := "GatedAcctChar-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE account_id = $1`, acct)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, acct)
	})

	// Empty to start.
	chars, err := p.AccountCharacters(ctx, acct)
	require.NoError(t, err)
	assert.Empty(t, chars)

	// The name is available, then create reserves it.
	free, err := p.NameAvailable(ctx, name)
	require.NoError(t, err)
	assert.True(t, free)

	id, err := p.CreateAccountCharacter(ctx, acct, name, "midgaard", "midgaard:room:temple", nil, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Now it shows up under the account and the name is taken.
	chars, err = p.AccountCharacters(ctx, acct)
	require.NoError(t, err)
	require.Len(t, chars, 1)
	assert.Equal(t, name, chars[0].Name)
	assert.Equal(t, "midgaard", chars[0].ZoneRef)

	free, err = p.NameAvailable(ctx, name)
	require.NoError(t, err)
	assert.False(t, free)

	// A duplicate create loses the unique-name guard with the typed error.
	_, err = p.CreateAccountCharacter(ctx, acct, name, "midgaard", "midgaard:room:temple", nil, nil)
	assert.True(t, errors.Is(err, ErrNameTaken), "duplicate name should return ErrNameTaken, got %v", err)
}

func TestOAuthIdentityRoundTrip(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	provider := "github"
	uid := "uid-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		// Identity rows cascade from the account; find the account first, then drop it.
		acct, found, _ := p.FindIdentity(context.Background(), provider, uid)
		if found {
			_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_identities WHERE account_id = $1`, acct)
			_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, acct)
		}
	})

	// An unknown identity is a miss (the website then creates an account).
	_, found, err := p.FindIdentity(ctx, provider, uid)
	require.NoError(t, err)
	require.False(t, found)

	// First-time sign-in: create the account + identity. email is informational, login is the display name.
	// bootstrapAdmin=false → an ordinary player account.
	acct, err := p.CreateAccountWithIdentity(ctx, provider, uid, "octo@example.com", "octocat", false)
	require.NoError(t, err)
	require.NotEmpty(t, acct)

	// A normal account defaults to the player tier (#27).
	tier, found, err := p.AccountTier(ctx, acct)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, TierPlayer, tier)

	// The same identity now resolves to that account (a returning sign-in — no new account).
	got, found, err := p.FindIdentity(ctx, provider, uid)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, acct, got)

	// The display name persisted.
	name, found, err := p.AccountDisplayName(ctx, acct)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "octocat", name)
}

// TestBootstrapAdminTierAndAudit (#27): creating an account with bootstrapAdmin=true grants the admin tier
// in the same transaction and writes an account_role_audit row with a NULL actor (system-granted).
func TestBootstrapAdminTierAndAudit(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	provider := "github"
	uid := "boot-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		acct, found, _ := p.FindIdentity(context.Background(), provider, uid)
		if found {
			_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_role_audit WHERE target_account = $1`, acct)
			_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_identities WHERE account_id = $1`, acct)
			_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, acct)
		}
	})

	acct, err := p.CreateAccountWithIdentity(ctx, provider, uid, "boss@example.com", "boss", true)
	require.NoError(t, err)

	tier, found, err := p.AccountTier(ctx, acct)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, TierAdmin, tier)

	// The grant was audited with a NULL actor (system/bootstrap) and new_tier=admin.
	var actor *string
	var newTier string
	err = p.pool.QueryRow(ctx,
		`SELECT actor_account, new_tier FROM account_role_audit WHERE target_account = $1`, acct).Scan(&actor, &newTier)
	require.NoError(t, err)
	assert.Nil(t, actor, "bootstrap grant should have a NULL actor (system-granted)")
	assert.Equal(t, TierAdmin, newTier)
}

// TestSetAccountTierAndResolve (#27 Slice 4): resolve a character name to its account, change the account's
// tier, and confirm the previous tier is returned + an actor-stamped audit row is written.
func TestSetAccountTierAndResolve(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	acct := uuid.NewString()
	_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status, tier) VALUES ($1, 'active', 'player')`, acct)
	require.NoError(t, err)
	name := "GatedTier-" + time.Now().Format("150405.000000")
	_, err = p.CreateAccountCharacter(ctx, acct, name, "midgaard", "midgaard:room:temple", nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_role_audit WHERE target_account = $1`, acct)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE account_id = $1`, acct)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, acct)
	})

	// The character name resolves to its owning account.
	got, found, err := p.AccountByCharacterName(ctx, name)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, acct, got)

	// Promote player -> builder (CAS base = the current player tier): returns the previous tier, updates the row.
	old, err := p.SetAccountTier(ctx, acct, acct, TierBuilder, TierPlayer)
	require.NoError(t, err)
	assert.Equal(t, TierPlayer, old)
	tier, _, err := p.AccountTier(ctx, acct)
	require.NoError(t, err)
	assert.Equal(t, TierBuilder, tier)

	// The change is audited with the acting account + the new tier.
	var actor, newTier string
	err = p.pool.QueryRow(ctx,
		`SELECT actor_account, new_tier FROM account_role_audit WHERE target_account = $1 AND actor_account IS NOT NULL`,
		acct).Scan(&actor, &newTier)
	require.NoError(t, err)
	assert.Equal(t, acct, actor)
	assert.Equal(t, TierBuilder, newTier)
}

// TestSetAccountTierAuditTrail (#130) covers the promote->demote round-trip + full audit-row CONTENTS + the
// APPEND semantics that TestSetAccountTierAndResolve leaves open (it asserts only actor+new_tier of a single
// promote): each SetAccountTier returns the tier it replaced AND writes exactly one account_role_audit row
// capturing actor/target/old/new; a subsequent DEMOTE appends a SECOND row (old=builder,new=player), so the
// audit is an ordered append log and the tier + audit write atomically (in one transaction).
func TestSetAccountTierAuditTrail(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	actor := uuid.NewString()
	target := uuid.NewString()
	for _, id := range []string{actor, target} {
		_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status, tier) VALUES ($1, 'active', 'player')`, id)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		ids := []string{actor, target}
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_role_audit WHERE target_account = ANY($1) OR actor_account = ANY($1)`, ids)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = ANY($1)`, ids)
	})

	// Promote player -> builder, then demote builder -> player: each returns the tier it REPLACED. The CAS base
	// is the tier each call expects to find (the prior line's outcome).
	old, err := p.SetAccountTier(ctx, actor, target, TierBuilder, TierPlayer)
	require.NoError(t, err)
	assert.Equal(t, TierPlayer, old, "promote should report the player tier it replaced")
	old, err = p.SetAccountTier(ctx, actor, target, TierPlayer, TierBuilder)
	require.NoError(t, err)
	assert.Equal(t, TierBuilder, old, "demote should report the builder tier it replaced")
	// A same-tier set whose CAS base matches still writes an audit row (the contract is "audit every WRITE",
	// not only real transitions), and reports the unchanged tier as old.
	old, err = p.SetAccountTier(ctx, actor, target, TierPlayer, TierPlayer)
	require.NoError(t, err)
	assert.Equal(t, TierPlayer, old, "a no-op set reports the unchanged tier")

	// The audit log APPENDED exactly one row per SetAccountTier call — incl. the no-op — with full old->new
	// contents. Order-independent (the `at` DEFAULT now() could tie for rapid ops) — assert the SET.
	type auditRow struct{ Actor, Target, Old, New string }
	rows, err := p.pool.Query(ctx,
		`SELECT actor_account, target_account, old_tier, new_tier FROM account_role_audit WHERE target_account = $1`, target)
	require.NoError(t, err)
	defer rows.Close()
	var got []auditRow
	for rows.Next() {
		var r auditRow
		require.NoError(t, rows.Scan(&r.Actor, &r.Target, &r.Old, &r.New))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 3, "exactly one audit row per SetAccountTier call, including the no-op")
	assert.Contains(t, got, auditRow{actor, target, TierPlayer, TierBuilder}, "the promote audit row (old=player,new=builder)")
	assert.Contains(t, got, auditRow{actor, target, TierBuilder, TierPlayer}, "the demote audit row (old=builder,new=player)")
	assert.Contains(t, got, auditRow{actor, target, TierPlayer, TierPlayer}, "the no-op audit row (audit every write)")

	// The UNIFIED #350 trail (character_audit) ALSO recorded all three tier changes, one row each. This is
	// the dedup-key regression guard: the idempotency index is (subject_id, event_kind, dedup_key), so if
	// tier_changed rows shared an empty dedup_key every write after the first would silently collide away —
	// each SetAccountTier must carry a distinct dedup_key (the account_role_audit row id).
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM character_audit WHERE subject_id = $1`, target)
	})
	trail, err := p.ListAuditForSubject(ctx, target, 50)
	require.NoError(t, err)
	tierRows := 0
	for _, e := range trail {
		if e.EventKind == world.AuditKindTierChanged {
			tierRows++
		}
	}
	assert.Equal(t, 3, tierRows, "the unified trail records EVERY tier change (distinct dedup_key per write)")
}

// TestSetAccountTierCASConflict (#165) pins the compare-and-set: a write whose expectedOldTier does NOT match
// the row's current tier is refused with ErrTierConflict, and it neither changes the tier NOR appends an audit
// row. This is what makes the account service's ceilings (evaluated against a tier read outside the row lock)
// binding rather than advisory — a concurrent promote that moved the base cannot be overwritten blind.
func TestSetAccountTierCASConflict(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	actor := uuid.NewString()
	target := uuid.NewString()
	for _, id := range []string{actor, target} {
		_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status, tier) VALUES ($1, 'active', 'player')`, id)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		ids := []string{actor, target}
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_role_audit WHERE target_account = ANY($1) OR actor_account = ANY($1)`, ids)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = ANY($1)`, ids)
	})

	// The target is 'player'. A write that expects 'builder' (a stale base) must conflict and no-op.
	observed, err := p.SetAccountTier(ctx, actor, target, TierBuilder, TierBuilder)
	require.ErrorIs(t, err, ErrTierConflict)
	assert.Equal(t, TierPlayer, observed, "the conflict returns the tier actually observed under the lock")

	tier, _, err := p.AccountTier(ctx, target)
	require.NoError(t, err)
	assert.Equal(t, TierPlayer, tier, "a CAS conflict must not change the tier")

	var n int
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT count(*) FROM account_role_audit WHERE target_account = $1`, target).Scan(&n))
	assert.Zero(t, n, "a CAS conflict must not append an audit row")

	// With the correct base the same write succeeds — proving the conflict was the base, not the request.
	old, err := p.SetAccountTier(ctx, actor, target, TierBuilder, TierPlayer)
	require.NoError(t, err)
	assert.Equal(t, TierPlayer, old)
}

// TestPendingChargenRoundTrip (Phase 14.8) proves the first-spawn chargen marker survives create -> load, and
// that the FIRST save clears it (chargen = NULL) — so the world applies the build exactly once.
func TestPendingChargenRoundTrip(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	acct := uuid.NewString()
	_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status) VALUES ($1, 'active')`, acct)
	require.NoError(t, err)
	name := "GatedChargen-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE account_id = $1`, acct)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, acct)
	})

	marker, err := json.Marshal(world.ChargenResult{
		Bundles: []string{"elf", "fighter"},
		Attrs:   map[string]float64{"strength": 15, "constitution": 13},
	})
	require.NoError(t, err)
	_, err = p.CreateAccountCharacter(ctx, acct, name, "midgaard", "midgaard:room:temple", nil, marker)
	require.NoError(t, err)

	// Load: the pending chargen comes back populated.
	snap, found, err := p.LoadCharacter(ctx, name)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, snap.PendingChargen, "a freshly-created character must carry its pending chargen")
	assert.Equal(t, []string{"elf", "fighter"}, snap.PendingChargen.Bundles)
	assert.Equal(t, 15.0, snap.PendingChargen.Attrs["strength"])

	// The first save (after the world applies + persists the built state) clears the marker.
	res, err := p.SaveCharacter(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, world.SaveApplied, res.Outcome)
	snap2, found, err := p.LoadCharacter(ctx, name)
	require.NoError(t, err)
	require.True(t, found)
	assert.Nil(t, snap2.PendingChargen, "the first save must clear the chargen marker (one-time application)")
}

// TestAccountColorPrefRoundTrip (#23) is the gated Postgres round-trip for the terminal color preference: a
// fresh account has NO stored preference (NULL => set=false, so the gate keeps its default); a write persists
// true/false; and it survives a read-back (the "reconnect" leg). It uses migration 00026's nullable column.
func TestAccountColorPrefRoundTrip(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	acct := uuid.NewString()
	_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status) VALUES ($1, 'active')`, acct)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, acct)
	})

	// Never set: the column is NULL, so set=false and the gate keeps its default.
	enabled, set, err := p.AccountColorPref(ctx, acct)
	require.NoError(t, err)
	assert.False(t, set, "a fresh account has no stored color preference")
	assert.False(t, enabled)

	// Persist color OFF, read it back (the reconnect leg): set=true, enabled=false.
	require.NoError(t, p.SetAccountColorPref(ctx, acct, false))
	enabled, set, err = p.AccountColorPref(ctx, acct)
	require.NoError(t, err)
	assert.True(t, set, "after a write the preference is stored")
	assert.False(t, enabled, "color off should read back as disabled")

	// Flip to ON: the write overwrites, read-back reflects it.
	require.NoError(t, p.SetAccountColorPref(ctx, acct, true))
	enabled, set, err = p.AccountColorPref(ctx, acct)
	require.NoError(t, err)
	assert.True(t, set)
	assert.True(t, enabled, "color on should read back as enabled")

	// An unknown account is a clean miss (set=false, no error) — mirrors the NULL default.
	_, set, err = p.AccountColorPref(ctx, uuid.NewString())
	require.NoError(t, err)
	assert.False(t, set, "an unknown account reports no stored preference")
}

// TestSetAccountTierCASConcurrent is the test that actually PROTECTS the FOR UPDATE serialization (#165): two
// goroutines race to change the SAME target from the SAME base tier. Exactly one must win; the other must get
// ErrTierConflict observing the winner's tier, and the audit log must hold exactly one row. A sequential
// base-mismatch test (TestSetAccountTierCASConflict) passes even if FOR UPDATE is deleted — this one does not,
// because without the lock both reads would see 'player' and both writes would land.
func TestSetAccountTierCASConcurrent(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	actor := uuid.NewString()
	target := uuid.NewString()
	for _, id := range []string{actor, target} {
		_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status, tier) VALUES ($1, 'active', 'player')`, id)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		ids := []string{actor, target}
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_role_audit WHERE target_account = ANY($1) OR actor_account = ANY($1)`, ids)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = ANY($1)`, ids)
	})

	// Both goroutines expect base 'player' but write DIFFERENT tiers, so the winner is identifiable.
	type res struct {
		old string
		err error
	}
	start := make(chan struct{})
	results := make(chan res, 2)
	for _, newTier := range []string{TierBuilder, TierAdmin} {
		newTier := newTier
		go func() {
			<-start // barrier: maximize the odds both reach the SELECT together
			old, err := p.SetAccountTier(ctx, actor, target, newTier, TierPlayer)
			results <- res{old, err}
		}()
	}
	close(start)
	r1, r2 := <-results, <-results

	// Exactly one NoError and one ErrTierConflict.
	winners, conflicts := 0, 0
	for _, r := range []res{r1, r2} {
		switch {
		case r.err == nil:
			winners++
			// The WINNER replaced the 'player' base it expected.
			assert.Equal(t, TierPlayer, r.old, "the winner's CAS base was player")
		case errors.Is(r.err, ErrTierConflict):
			conflicts++
			// The LOSER blocked on FOR UPDATE and, after the winner committed, re-read the row under the lock:
			// it observes the WINNER's tier (builder or admin), NOT the stale 'player' it expected — that
			// moved base is exactly why its CAS conflicts.
			assert.Contains(t, []string{TierBuilder, TierAdmin}, r.old,
				"the conflict must observe the winner's committed tier under the lock, not the stale base")
		default:
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	assert.Equal(t, 1, winners, "exactly one writer may win the CAS race")
	assert.Equal(t, 1, conflicts, "the other writer must lose with ErrTierConflict (FOR UPDATE serialization)")

	// The row holds the winner's tier, and the audit log has exactly ONE row (the loser wrote nothing).
	tier, _, err := p.AccountTier(ctx, target)
	require.NoError(t, err)
	assert.Contains(t, []string{TierBuilder, TierAdmin}, tier)

	var n int
	require.NoError(t, p.pool.QueryRow(ctx, `SELECT count(*) FROM account_role_audit WHERE target_account = $1`, target).Scan(&n))
	assert.Equal(t, 1, n, "only the winning write may append an audit row")
}

// TestSetAccountTierSystem (#108) is the break-glass write: it forces the tier unconditionally and audits with
// a NULL (system) actor — the recovery path from a last-admin lockout. It must land regardless of the current
// tier and record the change without an actor account (like the bootstrap grant).
func TestSetAccountTierSystem(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	target := uuid.NewString()
	_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status, tier) VALUES ($1, 'active', 'player')`, target)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_role_audit WHERE target_account = $1`, target)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, target)
	})

	// Force player -> admin (the recovery). Returns the prior tier, writes the new one.
	old, err := p.SetAccountTierSystem(ctx, target, TierAdmin)
	require.NoError(t, err)
	assert.Equal(t, TierPlayer, old)
	tier, _, err := p.AccountTier(ctx, target)
	require.NoError(t, err)
	assert.Equal(t, TierAdmin, tier)

	// The audit row has a NULL actor (system-granted) and the old->new contents.
	var actor *string
	var oldTier, newTier string
	err = p.pool.QueryRow(ctx,
		`SELECT actor_account, old_tier, new_tier FROM account_role_audit WHERE target_account = $1`,
		target).Scan(&actor, &oldTier, &newTier)
	require.NoError(t, err)
	assert.Nil(t, actor, "a break-glass write must audit with a NULL (system) actor")
	assert.Equal(t, TierPlayer, oldTier)
	assert.Equal(t, TierAdmin, newTier)

	// An unknown account is an error (nothing to recover).
	if _, err := p.SetAccountTierSystem(ctx, uuid.NewString(), TierAdmin); err == nil {
		t.Fatal("set-tier on an unknown account must error")
	}
}

// TestSetAccountTierSystemVsInGameCAS pins the concurrency claim in SetAccountTierSystem's doc comment (#108):
// the break-glass system write and an in-game SetAccountTier serialize on the row's FOR UPDATE lock, and the
// recovery is never silently clobbered. Both orderings are exercised.
func TestSetAccountTierSystemVsInGameCAS(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	mkAccount := func(tier string) string {
		id := uuid.NewString()
		_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status, tier) VALUES ($1, 'active', $2)`, id, tier)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_role_audit WHERE target_account = $1`, id)
			_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, id)
		})
		return id
	}
	actor := mkAccount(TierAdmin)

	// Ordering 1: the SYSTEM write commits, then an in-game CAS with a now-stale base must conflict (not
	// clobber the recovery). Model it sequentially — the CAS base is the tier read BEFORE the system write.
	{
		target := mkAccount(TierPlayer)
		staleBase := TierPlayer // what an in-game actor would have read before recovery landed
		if _, err := p.SetAccountTierSystem(ctx, target, TierAdmin); err != nil {
			t.Fatalf("system recovery write failed: %v", err)
		}
		if _, err := p.SetAccountTier(ctx, actor, target, TierBuilder, staleBase); !errors.Is(err, ErrTierConflict) {
			t.Fatalf("an in-game CAS on a stale base must conflict after a system write, got %v", err)
		}
		tier, _, _ := p.AccountTier(ctx, target)
		assert.Equal(t, TierAdmin, tier, "the system recovery must survive a losing in-game CAS")
	}

	// Ordering 2: an in-game promote commits, then the system write forces UNCONDITIONALLY over it.
	{
		target := mkAccount(TierPlayer)
		if _, err := p.SetAccountTier(ctx, actor, target, TierBuilder, TierPlayer); err != nil {
			t.Fatalf("in-game promote failed: %v", err)
		}
		old, err := p.SetAccountTierSystem(ctx, target, TierAdmin)
		require.NoError(t, err)
		assert.Equal(t, TierBuilder, old, "the system write observes the in-game tier it forces over")
		tier, _, _ := p.AccountTier(ctx, target)
		assert.Equal(t, TierAdmin, tier, "the system write wins unconditionally")
	}
}
