package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/double-nibble/telosmud/internal/world"
)

// state_version is a monotonic counter: a `bigint` (int64) column the app models as uint64. These
// two helpers make the type-boundary conversions explicit + bounded (the gosec G115 requirement),
// and defensively floor/cap so neither direction can ever produce a value the CAS would misread.

// nonNegU64 clamps a DB-sourced signed state_version to uint64. The column defaults to 0 and only
// ever increments, so a negative is impossible barring corruption — where 0 is the safe floor.
func nonNegU64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// stateVersionParam narrows the app's uint64 state_version to the int64 the bigint CAS predicate
// binds. A counter incremented by 1 per save cannot realistically approach MaxInt64; cap defensively
// so the conversion never wraps to a negative the `WHERE state_version = $n` predicate would misread.
func stateVersionParam(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// character.go is the pgx implementation of world.CharacterStore against the `characters` table
// (slice 4.1 created it; docs/PERSISTENCE.md §2, docs/PHASE4-PLAN.md §2). The table is the
// canonical durable-state shape: engine-universal relational columns (id/name/zone_ref/room_ref/
// state_version) plus one `state` JSONB carrying ALL content-defined state (== the PlayerSnapshot
// shape). No per-stat column — adding an attribute is a content write into `state`, never a
// migration (the pillar). All three methods do blocking pool I/O and run OFF the zone goroutine
// (the login read and the async saver), so synchronous calls are fine.
//
// The store maps between world.CharSnapshot (the world-side DTO) and the row columns + the JSONB
// `state`. The world package owns the runtime<->DTO mapping (character.go dumpCharacter/
// loadCharacter); THIS file owns only the DTO<->row mapping, keeping the on-disk format independent
// of both.

// The JSONB `state` column round-trips world.StateJSON directly (it carries the json tags), so
// there is no re-declared row struct here — the world DTO IS the wire format, and the store maps
// only the relational columns. The round-trip test guards that the two stay convergent.

// LoadCharacter reads the durable snapshot for name (CITEXT, so case-insensitive), excluding
// soft-deleted rows. found=false (nil error) when no live row exists — a brand-new character.
func (p *Pool) LoadCharacter(ctx context.Context, name string) (world.CharSnapshot, bool, error) {
	var (
		id           uuid.UUID
		zoneRef      *string
		roomRef      *string
		stateVersion int64
		stateJSON    []byte
		chargenJSON  []byte
	)
	err := p.pool.QueryRow(ctx,
		`SELECT id, zone_ref, room_ref, state_version, state, chargen
		   FROM characters
		  WHERE name = $1 AND deleted_at IS NULL`, name).
		Scan(&id, &zoneRef, &roomRef, &stateVersion, &stateJSON, &chargenJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return world.CharSnapshot{}, false, nil
	}
	if err != nil {
		return world.CharSnapshot{}, false, fmt.Errorf("store: load character %q: %w", name, err)
	}
	snap := world.CharSnapshot{
		PID:          world.PersistID(id.String()),
		Name:         name,
		ZoneRef:      derefStr(zoneRef),
		RoomRef:      derefStr(roomRef),
		StateVersion: nonNegU64(stateVersion),
	}
	if len(stateJSON) > 0 {
		if err := json.Unmarshal(stateJSON, &snap.State); err != nil {
			return world.CharSnapshot{}, false, fmt.Errorf("store: unmarshal character %q state: %w", name, err)
		}
	}
	// Pending chargen (Phase 14.8): a not-yet-spawned content-built character carries its chosen bundles +
	// bought attributes here; the world applies them on first spawn and the next save nulls the column.
	if len(chargenJSON) > 0 {
		var cg world.ChargenResult
		if err := json.Unmarshal(chargenJSON, &cg); err != nil {
			return world.CharSnapshot{}, false, fmt.Errorf("store: unmarshal character %q chargen: %w", name, err)
		}
		snap.PendingChargen = &cg
	}
	return snap, true, nil
}

// CreateCharacter inserts a fresh row, minting the PersistID (a v4 UUID) and starting at
// state_version 0 with an empty state. account_id is left NULL (Phase 13 auth). It returns an
// error if the name already exists (the CITEXT UNIQUE constraint) — the caller treats a brand-new
// login that races to create as "load instead", but for slice 4.2 the login path only creates
// when LoadCharacter found nothing, so a collision here is a genuine concurrent-create race.
func (p *Pool) CreateCharacter(ctx context.Context, name, zoneRef, roomRef string) (world.PersistID, error) {
	id := uuid.New()
	emptyState, _ := json.Marshal(world.StateJSON{})
	_, err := p.pool.Exec(ctx,
		`INSERT INTO characters (id, name, zone_ref, room_ref, state_version, state, last_login_at)
		 VALUES ($1, $2, $3, $4, 0, $5, now())`,
		id, name, nullStr(zoneRef), nullStr(roomRef), emptyState)
	if err != nil {
		return "", fmt.Errorf("store: create character %q: %w", name, err)
	}
	return world.PersistID(id.String()), nil
}

// SaveCharacter writes snap with an optimistic-concurrency CAS on state_version
// (docs/PERSISTENCE.md §7): the UPDATE applies only WHERE state_version = the value the snapshot
// was dumped at, then bumps it and RETURNs the new value. Zero rows updated => a stale writer (a
// mis-fired handoff, a zombie/duplicated owner) lost the race; ok=false (nil error) tells the
// caller to reconcile rather than force the write. This is the backstop behind the directory epoch
// and the fence that protects a genuinely-rehydrated player on a new shard from a zombie original.
func (p *Pool) SaveCharacter(ctx context.Context, snap world.CharSnapshot) (uint64, bool, error) {
	if snap.PID == "" {
		return 0, false, fmt.Errorf("store: save character %q: missing persist id", snap.Name)
	}
	id, err := uuid.Parse(string(snap.PID))
	if err != nil {
		return 0, false, fmt.Errorf("store: save character %q: bad persist id %q: %w", snap.Name, snap.PID, err)
	}
	stateJSON, err := json.Marshal(snap.State)
	if err != nil {
		return 0, false, fmt.Errorf("store: marshal character %q state: %w", snap.Name, err)
	}
	var newVersion int64
	err = p.pool.QueryRow(ctx,
		// chargen = NULL clears the Phase-14.8 first-spawn marker in the SAME write that persists the built
		// state — so application + clear are atomic from the DB's view: a crash before this save re-applies
		// from the still-empty state (the additive racial mods never double-apply).
		`UPDATE characters
		    SET state = $1,
		        zone_ref = $2,
		        room_ref = $3,
		        state_version = state_version + 1,
		        chargen = NULL,
		        last_saved_at = now()
		  WHERE id = $4 AND state_version = $5
		 RETURNING state_version`,
		stateJSON, nullStr(snap.ZoneRef), nullStr(snap.RoomRef), id, stateVersionParam(snap.StateVersion)).
		Scan(&newVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		// CAS lost: the stored state_version moved past snap.StateVersion (or the row is gone).
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: save character %q: %w", snap.Name, err)
	}
	return nonNegU64(newVersion), true, nil
}

// derefStr returns the pointed-to string, or "" for a SQL NULL.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Compile-time assertion that *Pool satisfies world.CharacterStore.
var _ world.CharacterStore = (*Pool)(nil)
