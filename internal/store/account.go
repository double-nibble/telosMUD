package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
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

// CreateAccountCharacter creates a character OWNED by an account with the given starting location + initial
// state JSON (the chargen result). It returns ErrNameTaken on a unique-name conflict so the caller can
// surface "that name was just taken" rather than a generic error.
func (p *Pool) CreateAccountCharacter(ctx context.Context, accountID, name, zoneRef, roomRef string, state []byte) (string, error) {
	if len(state) == 0 {
		state = []byte("{}")
	}
	id := uuid.New()
	_, err := p.pool.Exec(ctx,
		`INSERT INTO characters (id, account_id, name, zone_ref, room_ref, state_version, state, last_login_at)
		 VALUES ($1, $2, $3, $4, $5, 0, $6, now())`,
		id, accountID, name, nullStr(zoneRef), nullStr(roomRef), state)
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
