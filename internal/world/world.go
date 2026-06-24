// Package world is the Phase 1 simulation shard: one zone of hardcoded rooms, the
// actor event loop, and the gRPC Play server. Content loading, the full mudlib,
// and multi-zone sharding arrive in later phases (docs/ROADMAP.md).
//
// # The actor-per-zone model (docs/ARCHITECTURE.md §3)
//
// A Zone is owned by exactly one goroutine (Zone.Run). Rooms and players are plain
// data, not goroutines; only the zone goroutine reads or mutates them, so game logic
// needs no locks. Every interaction from outside that goroutine — a player's gRPC
// stream handler, and later cross-zone senders — happens by posting a message to the
// zone inbox (Zone.post), never by touching zone state directly. The engine is pure
// mechanism: nothing about game flavor is hardcoded into the loop.
//
// # Following a command end to end
//
// The path from "player input arrives" to "frame sent back" crosses three goroutines:
//
//	gRPC reader (server.go) -> zone inbox -> zone loop (zone.go/commands.go)
//	                                      -> player.out -> gRPC writer (server.go) -> wire
//
// Run with DEBUG=1 (see internal/obs) to watch every step of that path narrated via
// slog.Debug: stream connect, attach, join, each input, dispatch, movement,
// broadcasts, leave, and any dropped frames.
package world

import (
	"context"

	"google.golang.org/grpc"
)

// Shard holds the zone(s) this world process owns. Phase 1: exactly one.
type Shard struct {
	zone *Zone
}

// NewDemoShard builds the hardcoded two-room demo world (Temple <-> Market) and
// returns the shard that owns it. The world is wired by hand here; content-driven
// loading arrives in a later phase.
func NewDemoShard() *Shard {
	z := newZone("midgaard")

	temple := newRoom("temple", "The Temple Square",
		"A broad plaza of worn flagstones stretches before the great temple. "+
			"Pilgrims murmur in the shade of its columns.")
	market := newRoom("market", "Market Square",
		"Stalls crowd the square and merchants cry their wares over the din of haggling.")

	temple.exits["north"] = "market"
	market.exits["south"] = "temple"

	z.rooms[temple.id] = temple
	z.rooms[market.id] = market
	z.startRoom = temple.id

	return &Shard{zone: z}
}

// Zone returns the shard's zone (Phase 1 single-zone accessor).
func (s *Shard) Zone() *Zone { return s.zone }

// Run starts the zone's actor loop (the single owning goroutine) and blocks until
// ctx is cancelled. Callers run this in its own goroutine.
func (s *Shard) Run(ctx context.Context) { s.zone.Run(ctx) }

// Register installs the gRPC Play service (the network bridge into the zone) on the
// given server.
func (s *Shard) Register(gs *grpc.Server) {
	registerPlay(gs, s.zone)
}
