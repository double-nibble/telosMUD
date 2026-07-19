package contentbus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/content"
)

// publish424_test.go — the publisher half of #424.
//
// The shard-side filter now fails CLOSED on an empty pack, which makes an unnamed pack here a whole pack's
// worth of messages that reach NOBODY — a fleet-wide no-op reported as success. That is the same class of
// silent failure #424 is about, so PublishPack refuses it. This file exists because a review proved the
// guard had no coverage at all: neutering it left the entire suite green, so a later refactor could drop it
// and reintroduce the silent bug undetected.

// countingBus records every published invalidation so a test can assert on what actually reached the wire,
// not merely on the returned count.
type countingBus struct{ published []Invalidation }

func (b *countingBus) Publish(_ context.Context, inv Invalidation) error {
	b.published = append(b.published, inv)
	return nil
}
func (b *countingBus) Subscribe(func(Invalidation)) (Subscription, error) { return nil, nil }
func (b *countingBus) OnReconnect(func())                                 {}
func (b *countingBus) Close() error                                       { return nil }

func namelessPack() content.Pack {
	return content.Pack{
		Zones: []content.ZoneDTO{{
			Ref: "z", Name: "Z", StartRoom: "z:room:a",
			Rooms: []content.RoomDTO{{Ref: "z:room:a", Name: "A"}},
			Items: []content.ProtoDTO{{Ref: "z:obj:torch", Short: "a torch"}},
		}},
		Channels: []content.ChannelDTO{{Ref: "gossip", Name: "Gossip"}},
	}
}

// TestPublishPackRefusesAPackWithNoName asserts the refusal AND that nothing reached the wire. Asserting
// only the returned error would still pass a version that errored after publishing half a pack.
func TestPublishPackRefusesAPackWithNoName(t *testing.T) {
	bus := &countingBus{}
	n, err := PublishPack(context.Background(), bus, namelessPack(), 1)

	require.Error(t, err, "an unnamed pack publishes to nobody (every subscriber fails closed) and must not "+
		"be reported as a successful broadcast")
	require.Zero(t, n)
	require.Empty(t, bus.published, "nothing may reach the wire for an unnamed pack")
}

// TestPublishPackStillPublishesANamedPack is the positive control: the guard must refuse only the nameless
// case, or it would silently disable hot reload entirely.
func TestPublishPackStillPublishesANamedPack(t *testing.T) {
	pk := namelessPack()
	pk.Pack = "demo"
	bus := &countingBus{}

	n, err := PublishPack(context.Background(), bus, pk, 1)
	require.NoError(t, err)
	require.Equal(t, 3, n, "one room + one item + one channel (the zone invalidation is not counted)")
	require.Len(t, bus.published, 4, "...plus the trailing KindZone shape invalidation")
	for _, inv := range bus.published {
		require.Equal(t, "demo", inv.Pack, "every published invalidation must carry the pack name")
	}
}
