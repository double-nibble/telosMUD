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

	// FAILURE: the roster read errors → who DEGRADES to the zone-local list. The player still gets a
	// valid "Players online:" listing with the local player, and the cross-shard Bob is absent (the
	// fallback can't see him) — never an error to the player.
	fr.failList.Store(true)
	degraded := waitWho(t, za, alice)
	if !strings.Contains(degraded, "Alice") {
		t.Fatalf("degraded who dropped the local player; got %q", degraded)
	}
	if strings.Contains(degraded, "Bob") {
		t.Fatalf("who showed a cross-shard player despite the roster read failing — it did not fall back to local: %q", degraded)
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
