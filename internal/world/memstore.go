package world

import (
	"context"
	"strconv"
	"sync"
	"time"
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

	// mail is the in-memory durable mail inbox (Phase 8.7), a flat slice of every message. It mirrors
	// the pgx `mail` table semantics EXACTLY — newest-first per recipient, player-scoped read/delete by
	// 1-based position — so the hermetic mail journey asserts the same contract the gated Postgres test
	// asserts against the real store. Guarded by the same mu (a test mails from one goroutine and reads
	// from another, like the real store hit concurrently). nextMailID mints fake message ids.
	mail       []MailEntry
	nextMailID int
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

// --- MailStore (Phase 8.7) ------------------------------------------------------------------

// SendMail appends one message, minting a fake id. `from` is the engine-set sender the caller captured
// (the mem store never derives it). Mirrors the pgx INSERT.
func (m *MemStore) SendMail(_ context.Context, to, from, subject, body string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Mirror the pgx inbox cap so the hermetic path enforces the same ceiling (security hardening).
	if len(m.inboxLocked(to)) >= MailInboxCap {
		// Retention sweep parity with the pgx store (docs/REMAINING.md §1): evict the oldest READ message
		// so a full-of-read inbox doesn't wedge on spam; an inbox full of UNREAD mail still refuses.
		if !m.evictOldestReadLocked(to) {
			return "", ErrMailboxFull
		}
	}
	m.nextMailID++
	id := "mem-mail-" + strconv.Itoa(m.nextMailID)
	m.mail = append(m.mail, MailEntry{
		ID:      id,
		To:      to,
		From:    from,
		Subject: subject,
		Body:    body,
		SentAt:  time.Now(),
		Read:    false,
	})
	return id, nil
}

// evictOldestReadLocked removes the single oldest READ message in `player`'s inbox from m.mail, returning
// whether one was removed. It mirrors the pgx evictOldestRead: only READ mail is reclaimable, and oldest
// (earliest SentAt, insertion order tie-break) is evicted first. Caller holds mu.
func (m *MemStore) evictOldestReadLocked(player string) bool {
	key := memKey(player)
	oldest := -1
	for i, e := range m.mail {
		if memKey(e.To) != key || !e.Read {
			continue
		}
		if oldest == -1 || m.mail[i].SentAt.Before(m.mail[oldest].SentAt) {
			oldest = i
		}
	}
	if oldest == -1 {
		return false
	}
	m.mail = append(m.mail[:oldest], m.mail[oldest+1:]...)
	return true
}

// inboxLocked returns `player`'s messages newest-first (the same order the pgx ORDER BY produces). Caller
// holds mu. This is the player-scoped projection EVERY mem read/delete goes through, so the access-control
// contract (a player only ever sees their own mail) holds for the mem path exactly as the SQL WHERE does.
func (m *MemStore) inboxLocked(player string) []MailEntry {
	key := memKey(player)
	var out []MailEntry
	for _, e := range m.mail {
		if memKey(e.To) == key {
			out = append(out, e)
		}
	}
	sortMailNewestFirst(out)
	return out
}

// ListMail returns the player's inbox newest-first (scoped to to_player). A copy, so the caller cannot
// mutate the store's rows.
func (m *MemStore) ListMail(_ context.Context, player string) ([]MailEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inboxLocked(player), nil
}

// ReadMail fetches + marks-read the player's nth (1-based) message newest-first. found=false when out of
// range. SCOPED: pos indexes the player's OWN inbox, so no id another player owns is reachable.
func (m *MemStore) ReadMail(_ context.Context, player string, pos int) (MailEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inbox := m.inboxLocked(player)
	if pos < 1 || pos > len(inbox) {
		return MailEntry{}, false, nil
	}
	target := inbox[pos-1]
	// Mark read in the backing slice (find by id — the projection is a copy).
	for i := range m.mail {
		if m.mail[i].ID == target.ID {
			m.mail[i].Read = true
			target.Read = true
			break
		}
	}
	return target, true, nil
}

// DeleteMail removes the player's nth (1-based) message newest-first. deleted=false when out of range.
// SCOPED to the player's own inbox by position.
func (m *MemStore) DeleteMail(_ context.Context, player string, pos int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inbox := m.inboxLocked(player)
	if pos < 1 || pos > len(inbox) {
		return false, nil
	}
	targetID := inbox[pos-1].ID
	for i := range m.mail {
		if m.mail[i].ID == targetID {
			m.mail = append(m.mail[:i], m.mail[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// Compile-time assertions that *MemStore satisfies all three tiers.
var (
	_ CharacterStore = (*MemStore)(nil)
	_ Checkpointer   = (*MemStore)(nil)
	_ MailStore      = (*MemStore)(nil)
)
