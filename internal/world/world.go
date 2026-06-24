// Package world is the Phase 1 simulation shard: one zone of hardcoded rooms, the
// actor event loop, and the gRPC Play server. Content loading, the full mudlib,
// and multi-zone sharding arrive in later phases (docs/ROADMAP.md).
package world

import (
	"context"

	"google.golang.org/grpc"
)

// Shard holds the zone(s) this world process owns. Phase 1: exactly one.
type Shard struct {
	zone *Zone
}

// NewDemoShard builds the hardcoded two-room demo world.
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

// Run starts the zone's actor loop; returns when ctx is cancelled.
func (s *Shard) Run(ctx context.Context) { s.zone.Run(ctx) }

// Register installs the Play service on a gRPC server.
func (s *Shard) Register(gs *grpc.Server) {
	registerPlay(gs, s.zone)
}
