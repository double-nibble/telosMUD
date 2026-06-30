package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// account.go — Phase-14 account/character store methods backing telos-account (docs/ACCOUNT.md). These are
// the queries the Account gRPC service runs: the character list/create for an account, name reservation, and
// (later slices) the OAuth identity + passphrase + SSH-key lookups. The gate/world never call these directly
// — only telos-account does.

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

// isUniqueViolation reports whether err is a Postgres unique-constraint violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// --- Phase 14.5 passphrase auth (account_auth) -----------------------------------------------------------

// CharacterAccount resolves a character name to its owning account id. found=false when the name is unknown
// or the character has no account (a pre-14 stub character). The gate's passphrase login resolves the
// account this way (a player logs in by character name, not account id).
func (p *Pool) CharacterAccount(ctx context.Context, name string) (string, bool, error) {
	var acct *string
	err := p.pool.QueryRow(ctx,
		`SELECT account_id FROM characters WHERE name = $1 AND deleted_at IS NULL`, name).Scan(&acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: resolve account for %q: %w", name, err)
	}
	if acct == nil {
		return "", false, nil // a character with no account (pre-14 stub)
	}
	return *acct, true, nil
}

// PassphraseAuth is the account_auth row: the Argon2id hash (empty if unset), the consecutive-failure count,
// and the lockout deadline (zero if not locked). found=false when no row exists yet (no passphrase ever set).
type PassphraseAuth struct {
	Hash           string
	FailedAttempts int
	LockedUntil    time.Time
}

// AccountAuth reads an account's auth row. found=false => no passphrase configured.
func (p *Pool) AccountAuth(ctx context.Context, accountID string) (PassphraseAuth, bool, error) {
	var a PassphraseAuth
	var hash *string
	var locked *time.Time
	err := p.pool.QueryRow(ctx,
		`SELECT passphrase_hash, failed_attempts, locked_until FROM account_auth WHERE account_id = $1`,
		accountID).Scan(&hash, &a.FailedAttempts, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return PassphraseAuth{}, false, nil
	}
	if err != nil {
		return PassphraseAuth{}, false, fmt.Errorf("store: read account_auth %s: %w", accountID, err)
	}
	if hash != nil {
		a.Hash = *hash
	}
	if locked != nil {
		a.LockedUntil = *locked
	}
	return a, true, nil
}

// SetPassphraseHash upserts an account's passphrase hash, resetting the failure/lockout state (setting a new
// passphrase clears any prior lockout).
func (p *Pool) SetPassphraseHash(ctx context.Context, accountID, hash string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO account_auth (account_id, passphrase_hash, failed_attempts, locked_until, updated_at)
		 VALUES ($1, $2, 0, NULL, now())
		 ON CONFLICT (account_id) DO UPDATE
		   SET passphrase_hash = EXCLUDED.passphrase_hash, failed_attempts = 0, locked_until = NULL, updated_at = now()`,
		accountID, hash)
	if err != nil {
		return fmt.Errorf("store: set passphrase %s: %w", accountID, err)
	}
	return nil
}

// RecordAuthFailure increments the consecutive-failure count and, when it reaches lockAfter, sets a lockout
// until now+lockFor. It returns the new failure count. A row is created if absent (a failure before any
// passphrase row — defensive; in practice AccountAuth already created reasoning).
func (p *Pool) RecordAuthFailure(ctx context.Context, accountID string, lockAfter int, lockFor time.Duration) (int, error) {
	var failed int
	err := p.pool.QueryRow(ctx,
		`INSERT INTO account_auth (account_id, failed_attempts, updated_at)
		 VALUES ($1, 1, now())
		 ON CONFLICT (account_id) DO UPDATE
		   SET failed_attempts = account_auth.failed_attempts + 1,
		       locked_until = CASE WHEN account_auth.failed_attempts + 1 >= $2 THEN now() + $3::interval ELSE account_auth.locked_until END,
		       updated_at = now()
		 RETURNING failed_attempts`,
		accountID, lockAfter, fmt.Sprintf("%d milliseconds", lockFor.Milliseconds())).Scan(&failed)
	if err != nil {
		return 0, fmt.Errorf("store: record auth failure %s: %w", accountID, err)
	}
	return failed, nil
}

// ResetAuthFailures clears the failure count + lockout after a successful login.
func (p *Pool) ResetAuthFailures(ctx context.Context, accountID string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE account_auth SET failed_attempts = 0, locked_until = NULL, updated_at = now() WHERE account_id = $1`,
		accountID)
	if err != nil {
		return fmt.Errorf("store: reset auth failures %s: %w", accountID, err)
	}
	return nil
}

// --- Phase 14.6 SSH public-key auth (ssh_keys) -----------------------------------------------------------

// ResolveSSHKey maps an SSH key fingerprint (SHA256) to its owning account. found=false for an unknown key.
func (p *Pool) ResolveSSHKey(ctx context.Context, fingerprint string) (string, bool, error) {
	var acct string
	err := p.pool.QueryRow(ctx, `SELECT account_id FROM ssh_keys WHERE fingerprint = $1`, fingerprint).Scan(&acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: resolve ssh key: %w", err)
	}
	return acct, true, nil
}

// AddSSHKey registers an SSH public key for an account (the website key-management path). Idempotent on the
// fingerprint PK (re-adding the same key updates its label/owner).
func (p *Pool) AddSSHKey(ctx context.Context, accountID, fingerprint, pubkey, label string) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO ssh_keys (fingerprint, account_id, pubkey, label, added_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (fingerprint) DO UPDATE SET account_id = EXCLUDED.account_id, pubkey = EXCLUDED.pubkey, label = EXCLUDED.label`,
		fingerprint, accountID, pubkey, nullStr(label))
	if err != nil {
		return fmt.Errorf("store: add ssh key: %w", err)
	}
	return nil
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

// CreateAccountWithIdentity creates a NEW account + its first OAuth identity in one transaction (a first-time
// sign-in). email is informational only — never an identity key (no auto-merge by email). Returns the new
// account id.
func (p *Pool) CreateAccountWithIdentity(ctx context.Context, provider, providerUID, email, displayName string) (string, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	acct := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO accounts (id, status, display_name) VALUES ($1, 'active', $2)`,
		acct, nullStr(displayName)); err != nil {
		return "", fmt.Errorf("store: create account: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO account_identities (provider, provider_uid, account_id, email) VALUES ($1, $2, $3, $4)`,
		provider, providerUID, acct, nullStr(email)); err != nil {
		return "", fmt.Errorf("store: create identity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit: %w", err)
	}
	return acct.String(), nil
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
