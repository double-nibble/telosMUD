// Package contentbus is the content hot-reload signalling layer (docs/PHASE4-PLAN.md §5). A
// content WRITER (an OLC tool, a deploy step, or `make seed`) publishes an INVALIDATION naming
// the (kind, ref, pack) that changed; every running shard SUBSCRIBES and, on a message, re-reads
// just that one definition row and swaps the rebuilt *Prototype into its per-shard cache — so the
// next spawn uses the edit with no restart and no live-instance corruption.
//
// # The interface boundary (mirrors slice 4.2's CharacterStore/MemStore)
//
// Everything goes through the Bus interface so the reload logic is unit-testable WITHOUT a live
// NATS: tests drive an in-memory MemBus exactly as the durability-ladder tests drive a MemStore,
// and production wires the NATS-backed bus. The world package depends only on Bus, never on
// nats.go — so a storeless/busless shard is byte-identical to a pre-4.3 shard, and the gated
// real-NATS test is the only thing that needs a broker.
//
// # Optional, never fatal
//
// The bus is OPTIONAL: if NATS is unreachable the world simply gets a nil/disabled Bus and hot
// reload is DISABLED — boot-load still works, every existing test stays green, and the engine
// degrades exactly as it does when Redis or Postgres is down. A publish/subscribe failure is
// logged, never crashes the world.
package contentbus

import (
	"context"
	"encoding/json"
)

// Subject is the NATS subject invalidations are published on / subscribed to. A single subject
// carries every (kind, ref, pack) tuple; the payload discriminates, so a shard takes one
// subscription and filters in the handler (the message volume is tiny — one per content edit).
const Subject = "content.invalidate"

// Invalidation is the published payload: the single definition that changed. kind is the
// definition KIND (== the table: "room" | "item" | "mob" | "zone"), ref is its stable content
// key (the prototype/room ref), and pack scopes it to a content pack so a shard can ignore an
// edit to a pack it does not load. The shard re-reads exactly (kind, ref) from its content source
// and reloads that one prototype.
type Invalidation struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Pack string `json:"pack"`
}

// marshal/unmarshal keep the wire format in one place so the NATS bus and any future transport
// agree, and so a malformed payload is a handled error rather than a panic in a subscriber.
func (inv Invalidation) marshal() ([]byte, error) { return json.Marshal(inv) }

func unmarshalInvalidation(data []byte) (Invalidation, error) {
	var inv Invalidation
	err := json.Unmarshal(data, &inv)
	return inv, err
}

// Bus is the publish/subscribe contract for content invalidations. Two implementations exist: a
// NATS-backed Bus (production) and an in-memory MemBus (tests + a bare run), mirroring the
// CharacterStore/MemStore split in slice 4.2 so the reload path is unit-testable with no broker.
//
// It is deliberately small: publish one invalidation, subscribe to deliver invalidations to a
// handler, and close. The whole bus is OPTIONAL — a nil Bus means hot reload is disabled (the
// world checks for nil), so a busless shard behaves exactly as before slice 4.3.
type Bus interface {
	// Publish broadcasts one invalidation to every subscribed shard. Called by the content
	// writer (OLC/deploy/seed) AFTER the row write commits, so a subscriber that re-reads on
	// receipt sees the new data. Off any zone goroutine.
	Publish(ctx context.Context, inv Invalidation) error

	// Subscribe registers handler to be called once per received invalidation, on a background
	// goroutine the bus owns (never a zone goroutine — the handler posts to the zone inbox or
	// does its own off-goroutine I/O). It returns a Subscription whose Unsubscribe stops
	// delivery. handler must be safe to call concurrently with itself ONLY if the impl says so;
	// both impls here deliver serially per subscription, so a handler that serializes the cache
	// swap (the world's applier) needs no extra lock.
	Subscribe(handler func(Invalidation)) (Subscription, error)

	// Close releases the bus (the NATS connection / the MemBus subscriber set). Idempotent.
	Close() error
}

// Subscription is a live subscription handle. Unsubscribe stops further delivery to its handler;
// it is idempotent and safe to call from any goroutine.
type Subscription interface {
	Unsubscribe() error
}
