package world

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	roster "github.com/double-nibble/telosmud/internal/presence"
)

// presence_chaos_test.go is W8 failure-injection for the presence/roster boundary (the Redis-backed
// cross-shard `who`). The never-fatal contract here (presence.go rosterList, commands.go cmdWho): a
// roster READ error degrades `who` to the zone-LOCAL player list — never an error, never a crash to the
// player. The shipped tests all use a healthy roster.NewMem, so the degradation branch was unreached.

// failingRoster wraps a real Roster and returns an injected error from List while `failList` is set
// (Set/Remove delegate, so the heartbeat keeps working — only the READ path fails, which is the
// "Redis read times out / is unreachable" shape `who` must survive).
type failingRoster struct {
	inner    roster.Roster
	failList atomic.Bool
}

func (f *failingRoster) Set(ctx context.Context, shardID string, entries []roster.Entry, ttl time.Duration) error {
	return f.inner.Set(ctx, shardID, entries, ttl)
}

func (f *failingRoster) Remove(ctx context.Context, shardID, playerID string) error {
	return f.inner.Remove(ctx, shardID, playerID)
}

func (f *failingRoster) List(ctx context.Context) ([]roster.Entry, error) {
	if f.failList.Load() {
		return nil, errors.New("injected: roster backend unavailable")
	}
	return f.inner.List(ctx)
}

// listCountRoster wraps a roster and counts List calls, to prove the who-read cache collapses a `who` flood.
type listCountRoster struct {
	roster.Roster
	lists atomic.Int64
}

func (c *listCountRoster) List(ctx context.Context) ([]roster.Entry, error) {
	c.lists.Add(1)
	return c.Roster.List(ctx)
}

// TestWhoCacheCollapsesReads pins the scale win: many `who` reads within whoCacheTTL do ONE Redis SCAN
// (List), not one each; after the window a read refreshes. This is the anti-`who`-flood guard.
func TestWhoCacheCollapsesReads(t *testing.T) {
	cr := &listCountRoster{Roster: roster.NewMem()}
	za := presenceShard(t, cr, "shard-a")
	joinPlayer(t, za, "Alice")
	tracker := za.shard.presence

	for i := 0; i < 25; i++ {
		if _, ok := tracker.cachedList(context.Background()); !ok {
			t.Fatal("cachedList degraded unexpectedly")
		}
	}
	if n := cr.lists.Load(); n != 1 {
		t.Fatalf("25 rapid who reads did %d Redis SCANs, want 1 (the cache must collapse them)", n)
	}

	time.Sleep(whoCacheTTL + 50*time.Millisecond)
	if _, ok := tracker.cachedList(context.Background()); !ok {
		t.Fatal("cachedList degraded after the window")
	}
	if n := cr.lists.Load(); n != 2 {
		t.Fatalf("after the cache window: %d SCANs, want 2 (one refresh)", n)
	}
}

// TestWhoDegradesToLocalOnRosterReadFailure pins the never-fatal presence contract: when the roster
// read fails (a Redis blip), `who` falls back to the zone-LOCAL list — the player still gets a valid
// listing of who is here, never an error and never the (now-unreadable) cross-shard view. A seeded
// remote player proves the difference: healthy `who` shows them (cross-shard read), the degraded `who`
// does NOT (local fallback), and on recovery they reappear.
func TestWhoDegradesToLocalOnRosterReadFailure(t *testing.T) {
	inner := roster.NewMem()
	fr := &failingRoster{inner: inner}
	za := presenceShard(t, fr, "shard-a")

	alice := joinPlayer(t, za, "Alice")
	// Seed a REMOTE player (shard-b) directly into the roster so a HEALTHY cross-shard who would list
	// them, but the zone-local fallback would not (Bob is not a resident of shard-a's zone).
	if err := inner.Set(context.Background(), "shard-b",
		[]roster.Entry{{PlayerID: "Bob", Name: "Bob", ShardID: "shard-b"}}, roster.DefaultTTL); err != nil {
		t.Fatal(err)
	}

	// HEALTHY: who is the cross-shard read — it lists the remote Bob. Poll until the roster shows both.
	for done, deadline := false, 100; !done && deadline > 0; deadline-- {
		if strings.Contains(waitWho(t, za, alice), "Bob") {
			done = true
		}
		if !done && deadline == 1 {
			t.Fatal("healthy who never listed the seeded cross-shard player")
		}
	}

	// FAILURE: the roster read errors → who DEGRADES to the zone-local list. The player still gets a valid
	// "Players online:" listing with the local player, and the cross-shard Bob is absent (the fallback can't
	// see him) — never an error to the player. NOTE: the who-read cache (whoCacheTTL) may serve the last-good
	// cross-shard snapshot for up to a window after the read starts failing, so poll until the cache lapses
	// and the fallback kicks in (a ≤1s stale view during a blip is acceptable, and better than an instant
	// vanish of the whole cross-shard roster).
	fr.failList.Store(true)
	var degraded string
	for deadline := 0; deadline < 100; deadline++ {
		degraded = waitWho(t, za, alice)
		if !strings.Contains(degraded, "Bob") {
			break // the cache lapsed; the failing read now degrades to the local list
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !strings.Contains(degraded, "Alice") {
		t.Fatalf("degraded who dropped the local player; got %q", degraded)
	}
	if strings.Contains(degraded, "Bob") {
		t.Fatalf("who still showed a cross-shard player after the cache window despite the read failing: %q", degraded)
	}

	// RECOVERY: the roster read works again → who shows the cross-shard view (Bob reappears).
	fr.failList.Store(false)
	for done, deadline := false, 100; !done && deadline > 0; deadline-- {
		if strings.Contains(waitWho(t, za, alice), "Bob") {
			done = true
		}
		if !done && deadline == 1 {
			t.Fatal("who did not recover the cross-shard view after the roster read recovered")
		}
	}
}
