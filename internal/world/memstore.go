package world

import (
	"context"
	"strconv"
	"sync"
)

// memstore.go is the in-memory CharacterStore + Checkpointer used by tests, so the durability-
// ladder logic (load-on-login, save-on-logout, the state_version CAS, the checkpoint-vs-Postgres
// freshness check, the crash-rehydrate-by-name primitive) is unit-testable WITHOUT a live
// Postgres or Redis. It deliberately mirrors the pgx store's semantics exactly — the same CAS
// rule, the same "load by name", the same minted-UUID create — so a test that passes against this
// is asserting the same contract the gated Postgres tests assert against the real store.
//
// It is concurrency-safe (a mutex): the async saver writes from its drainer goroutine while the
// login read reads from a stream goroutine, exactly as the real stores are hit concurrently.

// MemStore is an in-memory CharacterStore AND Checkpointer. A single struct backs both tiers so a
// test can model "Postgres" and "Redis" together (each tier is a separate map, so a test can let
// them diverge to exercise the freshness check). All methods are safe for concurrent use.
type MemStore struct {
	mu sync.Mutex
	// rows is the durable "Postgres" tier, keyed by lower-cased name (CITEXT is case-insensitive).
	rows map[string]CharSnapshot
	// ckpt is the "Redis" checkpoint tier, keyed the same way. Separate from rows so a test can
	// write a fresher checkpoint than the row and assert the load picks it.
	ckpt map[string]CharSnapshot
	// nextID mints monotonically increasing fake UUIDs ("mem-uuid-N") for CreateCharacter, enough
	// for tests to assert PersistID became real without a uuid dependency.
	nextID int
}

// NewMemStore returns an empty in-memory store/checkpointer.
func NewMemStore() *MemStore {
	return &MemStore{rows: map[string]CharSnapshot{}, ckpt: map[string]CharSnapshot{}}
}

// key normalizes a character name the way CITEXT does (case-insensitive), so "Alice" and "alice"
// resolve to one record — matching the real UNIQUE(name CITEXT) constraint.
func memKey(name string) string {
	lower := make([]byte, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	return string(lower)
}

// LoadCharacter returns the durable row for name, or found=false when none exists.
func (m *MemStore) LoadCharacter(_ context.Context, name string) (CharSnapshot, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap, ok := m.rows[memKey(name)]
	return snap, ok, nil
}

// CreateCharacter inserts a fresh row, minting a fake UUID and starting at state_version 0. A
// duplicate name is left as-is and its existing PID returned (the real store would error on the
// UNIQUE constraint; tests only create when a load found nothing, so a collision is a benign race).
func (m *MemStore) CreateCharacter(_ context.Context, name, zoneRef, roomRef string) (PersistID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(name)
	if existing, ok := m.rows[k]; ok {
		return existing.PID, nil
	}
	m.nextID++
	pid := PersistID("mem-uuid-" + strconv.Itoa(m.nextID))
	m.rows[k] = CharSnapshot{
		PID:          pid,
		Name:         name,
		ZoneRef:      zoneRef,
		RoomRef:      roomRef,
		StateVersion: 0,
		State:        StateJSON{},
	}
	return pid, nil
}

// SaveCharacter writes snap with the same optimistic-concurrency CAS as the pgx store: it applies
// only when the stored state_version equals snap.StateVersion, then bumps it and returns the new
// value. A version mismatch (a stale writer) returns ok=false with no error — the caller reconciles.
func (m *MemStore) SaveCharacter(_ context.Context, snap CharSnapshot) (uint64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(snap.Name)
	cur, ok := m.rows[k]
	if !ok {
		return 0, false, nil // no row to update (treated as a CAS loss)
	}
	if cur.StateVersion != snap.StateVersion {
		return 0, false, nil // stale writer lost the CAS
	}
	snap.PID = cur.PID // identity is immutable; never let a save rewrite it
	snap.StateVersion = cur.StateVersion + 1
	m.rows[k] = snap
	return snap.StateVersion, true, nil
}

// Checkpoint writes snap as the latest "Redis" checkpoint for snap.Name, overwriting the prior one.
func (m *MemStore) Checkpoint(_ context.Context, snap CharSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ckpt[memKey(snap.Name)] = snap
	return nil
}

// LoadCheckpoint returns the last checkpoint for name, or found=false if none.
func (m *MemStore) LoadCheckpoint(_ context.Context, name string) (CharSnapshot, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap, ok := m.ckpt[memKey(name)]
	return snap, ok, nil
}

// rowVersion is a test helper: the stored durable state_version for name (0, false if absent).
func (m *MemStore) rowVersion(name string) (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap, ok := m.rows[memKey(name)]
	if !ok {
		return 0, false
	}
	return snap.StateVersion, true
}

// Compile-time assertions that *MemStore satisfies both tiers.
var (
	_ CharacterStore = (*MemStore)(nil)
	_ Checkpointer   = (*MemStore)(nil)
)
