package director

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// memScopeStore is an in-memory ScopeStore for hermetic director tests, mirroring the Postgres store's
// versioned-CAS semantics: a first write (expected 0) creates at version 1; a correct-version write
// bumps; a version mismatch (incl. an expected>0 on an absent key) loses the CAS (ok=false).
type memScopeStore struct {
	mu     sync.Mutex
	world  map[string]memEntry
	region map[string]map[string]memEntry
}
type memEntry struct {
	value   []byte
	version uint64
}

func newMemStore() *memScopeStore {
	return &memScopeStore{world: map[string]memEntry{}, region: map[string]map[string]memEntry{}}
}

// errNullScopeValue mirrors the real schema. world_state.value / region_state.value are JSONB NOT NULL,
// so Postgres rejects a nil value with a 23502 constraint violation. This double used to accept it and
// store empty bytes — a fake that was MORE permissive than the thing it stands in for, which is the one
// direction a test double must never diverge, because it hides the bug instead of surfacing it. A delete
// is written as the JSON `null` literal, not as a SQL NULL.
var errNullScopeValue = errors.New("memScopeStore: null value violates the value NOT NULL constraint (mirrors Postgres 23502)")

func casSave(tbl map[string]memEntry, key string, value []byte, expected uint64) (uint64, bool) {
	e, ok := tbl[key]
	if !ok {
		if expected != 0 {
			return 0, false
		}
		tbl[key] = memEntry{value: append([]byte(nil), value...), version: 1}
		return 1, true
	}
	if e.version != expected {
		return 0, false
	}
	nv := e.version + 1
	tbl[key] = memEntry{value: append([]byte(nil), value...), version: nv}
	return nv, true
}

func (m *memScopeStore) LoadWorldState(_ context.Context, key string) ([]byte, uint64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.world[key]
	if !ok {
		return nil, 0, false, nil
	}
	return e.value, e.version, true, nil
}

func (m *memScopeStore) SaveWorldState(_ context.Context, key string, value []byte, expected uint64) (uint64, bool, error) {
	if value == nil {
		return 0, false, errNullScopeValue
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	nv, ok := casSave(m.world, key, value, expected)
	return nv, ok, nil
}

func (m *memScopeStore) LoadRegionState(_ context.Context, regionID, key string) ([]byte, uint64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.region[regionID]
	if r == nil {
		return nil, 0, false, nil
	}
	e, ok := r[key]
	if !ok {
		return nil, 0, false, nil
	}
	return e.value, e.version, true, nil
}

func (m *memScopeStore) SaveRegionState(_ context.Context, regionID, key string, value []byte, expected uint64) (uint64, bool, error) {
	if value == nil {
		return 0, false, errNullScopeValue
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.region[regionID] == nil {
		m.region[regionID] = map[string]memEntry{}
	}
	nv, ok := casSave(m.region[regionID], key, value, expected)
	return nv, ok, nil
}

var _ ScopeStore = (*memScopeStore)(nil)

// runDirector starts a director and returns it + a cancel; cleanup stops the loop.
func runDirector(t *testing.T, regionID string, store ScopeStore) *Director {
	t.Helper()
	d := New(regionID, store, discardLog()).WithTick(5 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	return d
}

func TestDirectorSetGet(t *testing.T) {
	d := runDirector(t, "", newMemStore())
	ctx := context.Background()

	// Unset → not found.
	_, found, err := d.Get(ctx, "invasion")
	if err != nil || found {
		t.Fatalf("unset key: found=%v err=%v", found, err)
	}
	// Set, then read back.
	if err := d.Set(ctx, "invasion", json.RawMessage(`{"phase":1}`)); err != nil {
		t.Fatal(err)
	}
	v, found, err := d.Get(ctx, "invasion")
	if err != nil || !found {
		t.Fatalf("after set: found=%v err=%v", found, err)
	}
	if string(v) != `{"phase":1}` {
		t.Fatalf("value = %s, want the json", v)
	}
}

func TestDirectorPersistsAndAFreshDirectorReloads(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()

	d1 := runDirector(t, "", store)
	if err := d1.Set(ctx, "boss", json.RawMessage(`"slain"`)); err != nil {
		t.Fatal(err)
	}
	// A brand-new director over the SAME store (the failover / restart case) reads the persisted value
	// — proving Set wrote through to the durable scope state, not just the cache.
	d2 := runDirector(t, "", store)
	v, found, err := d2.Get(ctx, "boss")
	if err != nil || !found {
		t.Fatalf("fresh director: found=%v err=%v", found, err)
	}
	if string(v) != `"slain"` {
		t.Fatalf("reloaded value = %s, want \"slain\"", v)
	}
}

func TestDirectorRegionScopeIsolated(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	world := runDirector(t, "", store)
	region := runDirector(t, "duskwall", store)

	if err := world.Set(ctx, "mood", json.RawMessage(`"global"`)); err != nil {
		t.Fatal(err)
	}
	if err := region.Set(ctx, "mood", json.RawMessage(`"besieged"`)); err != nil {
		t.Fatal(err)
	}
	// Same key, different scopes → independent values.
	wv, _, _ := world.Get(ctx, "mood")
	rv, _, _ := region.Get(ctx, "mood")
	if string(wv) != `"global"` || string(rv) != `"besieged"` {
		t.Fatalf("scope isolation broken: world=%s region=%s", wv, rv)
	}
}

func TestDirectorSingleWriterUnderLoad(t *testing.T) {
	d := runDirector(t, "", newMemStore())
	ctx := context.Background()

	// Many concurrent Sets to the same key serialize through the actor — no race, and the final value
	// is one of the writes (the actor applied them one at a time).
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Set(ctx, "counter", json.RawMessage(`123`))
		}()
	}
	wg.Wait()
	v, found, err := d.Get(ctx, "counter")
	if err != nil || !found || string(v) != `123` {
		t.Fatalf("after concurrent sets: v=%s found=%v err=%v", v, found, err)
	}
}
