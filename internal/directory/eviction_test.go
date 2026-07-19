package directory

import (
	"context"
	"testing"
)

// eviction_test.go — #340: the directory-is-not-a-cache startup check.

// TestClassifyEvictionFlagsEvictingPoliciesWithACeiling is the core decision, and the table encodes the one
// thing the policy alone cannot tell you: eviction needs a POLICY that discards keys AND a CEILING for it to
// discard them at.
//
// Both policy families count. `allkeys-*` can take the PERSISTed zone hash carrying the lease generation.
// `volatile-*` was allowed by #340's original conclusion — correct for that one key, wrong for the
// directory as a whole, because the TTL'd keys each turn a single-writer guard OFF when evicted: the
// director's leader-election lease (two directors both believe they lead), the shard registration (two
// processes sharing a shard id both register), and the instance-template claim, which carries the shortest
// TTL in the directory and is therefore what `volatile-ttl` takes first.
func TestClassifyEvictionFlagsEvictingPoliciesWithACeiling(t *testing.T) {
	const gb = int64(1 << 30)
	for _, tc := range []struct {
		name       string
		policy     string
		maxMemory  int64
		wantUnsafe bool
	}{
		{"allkeys-lru with a ceiling evicts the zone lease generation", "allkeys-lru", gb, true},
		{"allkeys-lfu with a ceiling", "allkeys-lfu", gb, true},
		{"allkeys-random with a ceiling", "allkeys-random", gb, true},
		{"volatile-lru takes the TTL'd single-writer guards", "volatile-lru", gb, true},
		{"volatile-lfu with a ceiling", "volatile-lfu", gb, true},
		{"volatile-random with a ceiling", "volatile-random", gb, true},
		{"volatile-ttl takes the shortest-TTL key first", "volatile-ttl", gb, true},
		{"case is not to be relied on", "ALLKEYS-LRU", gb, true},

		{"noeviction with a ceiling is the correct configuration", "noeviction", gb, false},
		{"noeviction with no ceiling", "noeviction", 0, false},

		// The false-positive class that would have made this check a fleet outage. With no ceiling, eviction
		// CANNOT occur however the policy is set — the invariant is not violated now, and will not be until
		// somebody sets a limit. Reporting it as an active hazard would cry wolf on the most common shape of
		// the misconfiguration (a policy set in a parameter group or chart values, with no limit anywhere).
		{"an evicting policy without a ceiling cannot actually evict", "allkeys-lru", 0, false},
		{"volatile without a ceiling cannot actually evict", "volatile-ttl", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyEviction(tc.policy, tc.maxMemory)
			if got.Unsafe != tc.wantUnsafe {
				t.Fatalf("classifyEviction(%q, %d).Unsafe = %v, want %v",
					tc.policy, tc.maxMemory, got.Unsafe, tc.wantUnsafe)
			}
		})
	}
}

// TestClassifyEvictionTreatsAnEmptyPolicyAsUnknown. A server that answers without saying anything leaves us
// in the same state of knowledge as a failed read, so it must not be recorded as a pass — otherwise a probe
// that silently returns nothing looks identical to a verified-safe directory.
func TestClassifyEvictionTreatsAnEmptyPolicyAsUnknown(t *testing.T) {
	got := classifyEviction("   ", 1<<30)
	if !got.Unknown {
		t.Fatal("an empty policy must be reported as Unknown, not silently accepted")
	}
	if got.Unsafe {
		t.Fatal("an unreadable policy must not be reported as an active hazard either")
	}
}

// TestCheckEvictionPolicyDegradesWhenTheConfigCannotBeRead is the deliberate asymmetry, and it is the
// difference between a check that ships and one that gets ripped out.
//
// `CONFIG GET` is restricted on ElastiCache and blocked on Memorystore. The check tries INFO memory as a
// fallback for exactly that reason — ElastiCache DEFAULTS to `volatile-lru`, so a CONFIG-only probe would be
// blind on the platform whose default is unsafe. When BOTH are unavailable the result is Unknown, the
// operator is told to verify by hand, and startup continues.
func TestCheckEvictionPolicyDegradesWhenNothingCanBeRead(t *testing.T) {
	d, mr := newTestRedisWithClock(t)
	mr.Close() // neither CONFIG GET nor INFO is answerable now

	got := d.CheckEvictionPolicy(context.Background())
	if !got.Unknown {
		t.Fatalf("an unreadable configuration must be Unknown, got %+v", got)
	}
	if got.Unsafe {
		t.Fatal("an unreadable configuration must never be reported as an active hazard — a check that " +
			"cannot answer must not manufacture one")
	}
}

// TestInfoFieldParsesAMaxmemoryPolicyLine pins the INFO fallback's parsing against the real INFO shape,
// including the CRLF line endings Redis actually emits and a prefix-colliding neighbour field.
func TestInfoFieldParsesAMaxmemoryPolicyLine(t *testing.T) {
	const info = "# Memory\r\n" +
		"used_memory:1032216\r\n" +
		"maxmemory:2147483648\r\n" +
		"maxmemory_human:2.00G\r\n" +
		"maxmemory_policy:volatile-lru\r\n" +
		"mem_fragmentation_ratio:1.03\r\n"

	if got := infoField(info, "maxmemory_policy"); got != "volatile-lru" {
		t.Fatalf("maxmemory_policy = %q, want volatile-lru", got)
	}
	// `maxmemory` must not match `maxmemory_human` or `maxmemory_policy` — the fields share a prefix, and
	// picking the wrong one would parse "2.00G" or a policy name as the ceiling and silently yield 0.
	if got := infoField(info, "maxmemory"); got != "2147483648" {
		t.Fatalf("maxmemory = %q, want 2147483648", got)
	}
	if got := infoField(info, "not_present"); got != "" {
		t.Fatalf("a missing field = %q, want empty", got)
	}
}
