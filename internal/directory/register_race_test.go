package directory

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// register_race_test.go — CONCURRENCY coverage for the single-writer registration
// guards. The existing tests (TestZoneClaimExclusiveAndRelease, TestShardIdConflict)
// exercise the claimZone/registerShard CAS scripts SEQUENTIALLY — one claim, then a
// second — which interleaves deterministically and never actually races the guard.
//
// The failure mode this file targets is the one debugged in the wild: two world
// servers booting at the same instant and BOTH trying to register the same zone /
// shard id concurrently. The Lua CAS (redis.go:99 claimZone / :216 registerShard) is
// the load-bearing guard, and its whole job is to elect exactly one winner under
// contention. These tests fire N real goroutines through a start barrier so they all
// hit the guard as close to simultaneously as the scheduler allows, then assert
// exactly-one-winner. A regression that split the atomic script into a TOCTOU
// check-then-set would let two racers both win here.

// releaseConcurrently runs fn(i) in n goroutines, holding them all at a start barrier
// so they contend as simultaneously as possible, and returns after every goroutine
// has finished. Each goroutine writes only its own results slot, so the shared slices
// are race-free without a mutex on the hot path.
func releaseConcurrently(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start // park until every goroutine is spawned, then race
			fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
}

func TestRegisterZoneRaceElectsExactlyOneOwner(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()
	const n = 24

	shardIDs := make([]string, n)
	errs := make([]error, n)
	for i := range shardIDs {
		shardIDs[i] = "shard-" + string(rune('a'+i%26)) + itoa(i)
	}

	// N distinct shards all try to RegisterZone the SAME zone at once.
	releaseConcurrently(n, func(i int) {
		errs[i] = d.RegisterZone(ctx, "midgaard", shardIDs[i])
	})

	// Exactly one racer wins; every other gets ErrZoneClaimed — never a silent
	// second success (which would mean two shards both believe they host midgaard).
	winners := 0
	winner := ""
	for i, err := range errs {
		if err == nil {
			winners++
			winner = shardIDs[i]
			continue
		}
		assert.ErrorIsf(t, err, ErrZoneClaimed, "loser %d: want ErrZoneClaimed, got %v", i, err)
	}
	require.Equal(t, 1, winners, "exactly one shard must win the zone claim race")

	// The directory resolves the zone to the winner, and the winner is stable: a
	// loser still cannot claim it, while the winner renews its own lease.
	got, err := d.ShardForZone(ctx, "midgaard")
	require.NoError(t, err)
	require.Equal(t, winner, got, "ShardForZone must resolve to the race winner")

	loser := firstOtherThan(shardIDs, winner)
	require.Error(t, d.RegisterZone(ctx, "midgaard", loser), "a loser must still be refused after the race")
	require.NoError(t, d.RegisterZone(ctx, "midgaard", winner), "the winner must be able to renew its own lease")
}

func TestRegisterShardRaceElectsExactlyOneEndpoint(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()
	const n = 24

	// N distinct processes all boot with the SAME shard id but different endpoints
	// (the duplicated-shard-id boot race). Exactly one registration must win; the
	// rest get ErrShardConflict, so a dup id can never silently become two writers.
	endpoints := make([]string, n)
	errs := make([]error, n)
	for i := range endpoints {
		endpoints[i] = "world-" + itoa(i) + ":9090"
	}

	releaseConcurrently(n, func(i int) {
		errs[i] = d.RegisterShard(ctx, "shard-a", endpoints[i], DefaultShardLease)
	})

	winners := 0
	winner := ""
	for i, err := range errs {
		if err == nil {
			winners++
			winner = endpoints[i]
			continue
		}
		assert.ErrorIsf(t, err, ErrShardConflict, "loser %d: want ErrShardConflict, got %v", i, err)
	}
	require.Equal(t, 1, winners, "exactly one process must win the shard-id registration race")

	got, err := d.EndpointForShard(ctx, "shard-a")
	require.NoError(t, err)
	require.Equal(t, winner, got, "EndpointForShard must resolve to the race winner's endpoint")
}

// TestRegisterZoneConcurrentDistinctZonesAllSucceed is the negative control: one shard
// registering MANY DIFFERENT zones concurrently must have every claim succeed. It guards
// against a regression where the guard over-serializes or keys on the wrong thing and
// falsely rejects an unrelated concurrent claim (a distinct-key claim is never a conflict).
func TestRegisterZoneConcurrentDistinctZonesAllSucceed(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()
	const n = 24

	zones := make([]string, n)
	errs := make([]error, n)
	for i := range zones {
		zones[i] = "zone-" + itoa(i)
	}

	releaseConcurrently(n, func(i int) {
		errs[i] = d.RegisterZone(ctx, zones[i], "shard-a")
	})

	for i, err := range errs {
		require.NoErrorf(t, err, "distinct zone %q claim must succeed", zones[i])
	}
	for _, z := range zones {
		got, err := d.ShardForZone(ctx, z)
		require.NoError(t, err)
		require.Equal(t, "shard-a", got)
	}
}

// itoa is a tiny dependency-free int->string for building unique test ids (avoids
// pulling strconv into the hot barrier path for readability only).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func firstOtherThan(ids []string, exclude string) string {
	for _, id := range ids {
		if id != exclude {
			return id
		}
	}
	return ""
}
