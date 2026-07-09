package director

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/presence"
)

// TestChannelRosterAggregatorOverRealPresence is the integration seam: the aggregator reads the REAL
// cross-shard presence store (presence.Mem, the same one the two-shard `who` harness shares) rather than a
// hand-rolled fake. Two shards heartbeat their residents' hear-sets into one shared roster; the leader
// aggregator inverts List() to each channel's members and publishes over a real bus. A shard that CRASHES
// (stops heart-beating) ages out of the roster and, on the next poll, drops from its channels — proving the
// membership is TTL-driven and self-correcting, not leave-event-driven.
func TestChannelRosterAggregatorOverRealPresence(t *testing.T) {
	clock := time.Now()
	mem := presence.NewMem()
	mem.SetClock(func() time.Time { return clock })

	ctx := context.Background()
	ttl := 30 * time.Second
	// Shard A hosts Ana (hears gossip). Shard B hosts Bo (hears gossip+trade) and a concealed wizard.
	if err := mem.Set(ctx, "shard-a", []presence.Entry{
		{PlayerID: "p-ana", Name: "Ana", ShardID: "shard-a", Channels: []string{"gossip"}},
	}, ttl); err != nil {
		t.Fatal(err)
	}
	if err := mem.Set(ctx, "shard-b", []presence.Entry{
		{PlayerID: "p-bo", Name: "Bo", ShardID: "shard-b", Channels: []string{"gossip", "trade"}},
		{PlayerID: "p-wiz", Name: "Wiz", ShardID: "shard-b", Concealed: true, Channels: []string{"gossip"}},
	}, ttl); err != nil {
		t.Fatal(err)
	}

	bus := commbus.NewMemBus()
	defer bus.Close()
	gossip := captureRoster(t, bus, "gossip")
	trade := captureRoster(t, bus, "trade")

	d := New("", nil, slog.New(slog.NewTextHandler(discardWriter{}, nil))).
		WithChannelRosterAggregator(mem, bus, time.Second)

	// First aggregate: gossip = {Ana, Bo} (Wiz concealed), trade = {Bo}.
	d.aggregateChannelRosters(ctx)
	if g := nextRoster(t, gossip, "gossip#1"); g != "Ana,Bo" {
		t.Fatalf("gossip = %q, want Ana,Bo (concealed Wiz omitted)", g)
	}
	if g := nextRoster(t, trade, "trade#1"); g != "Bo" {
		t.Fatalf("trade = %q, want Bo", g)
	}

	// Shard B crashes: it stops heart-beating. Advance the clock past its TTL so Bo (and Wiz) age out;
	// shard A keeps Ana alive by heart-beating again.
	clock = clock.Add(ttl + time.Second)
	if err := mem.Set(ctx, "shard-a", []presence.Entry{
		{PlayerID: "p-ana", Name: "Ana", ShardID: "shard-a", Channels: []string{"gossip"}},
	}, ttl); err != nil {
		t.Fatal(err)
	}

	// Next aggregate: gossip republishes {Ana} (Bo aged out); trade lost its last member so it is NOT
	// republished (no subscriber to notify — an emptied channel is simply forgotten until repopulated).
	d.aggregateChannelRosters(ctx)
	if g := nextRoster(t, gossip, "gossip#2"); g != "Ana" {
		t.Fatalf("after shard-b crash, gossip = %q, want Ana", g)
	}
	assertQuiet(t, trade, "trade (emptied by crash)")
}
