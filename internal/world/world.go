// Package world is the simulation shard: the zone(s) this process owns, the
// actor event loop, the gRPC Play server, and the source side of the cross-shard
// handoff. Content loading and the full mudlib arrive in later phases
// (docs/ROADMAP.md).
//
// # The actor-per-zone model (docs/ARCHITECTURE.md §3)
//
// A Zone is owned by exactly one goroutine (Zone.Run). Rooms and players are plain
// data, not goroutines; only the zone goroutine reads or mutates them, so game logic
// needs no locks. Every interaction from outside that goroutine — a player's gRPC
// stream handler, and the async handoff coordinator — happens by posting a message to
// the zone inbox (Zone.post), never by touching zone state directly.
//
// # Following a command end to end
//
//	gRPC reader (server.go) -> zone inbox -> zone loop (zone.go/commands.go)
//	                                      -> player.out -> gRPC writer (server.go) -> wire
//
// Run with DEBUG=1 (see internal/obs) to watch every step narrated via slog.Debug.
package world

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// handoffRPCTimeout bounds the whole source-side handoff conversation (ShardForZone/
// EndpointForShard/Prepare/SetPlayerShard). Without a deadline a Prepare to a restarting
// or draining destination hangs forever and the coordinator posts NEITHER redirectMsg NOR
// handoffFailMsg — leaving the player permanently frozen and locked out of reconnect. On
// the deadline the fail(...) path posts handoffFailMsg and the source thaws + restores the
// player to the room they tried to leave. It is a package var (not a const) so a test can
// shrink it to exercise the timeout->thaw path quickly.
var handoffRPCTimeout = 5 * time.Second

// HandoffDialer resolves a Handoff client for a peer shard's address. Injected so
// tests can dial in-process shards over bufconn.
type HandoffDialer func(addr string) (handoffv1.HandoffClient, error)

// Shard is one world process. It may host more than one zone: a player can walk
// between two zones THIS shard owns entirely in-process, with no cross-shard handoff
// (see Zone.transferIn / Zone.move). zones maps zone id -> zone; home is the zone a
// fresh login spawns in. addr is the shard's public address (what the gate and peer
// shards dial) and dir routes moves into zones OTHER shards own.
//
// # Cross-goroutine routing primitives (single-writer still holds)
//
// Each zone's state is mutated only by its own goroutine. Two deliberately small,
// concurrency-safe structures connect them:
//
//   - the per-connection currentZone (an atomic.Pointer[Zone] owned by the Play
//     stream, see server.go): which zone a player's input is posted to right now. A
//     zone Stores itself into it when it takes ownership of the player (attach /
//     transferIn); the reader loop Loads it for every line.
//   - tokenIndex (token -> zone): lets the Play attach and Handoff.Prepare route a
//     handoff bind to whichever hosted zone holds the matching pending player. Guarded
//     by mu; populated by Prepare, read on bind.
//
// No other shared mutable zone state exists. Intra-shard transfer of the player
// struct itself is still done by message-passing (transferInMsg), so only one zone
// goroutine ever owns a given player at a time.
type Shard struct {
	zones map[string]*Zone // zone id -> zone; all hosted on this process
	home  string           // zone a fresh login spawns in
	addr  string           // this shard's public address ("" in single-shard tests)
	dir   Locator          // directory for cross-shard routing; nil seals cross-shard exits
	peers HandoffDialer    // dials peer shards' Handoff service

	mu         sync.Mutex       // guards tokenIndex
	tokenIndex map[string]*Zone // handoff token -> hosting zone (populated by Prepare)

	// saver is the shard's async character writer (saver.go): one per shard, shared by every
	// hosted zone, drained by a single background goroutine started in Run. It does all the
	// off-zone-goroutine character I/O (Redis checkpoint + Postgres CAS). Always non-nil but
	// DISABLED (a no-op) unless a store/checkpointer was configured — so a storeless shard
	// keeps the pre-4.2 ephemeral behavior with zero extra goroutines or work.
	saver *saver

	// protos is the per-shard prototype cache shared read-only by every hosted zone. It is held
	// here too (not only on each zone) so the hot-reload applier can swap entries into the one
	// shared cache (reload.go). nil only on a zero-value shard; newBareShard leaves it for the
	// constructor to set after building the cache.
	protos *protoCache

	// defs is the per-shard bundle of pack-global definition registries (attributes/resources/
	// damage-types — defs.go), shared read-only by every hosted zone exactly like protos. Built
	// once at construction, then only read; the 4.3-style hot-reload swap (a later slice) would
	// reload an entry into this one shared bundle. Set by the constructors after registration.
	defs *defRegistries

	// reloader, if non-nil, is the content hot-reload applier (reload.go): it subscribes to the
	// content bus and atomically swaps a rebuilt *Prototype into protos on an invalidation. nil
	// when no bus/source is configured — hot reload disabled, a busless shard is byte-identical
	// to a pre-4.3 shard. Set by WithHotReload, torn down at Run end.
	reloader *reloader
}

// NewDemoShard builds a single-shard midgaard world with no directory wiring — its
// cross-shard exits are sealed. Used by the single-shard tests and a bare run.
func NewDemoShard() *Shard {
	return newShard([]string{"midgaard"}, "midgaard", "", nil, nil)
}

// NewShard builds the named demo zone and wires it for cross-shard handoff: addr is
// this shard's public address, dir routes moves into zones other shards own, and
// peers dials those shards' Handoff service. Single-zone convenience wrapper around
// NewMultiShard.
func NewShard(zoneID, addr string, dir Locator, peers HandoffDialer) *Shard {
	return NewMultiShard([]string{zoneID}, zoneID, addr, dir, peers)
}

// NewMultiShard builds a shard hosting every zone in zoneIDs (home is the spawn zone
// for fresh logins) and wires each for cross-shard handoff. A move into a zone this
// shard hosts is handled in-process (Zone.move); a move into a zone another shard owns
// goes through beginHandoff as before.
func NewMultiShard(zoneIDs []string, home, addr string, dir Locator, peers HandoffDialer) *Shard {
	return newShard(zoneIDs, home, addr, dir, peers)
}

// newShard builds a shard whose zones are the EMBEDDED demo pack (test/bare-run path). Each
// zone is constructed via newDemoZone, which loads packs/demo.yaml into the shared cache and
// builds the named zone — so the demo world has no Postgres dependency and the Phase 1-3
// tests construct it exactly as before. Production uses NewShardFromContent instead, so the
// demo pack is never compiled into the engine's production boot path.
func newShard(zoneIDs []string, home, addr string, dir Locator, peers HandoffDialer) *Shard {
	s := newBareShard(home, addr, dir, peers)
	// Build the per-shard prototype cache ONCE here, before any zone goroutine runs
	// (prototype.go). It is shared read-only across every hosted zone, so the flyweight
	// pays off across the whole process and the cross-goroutine sharing needs no lock —
	// it is published immutable. After this loop nothing mutates the cache or a *Prototype.
	protos := newProtoCache()
	s.protos = protos
	// Register the demo pack's pack-global defs ONCE into the shared shard bundle, before any zone
	// runs (defineGlobals), so every hosted zone reads the SAME atomic-swap registries — the same
	// shared-read-only-after-publish discipline as protos. adopt points each zone at this bundle.
	if lc, err := content.LoadDemoPack(); err == nil {
		defineGlobals(s.defs, lc)
	}
	for _, id := range zoneIDs {
		z := newDemoZone(id, protos)
		s.adopt(id, z)
	}
	if s.zones[home] == nil && len(zoneIDs) > 0 {
		s.home = zoneIDs[0]
	}
	return s
}

// NewShardFromContent is the PRODUCTION constructor (cmd/telos-world buildShard). It fills
// the shared per-shard prototype cache from already-loaded content (Postgres or the embedded
// pack, chosen by the binary), then builds every hosted zone from that content via buildZone.
// With empty content (no DB / no enabled packs) every zone boots EMPTY — the bare-engine
// invariant — and a login is rejected cleanly rather than panicking (Zone.join guards).
//
// Unlike newShard it does NOT call newDemoZone, so no demo content is linked into the
// production path; the demo lives only in the YAML/DB.
func NewShardFromContent(lc *content.LoadedContent, zoneIDs []string, home, addr string, dir Locator, peers HandoffDialer) *Shard {
	s := newBareShard(home, addr, dir, peers)
	protos := newProtoCache()
	s.protos = protos
	defineContent(protos, lc) // fill the cache once from all loaded zones, before any zone runs
	defineGlobals(s.defs, lc) // register pack-global attribute/resource/damage-type defs (5.1)
	for _, id := range zoneIDs {
		z := newZone(id)
		z.protos = protos
		z.buildZone(lc) // spawn room singletons + run resets (empty if the zone wasn't loaded)
		s.adopt(id, z)
	}
	if s.zones[home] == nil && len(zoneIDs) > 0 {
		s.home = zoneIDs[0]
	}
	return s
}

// newBareShard allocates the Shard struct with its routing maps; callers then build and
// adopt the hosted zones. The saver starts DISABLED (nil store + checkpointer); WithPersistence
// swaps in a configured one before any zone is adopted.
func newBareShard(home, addr string, dir Locator, peers HandoffDialer) *Shard {
	return &Shard{
		zones:      map[string]*Zone{},
		home:       home,
		addr:       addr,
		dir:        dir,
		peers:      peers,
		tokenIndex: map[string]*Zone{},
		saver:      newSaver(nil, nil), // disabled until WithPersistence configures it
		// Empty global-definition bundle (defs.go); the constructor registers content into it
		// before any zone runs and shares the SAME bundle pointer with every hosted zone.
		defs: newDefRegistries(),
	}
}

// WithPersistence configures the shard's durable character ladder: the Postgres-backed store and
// the optional Redis checkpointer. Either may be nil (a nil store = no durable record, a nil
// checkpointer = no Redis tier; both nil = ephemeral, today's behavior). It MUST be called before
// the shard's zones run (it is wired into the saver every zone shares via adopt). Returns the
// shard for chaining. The production constructor (cmd/telos-world buildShard) calls this; tests
// inject an in-memory store the same way.
func (s *Shard) WithPersistence(store CharacterStore, ckpt Checkpointer) *Shard {
	s.saver = newSaver(store, ckpt)
	// Re-point every already-adopted zone at the new saver (adopt copies the pointer). Callers
	// normally call this before adopting any zone, but re-pointing keeps the wiring order-free.
	for _, z := range s.zones {
		z.saver = s.saver
	}
	return s
}

// WithHotReload enables content hot reload (docs/PHASE4-PLAN.md §5): it subscribes the shard to
// the content bus so an invalidation re-reads and swaps the one affected prototype into the shared
// cache (reload.go). src is the single-ref re-read source (the Postgres store in prod, the embedded
// or in-memory source in tests); bus is the invalidation transport; enabledPacks scopes which
// edits this shard acts on. It is OPTIONAL: a nil bus or nil src leaves hot reload DISABLED and the
// shard byte-identical to a pre-4.3 shard. MUST be called before Run (the subscription is torn down
// at Run end). Returns the shard for chaining. The production constructor wires it; tests inject an
// in-memory bus + source the same way.
func (s *Shard) WithHotReload(src content.DefinitionSource, bus contentbus.Bus, enabledPacks []string) *Shard {
	s.reloader = newReloader(src, s.protos, bus, enabledPacks, s)
	return s
}

// adopt registers a built zone on the shard and wires its cross-shard handoff hook + the shared
// async saver.
func (s *Shard) adopt(id string, z *Zone) {
	z.shard = s
	z.handoff = s.beginHandoff
	z.saver = s.saver
	// Share the ONE per-shard global-definition bundle with every hosted zone (defs.go), replacing
	// the private bundle newZone/newDemoZone gave it. Every zone goroutine then reads the same
	// atomic-swap registries lock-free, exactly as it shares the one protoCache.
	if s.defs != nil {
		z.defs = s.defs
	}
	s.zones[id] = z
}

// indexToken records that zone z holds a pending player bound by token, so a Play
// attach or Handoff.Prepare on this shard can route the bind to the right zone.
func (s *Shard) indexToken(token string, z *Zone) {
	s.mu.Lock()
	s.tokenIndex[token] = z
	s.mu.Unlock()
}

// zoneForToken returns the zone holding the pending player for token, if any.
func (s *Shard) zoneForToken(token string) *Zone {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenIndex[token]
}

// dropToken removes a token index entry once its handoff resolves (bound/aborted/
// expired), so the map does not grow without bound.
func (s *Shard) dropToken(token string) {
	s.mu.Lock()
	delete(s.tokenIndex, token)
	s.mu.Unlock()
}

// GRPCDialer dials peer shards over plaintext gRPC, caching one connection per
// address. Used by the world binary; tests inject their own bufconn dialer.
func GRPCDialer() HandoffDialer {
	var mu sync.Mutex
	conns := map[string]*grpc.ClientConn{}
	return func(addr string) (handoffv1.HandoffClient, error) {
		mu.Lock()
		defer mu.Unlock()
		if conns[addr] == nil {
			cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return nil, err
			}
			conns[addr] = cc
		}
		return handoffv1.NewHandoffClient(conns[addr]), nil
	}
}

// Zone returns the shard's home zone. Convenience for single-zone callers/tests.
func (s *Shard) Zone() *Zone { return s.zones[s.home] }

// ZoneByID returns the hosted zone with the given id, or nil.
func (s *Shard) ZoneByID(id string) *Zone { return s.zones[id] }

// Run starts every hosted zone's actor loop on its own goroutine and the shard's single async
// saver drainer, then blocks until ctx is cancelled. One goroutine per zone preserves
// single-writer; the saver drainer is the ONE place character I/O runs, never on a zone
// goroutine. A disabled saver's run returns immediately (no goroutine cost for a storeless boot).
func (s *Shard) Run(ctx context.Context) {
	go s.saver.run(ctx) // off-zone-goroutine character writer (no-op if disabled)
	// Hot reload runs on the bus's own subscription goroutine (no shard goroutine of its own);
	// just tear the subscription down when the shard stops so a restart doesn't double-subscribe.
	if s.reloader != nil {
		defer s.reloader.stop()
	}
	var wg sync.WaitGroup
	for _, z := range s.zones {
		wg.Add(1)
		go func(z *Zone) {
			defer wg.Done()
			z.Run(ctx)
		}(z)
	}
	wg.Wait()
}

// Drain enqueues an immediate durable flush of every live, persisted player on every hosted zone
// — the shard-drain flush point (docs/PERSISTENCE.md §6, rolling redeploy). It posts a leaveMsg-
// free flush request to each zone goroutine (so the dump stays single-writer) and returns at once;
// the saver writes the snapshots off-goroutine. Phase 4 BUILDS this hook; the placement controller
// that TRIGGERS a drain (graceful handoff of every player before shutdown) is Phase 10. A no-op on
// a disabled (ephemeral) saver.
func (s *Shard) Drain() {
	if s.saver == nil || !s.saver.enabled() {
		return
	}
	for _, z := range s.zones {
		z.post(drainFlushMsg{})
	}
}

// Register installs the gRPC Play and Handoff services on the given server. The Play
// service routes a fresh login to the shard's home zone and a handoff bind to whichever
// hosted zone holds the matching pending player; Handoff routes Prepare by target zone.
func (s *Shard) Register(gs *grpc.Server) {
	registerPlay(gs, s)
	registerHandoff(gs, s)
}

// beginHandoff is the source side of a cross-shard move (PROTOCOL.md §3, §5). It runs
// asynchronously (never on the zone goroutine, so it may do blocking directory I/O):
// it resolves the destination shard, claims ownership in the directory with a bumped
// epoch (compare-and-set, so a stale handoff can't win), and posts a redirectMsg back
// to the zone to send the player a Redirect. On any failure it posts handoffFailMsg
// so the zone thaws the (otherwise stuck) frozen player.
//
// Step 4 adds the Handoff.Prepare RPC to the destination here, between the directory
// claim and the redirect; step 5 wires the gate to act on the Redirect.
func (s *Shard) beginHandoff(src *Zone, snap *handoffv1.PlayerSnapshot, destZone, destRoom string, epoch uint64) {
	go func() {
		// Bound the whole conversation: a hung Prepare to a restarting/draining destination
		// must not strand the frozen player forever. On deadline the fail(...) path below
		// thaws + restores them. Runs off the zone goroutine, so blocking here is safe.
		ctx, cancel := context.WithTimeout(context.Background(), handoffRPCTimeout)
		defer cancel()
		character := snap.GetCharacterId()
		newEpoch := epoch + 1
		log := slog.With("component", "handoff", "player", character, "dest_zone", destZone)

		fail := func(reason string) { src.post(handoffFailMsg{id: character, reason: reason}) }

		// Resolve the destination in two hops: which SHARD owns the zone, then where that
		// shard is reachable. The zone exit named only a logical zone ("darkwood"); the
		// directory turns that into a shard id and the shard id into a dial endpoint, so
		// the binding survives the owning shard moving hosts.
		destShardID, err := s.dir.ShardForZone(ctx, destZone)
		if err != nil {
			log.Warn("destination zone not in directory", "err", err)
			fail("destination unreachable")
			return
		}
		addr, err := s.dir.EndpointForShard(ctx, destShardID)
		if err != nil {
			log.Warn("owning shard has no live endpoint", "dest_shard", destShardID, "err", err)
			fail("destination unreachable")
			return
		}
		log.Debug("resolved destination", "dest_shard", destShardID, "endpoint", addr)
		client, err := s.peers(addr)
		if err != nil {
			log.Warn("cannot reach destination shard", "dest_shard", destShardID, "addr", addr, "err", err)
			fail("destination unreachable")
			return
		}

		// Prepare the destination: it rehydrates the player as a pending entity.
		resp, err := client.Prepare(ctx, &handoffv1.PrepareRequest{
			SessionId:    character, // session-id stand-in (deterministic token, §5)
			Snapshot:     snap,
			TargetZoneId: destZone,
			TargetRoomId: destRoom,
			Epoch:        newEpoch,
			FromShardId:  s.addr,
		})
		if err != nil {
			log.Warn("prepare rejected by destination", "err", err)
			fail("destination rejected the handoff")
			return
		}

		// Prepare succeeded: claim ownership in the directory (epoch CAS), recording the
		// destination SHARD ID (not its address). On conflict, roll back the destination's
		// pending entity.
		if ok, err := s.dir.SetPlayerShard(ctx, character, destShardID, newEpoch); err != nil || !ok {
			log.Warn("directory claim failed after prepare", "ok", ok, "err", err)
			// The rollback Abort needs a FRESH context: ctx may already be at/past its
			// deadline (e.g. SetPlayerShard was what timed out), which would cancel the
			// Abort before it could discard the destination's pending entity.
			abortCtx, ac := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = client.Abort(abortCtx, &handoffv1.AbortRequest{HandoffToken: resp.GetHandoffToken(), Reason: "directory conflict"})
			ac()
			fail("ownership conflict")
			return
		}

		// Commit-marker FIRST: the directory CAS just committed, so the both-own truth has
		// flipped. Post handedOffMsg ahead of redirectMsg (and ahead of the log line below) so
		// the freeze-reaper's success discriminator is set at the CAS-commit point, not one
		// message later at Redirect-frame send. This closes the narrow both-own window where a
		// freezeExpire firing in the gap would thaw a player whose handoff already succeeded —
		// correctness no longer depends on freezeTTL >> handoffRPCTimeout.
		src.post(handedOffMsg{id: character})

		log.Debug("prepared + ownership claimed; redirecting", "dest_addr", resp.GetTargetShardAddr(), "epoch", newEpoch)
		src.post(redirectMsg{
			id:         character,
			targetAddr: resp.GetTargetShardAddr(),
			token:      resp.GetHandoffToken(),
			resumeSeq:  snap.GetAppliedSeq(),
			epoch:      newEpoch,
		})
	}()
}

// defineRoom authors one room PROTOTYPE (docs/MUDLIB.md §5) into the shard's cache: an
// immutable template carrying the room's display name (short), description (long), and a
// Room component template with an exits map. The exits map is part of the immutable
// template — it is populated HERE at authoring time and never mutated afterward (an
// instance that re-routes an exit COWs it via mutableRoom). Returns the prototype so the
// caller can wire its exits before any instance is spawned.
func defineRoom(c *protoCache, ref ProtoRef, name, desc string) *Prototype {
	comps := componentSet{}
	r := &Room{exits: map[string]ProtoRef{}}
	comps[reflect.TypeFor[*Room]()] = r
	return c.define(ref, nil, name, desc, comps)
}

// spawnRoom instantiates a room prototype into zone z and registers it in z.rooms (MUDLIB
// §4: a room is an Entity with a Room component and no location, its container the zone).
// Rooms are singletons — one instance per ref — so they share the prototype's immutable
// exits/name/desc by reference until something COWs them (nothing does in the demo).
func (z *Zone) spawnRoom(ref ProtoRef) *Entity {
	e := z.spawn(ref)
	z.rooms[ref] = e
	return e
}

// newDemoZone has moved to build.go: the hand-authored body is GONE (Phase 4.1). It is now a
// thin wrapper that loads the EMBEDDED demo content pack into the shared per-shard cache and
// builds the named zone via the content loader, producing byte-identical prototypes. The
// authoring helpers below (defineRoom/spawnRoom/defineChest) remain as general prototype-
// construction utilities the prototype/container tests use directly.

// defineChest authors the CONTAINER prototype — the COW-arming object (Finding 6). It starts
// CLOSED; open/close flip Container.closed, which is shared with this prototype, so the verbs
// must COW via mutableComponent (cmdOpen/cmdClose). capacity caps how many items it holds.
// Retained as a test authoring helper (container_test's concurrent-COW race builds a bare
// cache from it); the demo world itself is authored in the demo pack (packs/demo.yaml).
func defineChest(c *protoCache) *Prototype {
	comps := componentSet{}
	comps[reflect.TypeFor[*Physical]()] = &Physical{weight: 40, material: "oak"}
	comps[reflect.TypeFor[*Container]()] = &Container{capacity: 10, closed: true}
	return c.define("midgaard:obj:chest",
		[]string{"chest", "oak", "wooden"},
		"a wooden chest",
		"A heavy wooden chest sits here.",
		comps)
}
