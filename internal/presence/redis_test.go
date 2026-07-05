package presence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (*Redis, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedis(rdb, "test"), mr
}

// rosters runs a roster test against BOTH the Redis (miniredis) and Mem implementations so they stay in
// lockstep on the write-authority + age-out semantics. The fast-forward hook advances each store's clock
// (miniredis.FastForward / Mem.SetClock) so TTL age-out is deterministic.
type rosterUnderTest struct {
	name string
	r    Roster
	ff   func(time.Duration)
}

func eachRoster(t *testing.T) []rosterUnderTest {
	rr, mr := newTestRedis(t)
	mem := NewMem()
	base := time.Now()
	memNow := base
	mem.SetClock(func() time.Time { return memNow })
	return []rosterUnderTest{
		{"redis", rr, func(d time.Duration) { mr.FastForward(d) }},
		{"mem", mem, func(d time.Duration) { memNow = memNow.Add(d) }},
	}
}

func TestSetAndListCrossShard(t *testing.T) {
	ctx := context.Background()
	for _, tc := range eachRoster(t) {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.r.Set(ctx, "shard-a", []Entry{{PlayerID: "Alice", Name: "Alice"}}, DefaultTTL); err != nil {
				t.Fatal(err)
			}
			if err := tc.r.Set(ctx, "shard-b", []Entry{{PlayerID: "Bob", Name: "Bob"}}, DefaultTTL); err != nil {
				t.Fatal(err)
			}
			got, err := tc.r.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			names := map[string]string{}
			for _, e := range got {
				names[e.Name] = e.ShardID
			}
			if names["Alice"] != "shard-a" || names["Bob"] != "shard-b" {
				t.Fatalf("cross-shard roster missing a player: %+v", got)
			}
		})
	}
}

// TestEntryFieldsRoundTrip guards the store field-drop trap (#98): the Redis roster marshals Entry
// field-by-field (a Lua HSET + HMGET), so a newly-added field must be threaded through BOTH the write and the
// read. Set an entry carrying every non-default flag and assert List returns them intact on both impls.
func TestEntryFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	for _, tc := range eachRoster(t) {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.r.Set(ctx, "shard-a", []Entry{
				{PlayerID: "Sneak", Name: "Sneak", AFK: true, Concealed: true},
				{PlayerID: "Plain", Name: "Plain"}, // both flags default false
			}, DefaultTTL); err != nil {
				t.Fatal(err)
			}
			got, err := tc.r.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]Entry{}
			for _, e := range got {
				byID[e.PlayerID] = e
			}
			if !byID["Sneak"].Concealed || !byID["Sneak"].AFK {
				t.Fatalf("Concealed/AFK flags dropped in round-trip: %+v", byID["Sneak"])
			}
			if byID["Plain"].Concealed || byID["Plain"].AFK {
				t.Fatalf("default flags spuriously set in round-trip: %+v", byID["Plain"])
			}
		})
	}
}

// TestWriteAuthority is the P8-A4 security test: a shard cannot write or evict a player a DIFFERENT live
// shard owns.
func TestWriteAuthority(t *testing.T) {
	ctx := context.Background()
	for _, tc := range eachRoster(t) {
		t.Run(tc.name, func(t *testing.T) {
			// shard-a owns Victim.
			if err := tc.r.Set(ctx, "shard-a", []Entry{{PlayerID: "Victim", Name: "Victim"}}, DefaultTTL); err != nil {
				t.Fatal(err)
			}
			// shard-b (a non-host / forger) tries to overwrite Victim's presence and to add a fake player.
			err := tc.r.Set(ctx, "shard-b", []Entry{
				{PlayerID: "Victim", Name: "FakeName"}, // refused: shard-a owns it
				{PlayerID: "Ghost", Name: "Ghost"},     // allowed: unowned
			}, DefaultTTL)
			if !errors.Is(err, ErrNotOwner) {
				t.Fatalf("over-reaching write: want ErrNotOwner, got %v", err)
			}
			// Victim is unchanged (still owned by shard-a, original name); the unowned Ghost applied.
			got, _ := tc.r.List(ctx)
			byID := map[string]Entry{}
			for _, e := range got {
				byID[e.PlayerID] = e
			}
			if v := byID["Victim"]; v.ShardID != "shard-a" || v.Name != "Victim" {
				t.Fatalf("a non-host shard overwrote a real player's presence: %+v", v)
			}
			// shard-b cannot EVICT Victim either (Remove is owner-guarded).
			if err := tc.r.Remove(ctx, "shard-b", "Victim"); err != nil {
				t.Fatal(err)
			}
			got, _ = tc.r.List(ctx)
			found := false
			for _, e := range got {
				if e.PlayerID == "Victim" {
					found = true
				}
			}
			if !found {
				t.Fatal("a non-host shard evicted a real player's presence")
			}
		})
	}
}

// TestEagerRemove is the clean-quit removal: the OWNING shard removes a player immediately (before TTL).
func TestEagerRemove(t *testing.T) {
	ctx := context.Background()
	for _, tc := range eachRoster(t) {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.r.Set(ctx, "shard-a", []Entry{{PlayerID: "Gone", Name: "Gone"}}, DefaultTTL); err != nil {
				t.Fatal(err)
			}
			if err := tc.r.Remove(ctx, "shard-a", "Gone"); err != nil {
				t.Fatal(err)
			}
			got, _ := tc.r.List(ctx)
			for _, e := range got {
				if e.PlayerID == "Gone" {
					t.Fatal("a cleanly-quit player was not removed immediately")
				}
			}
		})
	}
}

// TestCrashedShardAgeOut is the P8-A4 self-healing demonstration: a shard that stops heartbeating leaves
// its players in the roster only until their TTL lapses, then they age out of `who` with NO explicit
// cleanup.
func TestCrashedShardAgeOut(t *testing.T) {
	ctx := context.Background()
	for _, tc := range eachRoster(t) {
		t.Run(tc.name, func(t *testing.T) {
			ttl := 5 * time.Second
			if err := tc.r.Set(ctx, "shard-doomed", []Entry{{PlayerID: "Doomed", Name: "Doomed"}}, ttl); err != nil {
				t.Fatal(err)
			}
			if got, _ := tc.r.List(ctx); len(got) != 1 {
				t.Fatalf("player should be present before TTL: %+v", got)
			}
			// The shard "crashes": it never refreshes. Advance past the TTL.
			tc.ff(ttl + time.Second)
			got, _ := tc.r.List(ctx)
			if len(got) != 0 {
				t.Fatalf("crashed shard's player did not age out of who: %+v", got)
			}
		})
	}
}

// TestHeartbeatRefreshKeepsLive proves a LIVE shard's batched refresh keeps a player from ever flickering
// out: refreshing within the TTL resets it, so the entry survives well past the original expiry.
func TestHeartbeatRefreshKeepsLive(t *testing.T) {
	ctx := context.Background()
	for _, tc := range eachRoster(t) {
		t.Run(tc.name, func(t *testing.T) {
			ttl := 10 * time.Second
			entries := []Entry{{PlayerID: "Live", Name: "Live"}}
			if err := tc.r.Set(ctx, "shard-a", entries, ttl); err != nil {
				t.Fatal(err)
			}
			// Three heartbeats, each well within the TTL: the player must remain present throughout.
			for i := 0; i < 3; i++ {
				tc.ff(ttl / 2)
				if err := tc.r.Set(ctx, "shard-a", entries, ttl); err != nil {
					t.Fatal(err)
				}
				if got, _ := tc.r.List(ctx); len(got) != 1 {
					t.Fatalf("live player flickered out of who across a heartbeat (beat %d): %+v", i, got)
				}
			}
		})
	}
}
