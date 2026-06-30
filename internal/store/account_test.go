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
