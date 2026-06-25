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

// Shard holds the zone this world process owns, its public address (what the gate
// and peer shards dial), and the directory used to route cross-shard moves. Phase 2
// runs one zone per shard; multi-zone shards come later.
type Shard struct {
	zone  *Zone
	addr  string        // this shard's public address ("" in single-shard tests)
	dir   Locator       // directory for cross-shard routing; nil seals cross-shard exits
	peers HandoffDialer // dials peer shards' Handoff service
}

// NewDemoShard builds a single-shard midgaard world with no directory wiring — its
// cross-shard exits are sealed. Used by the single-shard tests and a bare run.
func NewDemoShard() *Shard {
	return &Shard{zone: newDemoZone("midgaard")}
}

// NewShard builds the named demo zone and wires it for cross-shard handoff: addr is
// this shard's public address, dir routes moves into zones other shards own, and
// peers dials those shards' Handoff service.
func NewShard(zoneID, addr string, dir Locator, peers HandoffDialer) *Shard {
	s := &Shard{zone: newDemoZone(zoneID), addr: addr, dir: dir, peers: peers}
	s.zone.handoff = s.beginHandoff
	return s
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

// Zone returns the shard's zone.
func (s *Shard) Zone() *Zone { return s.zone }

// Run starts the zone's actor loop and blocks until ctx is cancelled.
func (s *Shard) Run(ctx context.Context) { s.zone.Run(ctx) }

// Register installs the gRPC Play and Handoff services on the given server.
func (s *Shard) Register(gs *grpc.Server) {
	registerPlay(gs, s.zone)
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
func (s *Shard) beginHandoff(snap *handoffv1.PlayerSnapshot, destZone, destRoom string, epoch uint64) {
	go func() {
		ctx := context.Background()
		character := snap.GetCharacterId()
		newEpoch := epoch + 1
		log := slog.With("component", "handoff", "player", character, "dest_zone", destZone)

		fail := func(reason string) { s.zone.post(handoffFailMsg{id: character, reason: reason}) }

		addr, err := s.dir.ShardForZone(ctx, destZone)
		if err != nil {
			log.Warn("destination zone not in directory", "err", err)
			fail("destination unreachable")
			return
		}
		client, err := s.peers(addr)
		if err != nil {
			log.Warn("cannot reach destination shard", "addr", addr, "err", err)
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

		// Prepare succeeded: claim ownership in the directory (epoch CAS). On conflict,
		// roll back the destination's pending entity.
		if ok, err := s.dir.SetPlayerShard(ctx, character, addr, newEpoch); err != nil || !ok {
			log.Warn("directory claim failed after prepare", "ok", ok, "err", err)
			_, _ = client.Abort(ctx, &handoffv1.AbortRequest{HandoffToken: resp.GetHandoffToken(), Reason: "directory conflict"})
			fail("ownership conflict")
			return
		}

		log.Debug("prepared + ownership claimed; redirecting", "dest_addr", resp.GetTargetShardAddr(), "epoch", newEpoch)
		s.zone.post(redirectMsg{
			id:         character,
			targetAddr: resp.GetTargetShardAddr(),
			token:      resp.GetHandoffToken(),
			resumeSeq:  snap.GetAppliedSeq(),
			epoch:      newEpoch,
		})
	}()
}

// newDemoZone builds one of the hardcoded demo zones. midgaard's market has a
// cross-shard exit north into darkwood; darkwood's grove leads back south.
func newDemoZone(id string) *Zone {
	z := newZone(id)
	switch id {
	case "darkwood":
		grove := newRoom("grove", "A Moonlit Grove",
			"Silver birches ring a still clearing; the air hums with quiet magic.")
		hollow := newRoom("hollow", "A Dark Hollow",
			"The trees crowd close and the moonlight fails. Something rustles, unseen.")
		grove.exits["south"] = "midgaard:market" // back across the shard boundary
		grove.exits["north"] = "darkwood:hollow"
		hollow.exits["south"] = "darkwood:grove"
		z.rooms["grove"], z.rooms["hollow"] = grove, hollow
		z.startRoom = "grove"
	default: // "midgaard"
		temple := newRoom("temple", "The Temple Square",
			"A broad plaza of worn flagstones stretches before the great temple. "+
				"Pilgrims murmur in the shade of its columns.")
		market := newRoom("market", "Market Square",
			"Stalls crowd the square and merchants cry their wares over the din of haggling.")
		temple.exits["north"] = "midgaard:market"
		market.exits["south"] = "midgaard:temple"
		market.exits["north"] = "darkwood:grove" // cross-shard exit
		z.rooms["temple"], z.rooms["market"] = temple, market
		z.startRoom = "temple"
	}
	return z
}
