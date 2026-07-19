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
	var ownerEpoch int64
	err := p.pool.QueryRow(ctx,
		`SELECT id, zone_ref, room_ref, state_version, owner_epoch, state, chargen
		   FROM characters
		  WHERE name = $1 AND deleted_at IS NULL`, name).
		Scan(&id, &zoneRef, &roomRef, &stateVersion, &ownerEpoch, &stateJSON, &chargenJSON)
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
		// The ownership epoch is a COLUMN, not a field inside the `state` JSONB, so it does not free-ride
		// on the state round-trip: every SELECT list that omits it silently returns 0, and a 0 read here
		// would make the very next save claim to be unfenced. This repo has shipped a silently-dropped
		// field through a store round-trip three times; the tier round-trip test guards this one.
		OwnerEpoch: nonNegU64(ownerEpoch),
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
	// Character insert + its creation-audit row in ONE transaction (#350). This is the dev/login create
	// path (no owning account), so the actor is the SYSTEM (NULL actor_id) — nobody promoted or purchased
	// this character; the engine created it on first login.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO characters (id, name, zone_ref, room_ref, state_version, state, last_login_at)
		 VALUES ($1, $2, $3, $4, 0, $5, now())`,
		id, name, nullStr(zoneRef), nullStr(roomRef), emptyState); err != nil {
		return "", fmt.Errorf("store: create character %q: %w", name, err)
	}
	if err := appendAuditTx(ctx, tx, world.AuditEvent{
		SubjectType: world.AuditSubjectCharacter,
		SubjectID:   id.String(),
		SubjectName: name,
		ActorType:   world.AuditActorSystem,
		EventKind:   world.AuditKindCharacterCreated,
		Payload:     world.AuditPayload(map[string]any{"zone": zoneRef, "room": roomRef}),
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit: %w", err)
	}
	return world.PersistID(id.String()), nil
}

// maxOwnerEpoch bounds the ownership epoch a claim will accept as a floor. See ClaimCharacter for
// why an unbounded floor is a permanent brick rather than a transient error. 2^48 is ~2.8e14 — many
// orders of magnitude above any reachable claim count — and stays under 2^53 so the value round-trips
// exactly through the Lua double the checkpoint guard compares it as.
//
// OPERATOR RECOVERY, if a character is ever pinned anyway (a hand-edited row, a pre-ceiling artifact):
//
//	UPDATE characters SET owner_epoch = 0 WHERE name = '<name>';
//
// Safe when no session for that character is live anywhere in the fleet; the next login re-arms the
// fence by claiming 1.
const maxOwnerEpoch = 1 << 48

// ClaimCharacter mints the next ownership epoch for id, atomically (#432): the single UPDATE takes
// the row lock, raises owner_epoch to at least `floor`, adds one, and RETURNs it. Concurrent
// claimants therefore serialize on the row and receive DISTINCT, strictly increasing values — which
// is the entire property the fence rests on. A read-then-bump would hand two logins the same epoch
// and both would satisfy `owner_epoch <= k` at save time, restoring the bug in a shape that looks
// fixed.
//
// `floor` only ever RAISES the mint (it is inside GREATEST, not an assignment). That asymmetry is
// deliberate: the caller passes the directory's recorded placement epoch, and the directory is
// evictable Redis. A stale or missing directory value can cost us nothing — the row's own high-water
// mark still dominates — whereas an assignment would let an evicted directory hand a live character
// an epoch BELOW the one its last owner held, which is the wedge shape (a session whose every save is
// refused forever).
func (p *Pool) ClaimCharacter(ctx context.Context, pid world.PersistID, floor uint64) (uint64, error) {
	id, err := uuid.Parse(string(pid))
	if err != nil {
		return 0, fmt.Errorf("store: claim character: bad persist id %q: %w", pid, err)
	}
	// Reject an absurd floor rather than storing it. The floor's outside source is the directory —
	// evictable Redis, and writable by anything with access to it — and `GREATEST` would pin the row's
	// epoch at that value PERMANENTLY, after which every `+1` overflows BIGINT, every claim errors, and
	// the character can never be logged in again. There is no in-code recovery from that, so the
	// boundary refuses it instead. maxOwnerEpoch is far above any reachable count (one claim per login
	// and per cross-shard move, for a uint64 counter) and below 2^53, so the value also survives the
	// Lua double the checkpoint guard compares it as.
	if floor > maxOwnerEpoch {
		return 0, fmt.Errorf("store: claim character %q: floor %d exceeds the sane ceiling %d "+
			"(a corrupted directory epoch; refusing to pin the row)", pid, floor, maxOwnerEpoch)
	}
	var epoch int64
	err = p.pool.QueryRow(ctx,
		// last_login_at is deliberately NOT touched here. This mint is shared with the cross-shard
		// handoff path, and stamping it there would quietly redefine the column as "last moved between
		// shards" — wrong for exactly the active players any inactivity/retention query cares about.
		`UPDATE characters
		    SET owner_epoch = GREATEST(owner_epoch, $2) + 1
		  WHERE id = $1 AND deleted_at IS NULL
		 RETURNING owner_epoch`, id, stateVersionParam(floor)).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, world.ErrNoCharacterRow
	}
	if err != nil {
		return 0, fmt.Errorf("store: claim character %q: %w", pid, err)
	}
	return nonNegU64(epoch), nil
}

// SaveCharacter writes snap under two independent predicates (docs/PERSISTENCE.md §7).
//
// `owner_epoch <= $6` is the OWNERSHIP fence (#432). `state_version = $5` is CONTENTION control.
// They are NOT redundant, and the comment that used to sit here — calling the state_version CAS
// "the fence that protects a genuinely-rehydrated player on a new shard from a zombie original" —
// was the load-bearing false belief behind a live duplication primitive. It protected against
// nothing that a caller could not undo, because the documented answer to a version loss is to
// re-read, rebase and write again, and the world package did exactly that on BOTH save paths. A
// zombie owner therefore force-wrote its stale snapshot over the live owner's on every logout.
//
// An epoch loss is unreachable by rebasing: rebasing moves state_version, and the epoch predicate is
// a separate conjunct that a stale writer has no way to raise (only ClaimCharacter mints, and it
// mints for whoever claims NEXT, never retroactively for a writer already in flight).
//
// ONE statement, via a CTE, so the verdict and the diagnostic columns come from the same locked
// snapshot of the row. `pre` takes FOR UPDATE and supplies the observed (pre-UPDATE) columns; because
// it is both referenced twice and carries FOR UPDATE it can never be inlined, so it is materialized
// once and evaluates first. Under READ COMMITTED that lock is also what serializes a concurrent claim
// or save against this one: a later writer blocks on it, and FOR UPDATE follows the update chain and
// re-checks its own qual (including `deleted_at IS NULL`) against the newest version. The UPDATE's
// own predicates are re-checked by EvalPlanQual against whatever tuple wins. The verdict decodes
// unambiguously:
//
//	no rows            -> SaveNoRow      (deleted, or never created)
//	new_version present-> SaveApplied
//	pre.owner_epoch > k-> SaveNotOwner   (DEFINITIVE — never retry)
//	otherwise          -> SaveStaleVersion (retryable; rebase onto pre.state_version)
//
// `deleted_at IS NULL` is now part of the predicate; the pre-#432 UPDATE omitted it even though
// LoadCharacter had it, so a save could resurrect state onto a soft-deleted row.
func (p *Pool) SaveCharacter(ctx context.Context, snap world.CharSnapshot) (world.SaveResult, error) {
	if snap.PID == "" {
		return world.SaveResult{}, fmt.Errorf("store: save character %q: missing persist id", snap.Name)
	}
	id, err := uuid.Parse(string(snap.PID))
	if err != nil {
		return world.SaveResult{}, fmt.Errorf("store: save character %q: bad persist id %q: %w", snap.Name, snap.PID, err)
	}
	stateJSON, err := json.Marshal(snap.State)
	if err != nil {
		return world.SaveResult{}, fmt.Errorf("store: marshal character %q state: %w", snap.Name, err)
	}
	var (
		newVersion *int64
		curVersion int64
		curEpoch   int64
	)
	err = p.pool.QueryRow(ctx,
		// chargen = NULL clears the Phase-14.8 first-spawn marker in the SAME write that persists the built
		// state — so application + clear are atomic from the DB's view: a crash before this save re-applies
		// from the still-empty state (the additive racial mods never double-apply).
		// zone_ref = COALESCE($2::text, zone_ref): an EMPTY ZoneRef means "leave the stored location alone", never
		// "clear it" (#411). The world's only producer of ZoneRef (dumpCharacter) returns "" for a player who
		// is inside a runtime-minted zone INSTANCE, whose ephemeral id must never be persisted — but room_ref
		// still carries the template's AUTHORED ref, so a clearing write would leave the row internally
		// inconsistent: a real room with no zone. The reconnect then falls back to the home zone, cannot
		// resolve that room there, and start-rooms the player — durable location loss, in Postgres, for every
		// instance occupant on every save tick and every SIGTERM. Preserving instead keeps the entrance anchor
		// the row already holds. No caller ever legitimately intends to CLEAR a character's zone.
		`WITH pre AS (
		     SELECT id, state_version, owner_epoch
		       FROM characters
		      WHERE id = $4 AND deleted_at IS NULL
		      FOR UPDATE
		 ),
		 upd AS (
		     UPDATE characters c
		        SET state = $1,
		            zone_ref = COALESCE($2::text, zone_ref),
		            room_ref = $3,
		            state_version = c.state_version + 1,
		            -- GREATEST is provably equal to $6 given the owner_epoch <= $6 conjunct below; it is
		            -- written this way so the column's monotonicity is stated at the write site and does
		            -- not silently depend on a predicate three lines further down.
		            owner_epoch = GREATEST(c.owner_epoch, $6),
		            chargen = NULL,
		            last_saved_at = now()
		       FROM pre
		      WHERE c.id = pre.id
		        AND c.deleted_at IS NULL
		        AND c.state_version = $5
		        AND c.owner_epoch <= $6
		    RETURNING c.state_version AS new_version
		 )
		 SELECT (SELECT new_version FROM upd), pre.state_version, pre.owner_epoch
		   FROM pre`,
		stateJSON, nullStr(snap.ZoneRef), nullStr(snap.RoomRef), id,
		stateVersionParam(snap.StateVersion), stateVersionParam(snap.OwnerEpoch)).
		Scan(&newVersion, &curVersion, &curEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return world.SaveResult{Outcome: world.SaveNoRow}, nil
	}
	if err != nil {
		return world.SaveResult{}, fmt.Errorf("store: save character %q: %w", snap.Name, err)
	}
	res := world.SaveResult{CurVersion: nonNegU64(curVersion), CurOwnerEpoch: nonNegU64(curEpoch)}
	switch {
	case newVersion != nil:
		res.Outcome = world.SaveApplied
		res.NewVersion = nonNegU64(*newVersion)
	case res.CurOwnerEpoch > snap.OwnerEpoch:
		// Ownership, not contention. Report it as such even if the version ALSO mismatched: the caller
		// must stop, and telling it "stale version" would send it into the rebase loop this fence exists
		// to forbid.
		res.Outcome = world.SaveNotOwner
	default:
		res.Outcome = world.SaveStaleVersion
	}
	return res, nil
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
