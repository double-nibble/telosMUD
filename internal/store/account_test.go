package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	id, err := p.CreateAccountCharacter(ctx, acct, name, "midgaard", "midgaard:room:temple", nil)
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
	_, err = p.CreateAccountCharacter(ctx, acct, name, "midgaard", "midgaard:room:temple", nil)
	assert.True(t, errors.Is(err, ErrNameTaken), "duplicate name should return ErrNameTaken, got %v", err)
}

func TestAccountAuthPassphraseAndLockout(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	acct := uuid.NewString()
	_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status) VALUES ($1, 'active')`, acct)
	require.NoError(t, err)
	name := "GatedAuthChar-" + time.Now().Format("150405.000000")
	_, err = p.CreateAccountCharacter(ctx, acct, name, "midgaard", "midgaard:room:temple", nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM account_auth WHERE account_id = $1`, acct)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE account_id = $1`, acct)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, acct)
	})

	// The character resolves to its account.
	got, found, err := p.CharacterAccount(ctx, name)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, acct, got)

	// No auth row yet.
	_, found, err = p.AccountAuth(ctx, acct)
	require.NoError(t, err)
	assert.False(t, found)

	// Set a hash, then read it back (failed=0, not locked).
	require.NoError(t, p.SetPassphraseHash(ctx, acct, "phc-hash"))
	a, found, err := p.AccountAuth(ctx, acct)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "phc-hash", a.Hash)
	assert.Equal(t, 0, a.FailedAttempts)
	assert.True(t, a.LockedUntil.IsZero())

	// Record failures up to the threshold: the 3rd sets a lockout (lockAfter=3 here).
	for i := 1; i <= 3; i++ {
		n, err := p.RecordAuthFailure(ctx, acct, 3, time.Hour)
		require.NoError(t, err)
		assert.Equal(t, i, n)
	}
	a, _, err = p.AccountAuth(ctx, acct)
	require.NoError(t, err)
	assert.Equal(t, 3, a.FailedAttempts)
	assert.False(t, a.LockedUntil.IsZero(), "the account should be locked after reaching the threshold")
	assert.True(t, a.LockedUntil.After(time.Now()), "the lockout should be in the future")

	// Reset clears failures + lockout.
	require.NoError(t, p.ResetAuthFailures(ctx, acct))
	a, _, err = p.AccountAuth(ctx, acct)
	require.NoError(t, err)
	assert.Equal(t, 0, a.FailedAttempts)
	assert.True(t, a.LockedUntil.IsZero())
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

func TestSSHKeyResolve(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	acct := uuid.NewString()
	_, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status) VALUES ($1, 'active')`, acct)
	require.NoError(t, err)
	fp := "SHA256:gated-test-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM ssh_keys WHERE account_id = $1`, acct)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, acct)
	})

	// Unknown key -> not found.
	_, found, err := p.ResolveSSHKey(ctx, fp)
	require.NoError(t, err)
	assert.False(t, found)

	// Add it, then it resolves to the account; re-adding (same fingerprint) is idempotent.
	require.NoError(t, p.AddSSHKey(ctx, acct, fp, "ssh-ed25519 AAAA...", "laptop"))
	got, found, err := p.ResolveSSHKey(ctx, fp)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, acct, got)
	require.NoError(t, p.AddSSHKey(ctx, acct, fp, "ssh-ed25519 AAAA...", "laptop-renamed"))
}
