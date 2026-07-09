package director

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/presence"
)

// fakeRosterSource is a hermetic ChannelRosterSource: it returns a fixed roster (swappable between polls
// to exercise the diff) so the aggregator's invert/diff/publish runs with no Redis.
type fakeRosterSource struct {
	mu      sync.Mutex
	entries []presence.Entry
}

func (f *fakeRosterSource) set(e []presence.Entry) {
	f.mu.Lock()
	f.entries = e
	f.mu.Unlock()
}

func (f *fakeRosterSource) List(context.Context) ([]presence.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]presence.Entry(nil), f.entries...), nil
}

// TestInvertRoster pins the pure inversion: flat cross-shard roster -> per-channel sorted listener names,
// deduped by PlayerID (a handoff momentarily shows a player on two shards — still ONE member), with
// concealed players dropped entirely.
func TestInvertRoster(t *testing.T) {
	got := invertRoster([]presence.Entry{
		{PlayerID: "p-ana", Name: "Ana", ShardID: "a", Channels: []string{"gossip"}},
		{PlayerID: "p-bo", Name: "Bo", ShardID: "a", Channels: []string{"gossip", "trade"}},
		{PlayerID: "p-bo", Name: "Bo", ShardID: "b", Channels: []string{"gossip", "trade"}}, // handoff dup
		{PlayerID: "p-ghost", Name: "Ghost", ShardID: "a", Concealed: true, Channels: []string{"gossip"}},
	})
	if g := strings.Join(got["gossip"], ","); g != "Ana,Bo" {
		t.Fatalf("gossip = %q, want Ana,Bo (deduped, sorted, Ghost concealed)", g)
	}
	if g := strings.Join(got["trade"], ","); g != "Bo" {
		t.Fatalf("trade = %q, want Bo", g)
	}
	if _, ok := got["private"]; ok {
		t.Fatalf("no member hears 'private'; it must not appear")
	}
}

// captureRoster subscribes to a channel's roster subject for the whole test and returns the channel each
// published listener set lands on (delivery is async on the MemBus). One persistent subscription lets a
// test both read the next publish and assert none arrived.
func captureRoster(t *testing.T, bus *commbus.MemBus, ref string) <-chan []string {
	t.Helper()
	ch := make(chan []string, 8)
	sub, err := bus.Subscribe(commbus.RosterSubject(ref), func(m commbus.Message) {
		var in struct {
			Channel string   `json:"channel"`
			Players []string `json:"players"`
		}
		if err := json.Unmarshal([]byte(m.Body), &in); err != nil {
			return
		}
		ch <- in.Players
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", ref, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return ch
}

func nextRoster(t *testing.T, ch <-chan []string, what string) string {
	t.Helper()
	select {
	case p := <-ch:
		return strings.Join(p, ",")
	case <-time.After(2 * time.Second):
		t.Fatalf("no roster published for %s", what)
		return ""
	}
}

func assertQuiet(t *testing.T, ch <-chan []string, what string) {
	t.Helper()
	select {
	case p := <-ch:
		t.Fatalf("unexpected roster republish for unchanged %s: %v", what, p)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestAggregateChannelRostersDiff pins the aggregate cycle end to end: the first poll publishes every
// channel's roster; an unchanged poll republishes NOTHING; a changed channel republishes ONLY that channel.
func TestAggregateChannelRostersDiff(t *testing.T) {
	bus := commbus.NewMemBus()
	defer bus.Close()
	gossip := captureRoster(t, bus, "gossip")
	trade := captureRoster(t, bus, "trade")

	src := &fakeRosterSource{}
	src.set([]presence.Entry{
		{PlayerID: "p-ana", Name: "Ana", Channels: []string{"gossip"}},
		{PlayerID: "p-bo", Name: "Bo", Channels: []string{"gossip", "trade"}},
	})
	d := New("", nil, slog.New(slog.NewTextHandler(discardWriter{}, nil))).
		WithChannelRosterAggregator(src, bus, time.Second)

	// First poll: both channels are new -> both published.
	d.aggregateChannelRosters(context.Background())
	if g := nextRoster(t, gossip, "gossip#1"); g != "Ana,Bo" {
		t.Fatalf("first gossip = %q, want Ana,Bo", g)
	}
	if g := nextRoster(t, trade, "trade#1"); g != "Bo" {
		t.Fatalf("first trade = %q, want Bo", g)
	}

	// Unchanged poll: nothing republished on either channel.
	d.aggregateChannelRosters(context.Background())
	assertQuiet(t, gossip, "gossip (unchanged)")
	assertQuiet(t, trade, "trade (unchanged)")

	// Ana leaves gossip -> only gossip changes; trade (still {Bo}) is NOT republished.
	src.set([]presence.Entry{
		{PlayerID: "p-bo", Name: "Bo", Channels: []string{"gossip", "trade"}},
	})
	d.aggregateChannelRosters(context.Background())
	if g := nextRoster(t, gossip, "gossip#2"); g != "Bo" {
		t.Fatalf("after Ana leaves, gossip = %q, want Bo", g)
	}
	assertQuiet(t, trade, "trade (still Bo)")
}

// TestAggregateChannelRostersPeriodicResync pins the F1 fix: a roster is convergent state, so every
// rosterResyncEvery-th poll republishes ALL channels regardless of the diff — a transient publish dropped for
// some subscriber converges without waiting for the next membership change.
func TestAggregateChannelRostersPeriodicResync(t *testing.T) {
	bus := commbus.NewMemBus()
	defer bus.Close()
	gossip := captureRoster(t, bus, "gossip")

	src := &fakeRosterSource{}
	src.set([]presence.Entry{{PlayerID: "p-ana", Name: "Ana", Channels: []string{"gossip"}}})
	d := New("", nil, slog.New(slog.NewTextHandler(discardWriter{}, nil))).
		WithChannelRosterAggregator(src, bus, time.Second)

	// Poll #0 is a full resync (counter starts at 0) -> publishes.
	d.aggregateChannelRosters(context.Background())
	if g := nextRoster(t, gossip, "resync#0"); g != "Ana" {
		t.Fatalf("first resync = %q, want Ana", g)
	}
	// The next rosterResyncEvery-1 polls see no change -> quiet (the diff coalesces).
	for i := 1; i < rosterResyncEvery; i++ {
		d.aggregateChannelRosters(context.Background())
	}
	assertQuiet(t, gossip, "between resyncs")
	// The next poll's counter is a multiple of rosterResyncEvery -> full resync republishes despite no change.
	d.aggregateChannelRosters(context.Background())
	if g := nextRoster(t, gossip, "periodic-resync"); g != "Ana" {
		t.Fatalf("periodic resync = %q, want Ana (republished with no membership change)", g)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
