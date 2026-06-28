package content

import (
	"context"
	"fmt"
	"sync"
)

// definition.go is the SINGLE-REF read path for hot reload (docs/PHASE4-PLAN.md §5). Boot loads a
// whole pack (LoadPacks/Load); a hot reload instead re-reads exactly ONE changed definition by
// (kind, ref) so a shard rebuilds just that prototype. The result is the same neutral DTO the
// boot loader produces, so the world-side DTO->component mapper (content_map.go) is reused
// unchanged — a reloaded prototype is byte-identical to a freshly-booted one.

// DefKind names the kind of a definition (== the table). The hot-reload payload carries it so the
// re-read knows which table/DTO to fetch. These string values are the wire vocabulary shared with
// contentbus.Invalidation.Kind.
const (
	KindRoom = "room"
	KindItem = "item"
	KindMob  = "mob"
	KindZone = "zone"
)

// Definition is the result of a single-ref re-read: exactly one of Room/Proto is set (per Kind),
// or Found=false when the ref no longer exists (the row was deleted — the caller removes the
// prototype rather than serving stale data). It is intentionally a thin union so the world mapper
// switches on Kind and reuses roomComponents/protoComponents.
type Definition struct {
	Kind  string
	Ref   string
	Found bool

	Room  RoomDTO  // set when Kind == KindRoom && Found
	Proto ProtoDTO // set when Kind == KindItem || KindMob, && Found
}

// DefinitionSource is a content source that can re-read ONE definition by (kind, ref, pack). The
// pgx store implements it with a targeted single-row query; the EmbeddedSource implements it by
// scanning the (small) parsed pack. The world's hot-reload applier holds this interface, so the
// reload path is testable with the embedded source and runs against Postgres in production.
type DefinitionSource interface {
	// LoadDefinition re-reads the definition named by (kind, ref) within pack. Found=false (no
	// error) means the ref was deleted/never existed. An error is an infrastructure failure (the
	// caller keeps the last-known prototype rather than crashing).
	LoadDefinition(ctx context.Context, kind, ref, pack string) (Definition, error)
}

// LoadDefinition implements DefinitionSource for the embedded YAML source by parsing the named
// pack and scanning it for (kind, ref). The pack is small and reloads are rare, so a full parse +
// linear scan per reload is fine (and keeps the embedded source dependency-free). Used by tests
// and a bare run; production uses the pgx store's indexed single-row query.
func (s EmbeddedSource) LoadDefinition(ctx context.Context, kind, ref, pack string) (Definition, error) {
	packs, err := s.LoadPacks(ctx, []string{pack})
	if err != nil {
		return Definition{}, err
	}
	for _, p := range packs {
		for zi := range p.Zones {
			z := &p.Zones[zi]
			if def, ok := scanZone(kind, ref, z); ok {
				return def, nil
			}
		}
	}
	return Definition{Kind: kind, Ref: ref, Found: false}, nil
}

// scanZone looks for (kind, ref) inside one zone's child slices, returning a populated Definition
// when found. Shared by the embedded source; the store has its own indexed query.
func scanZone(kind, ref string, z *ZoneDTO) (Definition, bool) {
	switch kind {
	case KindRoom:
		for i := range z.Rooms {
			if z.Rooms[i].Ref == ref {
				return Definition{Kind: kind, Ref: ref, Found: true, Room: z.Rooms[i]}, true
			}
		}
	case KindItem:
		for i := range z.Items {
			if z.Items[i].Ref == ref {
				return Definition{Kind: kind, Ref: ref, Found: true, Proto: z.Items[i]}, true
			}
		}
	case KindMob:
		for i := range z.Mobs {
			if z.Mobs[i].Ref == ref {
				return Definition{Kind: kind, Ref: ref, Found: true, Proto: z.Mobs[i]}, true
			}
		}
	}
	return Definition{}, false
}

// MemSource is a mutable in-memory DefinitionSource for tests: it holds packs by name and lets a
// test EDIT a definition between reloads (change a room's long desc / an item's keywords) without
// a YAML file or a database — the in-memory equivalent the slice 4.3 unit test drives. It also
// implements Source (LoadPacks) so a shard can BOOT from it and then HOT-RELOAD from the same
// edited source, modelling the boot-then-edit-then-invalidate flow end to end.
//
// It is concurrency-safe (a mutex), because a real DefinitionSource (the pgx pool) is hit
// concurrently: the hot-reload applier RE-READS off the bus's subscription goroutine while a test
// (or a writer) EDITS — the same concurrent read/write shape the production store handles, so the
// concurrency test exercises the cache swap, not a test-double data race.
type MemSource struct {
	mu    sync.Mutex
	packs map[string]*Pack
}

// NewMemSource builds an empty mutable source.
func NewMemSource() *MemSource { return &MemSource{packs: map[string]*Pack{}} }

// SetPack installs (or replaces) a whole pack.
func (m *MemSource) SetPack(p Pack) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.packs[p.Pack] = &p
}

// LoadPacks implements Source: returns the named packs (a deep enough copy is unnecessary for
// tests — callers do not mutate the returned DTOs in place).
func (m *MemSource) LoadPacks(_ context.Context, enabled []string) ([]Pack, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Pack
	for _, name := range enabled {
		if p := m.packs[name]; p != nil {
			out = append(out, *p)
		}
	}
	return out, nil
}

// LoadDefinition implements DefinitionSource by scanning the live (possibly edited) pack, so a
// test mutates the pack then triggers a reload and the re-read observes the edit.
func (m *MemSource) LoadDefinition(_ context.Context, kind, ref, pack string) (Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.packs[pack]
	if p == nil {
		return Definition{Kind: kind, Ref: ref, Found: false}, nil
	}
	for zi := range p.Zones {
		if def, ok := scanZone(kind, ref, &p.Zones[zi]); ok {
			return def, nil
		}
	}
	return Definition{Kind: kind, Ref: ref, Found: false}, nil
}

// EditRoomLong is a test convenience: change one room's long description in place. Returns an
// error if the room is not present so a typo in a test ref fails loudly.
func (m *MemSource) EditRoomLong(pack, ref, long string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.packs[pack]
	if p == nil {
		return fmt.Errorf("content: MemSource has no pack %q", pack)
	}
	for zi := range p.Zones {
		for ri := range p.Zones[zi].Rooms {
			if p.Zones[zi].Rooms[ri].Ref == ref {
				p.Zones[zi].Rooms[ri].Long = long
				return nil
			}
		}
	}
	return fmt.Errorf("content: MemSource pack %q has no room %q", pack, ref)
}

// EditItemKeywords is a test convenience: replace one item prototype's keywords in place.
func (m *MemSource) EditItemKeywords(pack, ref string, keywords []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.packs[pack]
	if p == nil {
		return fmt.Errorf("content: MemSource has no pack %q", pack)
	}
	for zi := range p.Zones {
		for ii := range p.Zones[zi].Items {
			if p.Zones[zi].Items[ii].Ref == ref {
				// Replace the keyword slice with a fresh one rather than mutating in place, so a
				// reader that already grabbed the prior slice header is unaffected (the reload
				// copies it into a new prototype anyway).
				p.Zones[zi].Items[ii].Keywords = append([]string(nil), keywords...)
				return nil
			}
		}
	}
	return fmt.Errorf("content: MemSource pack %q has no item %q", pack, ref)
}

// EditMobLua is a test convenience: replace one MOB prototype's `lua` trigger block in place
// (Phase 7.7 hot-reload coverage). Returns an error if the mob is not present.
func (m *MemSource) EditMobLua(pack, ref, lua string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.packs[pack]
	if p == nil {
		return fmt.Errorf("content: MemSource has no pack %q", pack)
	}
	for zi := range p.Zones {
		for mi := range p.Zones[zi].Mobs {
			if p.Zones[zi].Mobs[mi].Ref == ref {
				p.Zones[zi].Mobs[mi].Lua = lua
				return nil
			}
		}
	}
	return fmt.Errorf("content: MemSource pack %q has no mob %q", pack, ref)
}

// Compile-time assertions.
var (
	_ Source           = (*MemSource)(nil)
	_ DefinitionSource = (*MemSource)(nil)
	_ DefinitionSource = EmbeddedSource{}
)
