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

// presence_who_test.go is the white-box test set for Phase-8 slice 8.4 (cross-shard presence + `who`). It
// runs N demo shards in ONE process against ONE shared in-process roster (presence.NewMem) — the same
// shape the commbus tests use to prove cross-shard delivery — so the done-when (two players on DIFFERENT
// shards both appear in `who`), the crashed-shard age-out, eager clean-quit removal, the write-authority
// (P8-A4), and the batched-heartbeat write rate are all exercised with no live Redis.

// presenceShard builds a demo shard wired with the shared roster under shardID, runs it, and returns its
// home zone. The heartbeat is shrunk so the batched refresh fires within a test's lifetime.
func presenceShard(t *testing.T, shared roster.Roster, shardID string) *Zone {
	t.Helper()
	sh := NewDemoShard().WithPresence(shared, shardID)
	sh.presence.heartbeat = 20 * time.Millisecond // fast beat for the test
	sh.Zone().whoCooldown = 0                     // these tests POLL `who` on one session (waitWho loops)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sh.Run(ctx)
	return sh.Zone()
}

// TestWhoSessionCooldown pins the per-session rate limit: a second `who` inside the cooldown window
// gets the notice, another SESSION is unaffected, and the window expiring restores the list.
func TestWhoSessionCooldown(t *testing.T) {
	z := NewDemoShard().Zone() // no presence → the zone-local path, same cmdWho guard
	z.whoCooldown = 500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go z.Run(ctx)

	alice := joinPlayer(t, z, "Alice")
	bob := joinPlayer(t, z, "Bob")

	drain(alice)
	z.post(inputMsg{id: "Alice", line: "who"})
	waitMarkup(t, alice, "Players online:")

	// Inside the window → the cooldown notice, not a list.
	drain(alice)
	z.post(inputMsg{id: "Alice", line: "who"})
	waitMarkup(t, alice, "You just checked")

	// A different session is not throttled by Alice's mark.
	drain(bob)
	z.post(inputMsg{id: "Bob", line: "who"})
	waitMarkup(t, bob, "Players online:")

	// Past the window → Alice gets the list again. (lastWho is anchored at the first successful who —
	// the notice path doesn't re-mark — so the sleep only needs to beat the window once.)
	time.Sleep(550 * time.Millisecond)
	drain(alice)
	z.post(inputMsg{id: "Alice", line: "who"})
	waitMarkup(t, alice, "Players online:")
}

// joinPlayer joins a fresh player into z and waits until they have arrived (the temple).
func joinPlayer(t *testing.T, z *Zone, name string) *session {
	t.Helper()
	s := newTestPlayerEntity(z, name)
	z.post(joinMsg{s: s})
	waitMarkup(t, s, "The Temple Square")
	return s
}

// waitWho posts `who` from s and returns the rendered cross-shard list once it arrives (skipping the
// arrival/prompt frames). The roster read is async, so we poll a few times.
func waitWho(t *testing.T, z *Zone, s *session) string {
	t.Helper()
	drain(s)
	z.post(inputMsg{id: s.character, line: "who"})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), "Players online:") {
				return o.GetMarkup()
			}
		case <-deadline:
			t.Fatalf("player %s: timed out waiting for who", s.character)
			return ""
		}
	}
}

// TestCrossShardWho is the slice-8.4 done-when: two players on DIFFERENT shards both appear in `who`.
func TestCrossShardWho(t *testing.T) {
	shared := roster.NewMem()
	za := presenceShard(t, shared, "shard-a")
	zb := presenceShard(t, shared, "shard-b")

	alice := joinPlayer(t, za, "Alice")
	joinPlayer(t, zb, "Bob")

	// `who` from Alice (shard-a) must list BOTH her and Bob (shard-b) — the cross-shard roster read, not
	// the zone-local list (which would show only Alice). Poll until the eager join writes have landed.
	deadline := time.After(2 * time.Second)
	for {
		who := waitWho(t, za, alice)
		if strings.Contains(who, "Alice") && strings.Contains(who, "Bob") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("cross-shard who never listed both players (last: %q)", who)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestCleanQuitEagerRemoval: a clean leave removes the player from `who` IMMEDIATELY (before any TTL).
func TestCleanQuitEagerRemoval(t *testing.T) {
	shared := roster.NewMem()
	za := presenceShard(t, shared, "shard-a")
	zb := presenceShard(t, shared, "shard-b")

	alice := joinPlayer(t, za, "Alice")
	joinPlayer(t, zb, "Bob")

	// Both present first.
	deadline := time.After(2 * time.Second)
	for {
		if who := waitWho(t, za, alice); strings.Contains(who, "Bob") {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Bob never appeared in who before the quit test")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// Bob cleanly leaves shard-b. The eager Remove drops him from the roster at once (TTL is 30s; this
	// must NOT wait it out).
	zb.post(leaveMsg{id: "Bob"})
	gone := time.After(2 * time.Second)
	for {
		who := waitWho(t, za, alice)
		if !strings.Contains(who, "Bob") {
			break // eagerly removed
		}
		select {
		case <-gone:
			t.Fatalf("a cleanly-quit player was not removed from who eagerly (still: %q)", who)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestCrashedShardAgeOutWho is the P8-A4 self-healing demonstration through the world API: a shard that
// stops heartbeating leaves its players in who only until the TTL lapses, then they age out with NO
// explicit cleanup. We drive the Mem roster's clock so the age-out is deterministic.
func TestCrashedShardAgeOutWho(t *testing.T) {
	shared := roster.NewMem()
	base := time.Now()
	var nowNs atomic.Int64
	nowNs.Store(base.UnixNano())
	shared.SetClock(func() time.Time { return time.Unix(0, nowNs.Load()) })

	za := presenceShard(t, shared, "shard-a")
	zb := presenceShard(t, shared, "shard-b")
	// Shrink shard-b's TTL so the age-out window is small; shard-a keeps the default so Alice never lapses.
	zbShard := zb.shard
	zbShard.presence.ttl = 2 * time.Second

	alice := joinPlayer(t, za, "Alice")
	joinPlayer(t, zb, "Bob")

	// Both present.
	deadline := time.After(2 * time.Second)
	for {
		if who := waitWho(t, za, alice); strings.Contains(who, "Bob") {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Bob never appeared before the crash test")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// shard-b "crashes": stop its heartbeat loop AND its eager writes by cancelling its run. We simulate
	// the crash by disabling the tracker's roster (no more writes), then advancing the clock past Bob's
	// TTL. (A real crash kills the process; here we just stop writing for shard-b.)
	zbShard.presence.mu.Lock()
	zbShard.presence.roster = nil // shard-b stops all presence I/O — the crash
	zbShard.presence.mu.Unlock()

	// Advance the clock past Bob's TTL; Alice (shard-a, still live) keeps refreshing so she stays.
	nowNs.Add(int64(5 * time.Second))

	aged := time.After(2 * time.Second)
	for {
		who := waitWho(t, za, alice)
		if !strings.Contains(who, "Bob") {
			if !strings.Contains(who, "Alice") {
				t.Fatalf("live shard's player aged out too (Alice gone): %q", who)
			}
			break // Bob aged out, Alice still present — the self-healing path
		}
		select {
		case <-aged:
			t.Fatalf("crashed shard's player never aged out of who: %q", who)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestPresenceWriteAuthorityThroughWorld is the P8-A4 write-authority test at the world boundary: a shard
// hosts only its own residents, so the tracker can never assert a player it doesn't host. We drive the
// tracker directly: shard-b cannot mark/evict a player shard-a hosts.
func TestPresenceWriteAuthorityThroughWorld(t *testing.T) {
	shared := roster.NewMem()
	za := presenceShard(t, shared, "shard-a")
	_ = presenceShard(t, shared, "shard-b")
	joinPlayer(t, za, "Alice") // shard-a hosts Alice

	// Wait until shard-a's eager join write has landed so Alice is genuinely owned by shard-a (otherwise an
	// unowned key would legitimately accept any shard's write — that is not the case under test).
	deadline := time.After(2 * time.Second)
	for {
		list, _ := shared.List(context.Background())
		owned := false
		for _, e := range list {
			if e.PlayerID == "Alice" && e.ShardID == "shard-a" {
				owned = true
			}
		}
		if owned {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Alice never became owned by shard-a in the roster")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// shard-b's tracker can only Set what it claims to host; the store refuses Alice (shard-a owns her).
	// A direct over-reaching Set is refused with ErrNotOwner and Alice's entry is untouched.
	err := shared.Set(context.Background(), "shard-b",
		[]roster.Entry{{PlayerID: "Alice", Name: "Imposter", ShardID: "shard-b"}}, roster.DefaultTTL)
	if !errors.Is(err, roster.ErrNotOwner) {
		t.Fatalf("a non-host shard's write was not refused: %v", err)
	}
	list, _ := shared.List(context.Background())
	for _, e := range list {
		if e.PlayerID == "Alice" && (e.ShardID != "shard-a" || e.Name != "Alice") {
			t.Fatalf("a non-host shard overwrote a real player's presence: %+v", e)
		}
	}
	// And shard-b cannot evict Alice.
	_ = shared.Remove(context.Background(), "shard-b", "Alice")
	list, _ = shared.List(context.Background())
	found := false
	for _, e := range list {
		if e.PlayerID == "Alice" {
			found = true
		}
	}
	if !found {
		t.Fatal("a non-host shard evicted a real player from the roster")
	}
}

// countingRoster wraps a Roster to count Set calls and the max batch size — the batched-heartbeat
// write-rate assertion: the heartbeat is ONE Set per beat (a batch of all residents), never one Set per
// player per beat.
type countingRoster struct {
	inner    roster.Roster
	setCalls atomic.Int64
	maxBatch atomic.Int64
}

func (c *countingRoster) Set(ctx context.Context, shardID string, entries []roster.Entry, ttl time.Duration) error {
	c.setCalls.Add(1)
	if n := int64(len(entries)); n > c.maxBatch.Load() {
		c.maxBatch.Store(n)
	}
	return c.inner.Set(ctx, shardID, entries, ttl)
}

func (c *countingRoster) Remove(ctx context.Context, shardID, playerID string) error {
	return c.inner.Remove(ctx, shardID, playerID)
}

func (c *countingRoster) List(ctx context.Context) ([]roster.Entry, error) { return c.inner.List(ctx) }

// TestBatchedHeartbeatWriteRate proves the heartbeat is a single BATCHED Set per beat covering all
// residents, not one Set per player per beat. With 3 residents and several beats, the heartbeat Sets must
// carry a batch of all 3 in one call (maxBatch == 3) — the O(shards/interval) write rate, not O(players).
func TestBatchedHeartbeatWriteRate(t *testing.T) {
	counter := &countingRoster{inner: roster.NewMem()}
	sh := NewDemoShard().WithPresence(counter, "shard-a")
	sh.presence.heartbeat = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	z := sh.Zone()

	for _, name := range []string{"Ann", "Ben", "Cas"} {
		joinPlayer(t, z, name)
	}

	// Let several heartbeats fire.
	deadline := time.After(2 * time.Second)
	for counter.maxBatch.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("heartbeat never batched all residents into one Set (maxBatch=%d, calls=%d)",
				counter.maxBatch.Load(), counter.setCalls.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The whole resident set went out in ONE Set call (batch of 3). If presence wrote one-per-player-per-
	// beat, maxBatch would be 1 regardless of how many residents there are.
	if counter.maxBatch.Load() != 3 {
		t.Fatalf("heartbeat batch size = %d, want 3 (one batched write per beat, not per player)",
			counter.maxBatch.Load())
	}
}
