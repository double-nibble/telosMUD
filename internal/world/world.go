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
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

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

	mu         sync.Mutex        // guards tokenIndex
	tokenIndex map[string]*Zone  // handoff token -> hosting zone (populated by Prepare)
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

func newShard(zoneIDs []string, home, addr string, dir Locator, peers HandoffDialer) *Shard {
	s := &Shard{
		zones:      map[string]*Zone{},
		home:       home,
		addr:       addr,
		dir:        dir,
		peers:      peers,
		tokenIndex: map[string]*Zone{},
	}
	for _, id := range zoneIDs {
		z := newDemoZone(id)
		z.shard = s
		z.handoff = s.beginHandoff
		s.zones[id] = z
	}
	if s.zones[home] == nil && len(zoneIDs) > 0 {
		s.home = zoneIDs[0]
	}
	return s
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

// Run starts every hosted zone's actor loop on its own goroutine and blocks until
// ctx is cancelled. One goroutine per zone preserves single-writer.
func (s *Shard) Run(ctx context.Context) {
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
		ctx := context.Background()
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
			_, _ = client.Abort(ctx, &handoffv1.AbortRequest{HandoffToken: resp.GetHandoffToken(), Reason: "directory conflict"})
			fail("ownership conflict")
			return
		}

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

// newRoom authors one room ENTITY (docs/MUDLIB.md §2, §4): an Entity with a Room
// component, keyed in the zone by its ProtoRef and carrying its display name (short)
// and description (long) as entity data, decoupled from the ref (MUDLIB §3). It has no
// location — its container is the zone — and starts with empty contents. This is the
// slice-1 inline authoring that slice 3 replaces with prototype spawning; callers wire
// exits onto the returned Room component and register the entity in z.rooms.
func (z *Zone) newRoom(ref ProtoRef, name, desc string) *Entity {
	e := z.newEntity(ref)
	e.short = name
	e.long = desc
	Add(e, &Room{exits: map[string]ProtoRef{}})
	z.rooms[ref] = e
	return e
}

// newDemoZone builds one of the hardcoded demo zones, authoring its rooms as entities
// keyed by ProtoRef. midgaard's market has a cross-shard exit north into darkwood;
// darkwood's grove leads back south. Phase 4's content loader replaces this function
// body (prototype authoring) without touching callers.
func newDemoZone(id string) *Zone {
	z := newZone(id)
	switch id {
	case "darkwood":
		grove := z.newRoom("darkwood:room:grove", "A Moonlit Grove",
			"Silver birches ring a still clearing; the air hums with quiet magic.")
		hollow := z.newRoom("darkwood:room:hollow", "A Dark Hollow",
			"The trees crowd close and the moonlight fails. Something rustles, unseen.")
		grove.room.exits["south"] = "midgaard:room:market" // back across the shard boundary
		grove.room.exits["north"] = "darkwood:room:hollow"
		hollow.room.exits["south"] = "darkwood:room:grove"
		z.startRoom = "darkwood:room:grove"
	default: // "midgaard"
		temple := z.newRoom("midgaard:room:temple", "The Temple Square",
			"A broad plaza of worn flagstones stretches before the great temple. "+
				"Pilgrims murmur in the shade of its columns.")
		market := z.newRoom("midgaard:room:market", "Market Square",
			"Stalls crowd the square and merchants cry their wares over the din of haggling.")
		temple.room.exits["north"] = "midgaard:room:market"
		market.room.exits["south"] = "midgaard:room:temple"
		market.room.exits["north"] = "darkwood:room:grove" // cross-shard exit
		z.startRoom = "midgaard:room:temple"
	}
	return z
}
