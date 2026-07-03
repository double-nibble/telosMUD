package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/double-nibble/telosmud/internal/world"
)

// account.go — account/character store methods backing telos-account (docs/ACCOUNT.md). These are
// the queries the Account gRPC service and the OAuth broker run: the character list/create for an account,
// name reservation, and the OAuth identity lookups (auth is OAuth-only since Phase 15). The gate/world never
// call these directly — only telos-account does.

// CharacterSummary is the account-facing summary of a character (the select menu / dashboard list).
type CharacterSummary struct {
	ID      string
	Name    string
	ZoneRef string
	RoomRef string
}

// AccountCharacters returns the (non-deleted) characters owned by an account, name-ordered.
func (p *Pool) AccountCharacters(ctx context.Context, accountID string) ([]CharacterSummary, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, COALESCE(zone_ref,''), COALESCE(room_ref,'')
		   FROM characters
		  WHERE account_id = $1 AND deleted_at IS NULL
		  ORDER BY name`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list characters for account %s: %w", accountID, err)
	}
	defer rows.Close()
	var out []CharacterSummary
	for rows.Next() {
		var c CharacterSummary
		if err := rows.Scan(&c.ID, &c.Name, &c.ZoneRef, &c.RoomRef); err != nil {
			return nil, fmt.Errorf("store: scan character: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NameAvailable reports whether a character name is free (the CITEXT UNIQUE constraint is the real guard;
// this is the pre-check the chargen flow shows the user before they commit).
func (p *Pool) NameAvailable(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := p.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM characters WHERE name = $1)`, name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: name-available check %q: %w", name, err)
	}
	return !exists, nil
}

// ErrNameTaken is returned by CreateAccountCharacter when the unique-name constraint rejects the insert (a
// race lost between the NameAvailable pre-check and the commit).
var ErrNameTaken = errors.New("store: character name already taken")

// CreateAccountCharacter creates a character OWNED by an account with the given starting location. state is
// the initial content state JSON (normally empty `{}` — the world builds the character on first spawn); chargen
// is the Phase-14.8 pending-chargen marker (the chosen bundles + bought attributes the world applies on first
// spawn), or nil/empty for a character that needs no build. It returns ErrNameTaken on a unique-name conflict
// so the caller can surface "that name was just taken" rather than a generic error.
func (p *Pool) CreateAccountCharacter(ctx context.Context, accountID, name, zoneRef, roomRef string, state, chargen []byte) (string, error) {
	if len(state) == 0 {
		state = []byte("{}")
	}
	id := uuid.New()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO characters (id, account_id, name, zone_ref, room_ref, state_version, state, chargen, last_login_at)
		 VALUES ($1, $2, $3, $4, $5, 0, $6, $7, now())`,
		id, accountID, name, nullStr(zoneRef), nullStr(roomRef), state, nullBytes(chargen))
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrNameTaken
		}
		return "", fmt.Errorf("store: create character %q for account %s: %w", name, accountID, err)
	}
	return id.String(), nil
}

// CreateCharacterWithChargen creates an account-owned character carrying a Phase-14.8 first-spawn chargen
// marker (the chosen bundles + bought attribute values). It marshals the world-side ChargenResult into the
// chargen column so the caller (telos-account) needs no knowledge of that serialization. An empty bundles+attrs
// is allowed (a bare character with no build). Returns ErrNameTaken on a unique-name conflict.
func (p *Pool) CreateCharacterWithChargen(ctx context.Context, accountID, name, zoneRef, roomRef string, bundles []string, attrs map[string]float64) (string, error) {
	var marker []byte
	if len(bundles) > 0 || len(attrs) > 0 {
		b, err := json.Marshal(world.ChargenResult{Bundles: bundles, Attrs: attrs})
		if err != nil {
			return "", fmt.Errorf("store: marshal chargen marker for %q: %w", name, err)
		}
		marker = b
	}
	return p.CreateAccountCharacter(ctx, accountID, name, zoneRef, roomRef, nil, marker)
}

// isUniqueViolation reports whether err is a Postgres unique-constraint violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// --- Phase 14.7 OAuth identities (account_identities) -----------------------------------------------------

// FindIdentity resolves an OAuth (provider, provider_uid) to its account. found=false for a new identity.
func (p *Pool) FindIdentity(ctx context.Context, provider, providerUID string) (string, bool, error) {
	var acct string
	err := p.pool.QueryRow(ctx,
		`SELECT account_id FROM account_identities WHERE provider = $1 AND provider_uid = $2`,
		provider, providerUID).Scan(&acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: find identity %s/%s: %w", provider, providerUID, err)
	}
	return acct, true, nil
}

// The builder trust tiers (#27/#97). Canonical strings — the accounts.tier CHECK constraint (migration
// 00019) is the source of truth; keep these in sync with it. Ordered player < builder < admin.
const (
	TierPlayer  = "player"
	TierBuilder = "builder"
	TierAdmin   = "admin"
)

// CreateAccountWithIdentity creates a NEW account + its first OAuth identity in one transaction (a first-time
// sign-in). email is informational only — never an identity key (no auto-merge by email). bootstrapAdmin
// (the config-pin match, decided by the caller) creates the account at the admin tier and records the grant
// in account_role_audit with a NULL actor (system granted) — all in the same transaction, so a crash can't
// leave a half-granted admin. Returns the new account id.
func (p *Pool) CreateAccountWithIdentity(ctx context.Context, provider, providerUID, email, displayName string, bootstrapAdmin bool) (string, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tier := TierPlayer
	if bootstrapAdmin {
		tier = TierAdmin
	}
	acct := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO accounts (id, status, display_name, tier) VALUES ($1, 'active', $2, $3)`,
		acct, nullStr(displayName), tier); err != nil {
		return "", fmt.Errorf("store: create account: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_identities (provider, provider_uid, account_id, email) VALUES ($1, $2, $3, $4)`,
		provider, providerUID, acct, nullStr(email)); err != nil {
		return "", fmt.Errorf("store: create identity: %w", err)
	}
	if bootstrapAdmin {
		if _, err := tx.Exec(ctx,
			`INSERT INTO account_role_audit (id, actor_account, target_account, old_tier, new_tier)
			 VALUES ($1, NULL, $2, NULL, $3)`,
			uuid.New(), acct, TierAdmin); err != nil {
			return "", fmt.Errorf("store: audit bootstrap admin: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit: %w", err)
	}
	return acct.String(), nil
}

// AccountTier returns an account's trust tier (player/builder/admin). found=false for an unknown account.
// telos-account reads this to sign the tier into the session assertion (the world trusts it offline).
func (p *Pool) AccountTier(ctx context.Context, accountID string) (string, bool, error) {
	var tier string
	err := p.pool.QueryRow(ctx, `SELECT tier FROM accounts WHERE id = $1`, accountID).Scan(&tier)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: account tier %s: %w", accountID, err)
	}
	return tier, true, nil
}

// AccountDisplayName returns an account's display name (may be empty). found=false for an unknown account.
func (p *Pool) AccountDisplayName(ctx context.Context, accountID string) (string, bool, error) {
	var name *string
	err := p.pool.QueryRow(ctx, `SELECT display_name FROM accounts WHERE id = $1`, accountID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: account %s: %w", accountID, err)
	}
	if name == nil {
		return "", true, nil
	}
	return *name, true, nil
}
