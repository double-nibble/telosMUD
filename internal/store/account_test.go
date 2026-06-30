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
	acct, err := p.CreateAccountWithIdentity(ctx, provider, uid, "octo@example.com", "octocat")
	require.NoError(t, err)
	require.NotEmpty(t, acct)

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
