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

	// Promote player -> builder: returns the previous tier, updates the row.
	old, err := p.SetAccountTier(ctx, acct, acct, TierBuilder)
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
	_, ok, err := p.SaveCharacter(ctx, snap)
	require.NoError(t, err)
	require.True(t, ok)
	snap2, found, err := p.LoadCharacter(ctx, name)
	require.NoError(t, err)
	require.True(t, found)
	assert.Nil(t, snap2.PendingChargen, "the first save must clear the chargen marker (one-time application)")
}
