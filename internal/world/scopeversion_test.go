package world

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopeversion_test.go — #355 Part B: a zone's scope read-replica fences a duplicated or reordered
// director state push on the STORE-assigned version, so it can never be left holding a superseded value.
//
// Why this matters more than "the bus reorders": single-publisher delivery over the transient tier is
// strictly ordered end-to-end, and that was confirmed rather than assumed. The reachable corruption is
// two directors publishing inside the lease-observation-lag window. What makes it stick is that
// world.flag / region:get read the LOCAL replica only — there is no read-through to the director — so a
// wrong replica value survives until the next write of that key or a full reseed.

// replicaZone builds a bare zone with an initialised scope replica, optionally in a region.
func replicaZone(regionID string) *Zone {
	z := &Zone{scopes: newScopeReplica()}
	z.scopes.regionID = regionID
	return z
}

func delta(kind, key, value string, version uint64) scopeDeltaMsg {
	var raw json.RawMessage
	if value != "" {
		raw = json.RawMessage(value)
	}
	return scopeDeltaMsg{kind: kind, key: key, value: raw, version: version}
}

func TestApplyScopeDeltaVersionFence(t *testing.T) {
	tests := []struct {
		name    string
		pushes  []scopeDeltaMsg
		wantVal string // "" => the key must be absent
		why     string
	}{
		{
			name:    "cold replica applies any version",
			pushes:  []scopeDeltaMsg{delta("world", "war", `"active"`, 7)},
			wantVal: `"active"`,
			why: "a replica with no recorded version MUST apply — a seeded replica has values but no " +
				"versions, and rejecting the unknown case would freeze every zone at its seed forever",
		},
		{
			name:    "a newer version applies",
			pushes:  []scopeDeltaMsg{delta("world", "war", `"active"`, 5), delta("world", "war", `"ended"`, 6)},
			wantVal: `"ended"`,
			why:     "the fence must not drop good pushes; that is worse than having no fence",
		},
		{
			name:    "a reordered older version is dropped",
			pushes:  []scopeDeltaMsg{delta("world", "war", `"ended"`, 6), delta("world", "war", `"active"`, 5)},
			wantVal: `"ended"`,
			why:     "the stale push must not win — this is the corruption the fence exists for",
		},
		{
			name:    "a duplicate of the same version is dropped",
			pushes:  []scopeDeltaMsg{delta("world", "war", `"ended"`, 6), delta("world", "war", `"active"`, 6)},
			wantVal: `"ended"`,
			why:     "equal versions describe the same write, so a second one is a duplicate, not an update",
		},
		{
			name:    "an unversioned push applies over a recorded version",
			pushes:  []scopeDeltaMsg{delta("world", "war", `"active"`, 9), delta("world", "war", `"ended"`, 0)},
			wantVal: `"ended"`,
			why: "version 0 means UNVERSIONED (an un-upgraded director mid-rolling-deploy), not oldest — " +
				"reading it as oldest would reject every push from that director",
		},
		{
			name:    "an unversioned push does not record a version",
			pushes:  []scopeDeltaMsg{delta("world", "war", `"a"`, 0), delta("world", "war", `"b"`, 1)},
			wantVal: `"b"`,
			why:     "an unversioned push must apply-and-not-advance, or it would fence out the next real version",
		},
		{
			name:    "a newer delete removes the key",
			pushes:  []scopeDeltaMsg{delta("world", "war", `"active"`, 5), delta("world", "war", "", 6)},
			wantVal: "",
			why:     "a delete is an ordinary versioned write (the store keeps the row and bumps the version)",
		},
		{
			name:    "a reordered older delete is dropped",
			pushes:  []scopeDeltaMsg{delta("world", "war", `"active"`, 6), delta("world", "war", "", 5)},
			wantVal: `"active"`,
			why:     "a stale delete must not remove a value written after it",
		},
		{
			name:    "a reordered older set cannot resurrect a deleted key",
			pushes:  []scopeDeltaMsg{delta("world", "war", `"x"`, 5), delta("world", "war", "", 7), delta("world", "war", `"active"`, 6)},
			wantVal: "",
			why: "the DELETE must record its version too — if it did not, the deleted key would have no " +
				"recorded version and the older set would be applied as a cold-replica push",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := replicaZone("")
			for _, p := range tc.pushes {
				z.applyScopeDelta(p)
			}
			got, ok := z.scopes.world["war"]
			if tc.wantVal == "" {
				assert.False(t, ok, "key must be absent: %s", tc.why)
				return
			}
			require.True(t, ok, "key must be present: %s", tc.why)
			assert.JSONEq(t, tc.wantVal, string(got), tc.why)
		})
	}
}

// TestApplyScopeDeltaFenceIsPerKey pins that one key's version never gates another's. A single shared
// high-water would drop every push to a rarely-written key once a busy key ran ahead of it.
func TestApplyScopeDeltaFenceIsPerKey(t *testing.T) {
	z := replicaZone("")
	z.applyScopeDelta(delta("world", "busy", `"v20"`, 20))
	z.applyScopeDelta(delta("world", "quiet", `"v2"`, 2))

	got, ok := z.scopes.world["quiet"]
	require.True(t, ok, "a low-versioned key must not be fenced by an unrelated high-versioned one")
	assert.JSONEq(t, `"v2"`, string(got))
}

// TestApplyScopeDeltaFenceIsPerScope pins that the world and region fences are independent. Sharing one
// map would let a world key's version drop a region key of the same name.
func TestApplyScopeDeltaFenceIsPerScope(t *testing.T) {
	z := replicaZone("heartlands")
	z.applyScopeDelta(delta("world", "war", `"world-v9"`, 9))
	z.applyScopeDelta(delta("region", "war", `"region-v1"`, 1))

	got, ok := z.scopes.region["war"]
	require.True(t, ok, "a region key must not be fenced by the world key of the same name")
	assert.JSONEq(t, `"region-v1"`, string(got))
}

// TestApplyScopeSeedResetsTheVersionFence pins the trap most likely to be gotten wrong. The snapshot
// carries no versions, so any version recorded before it describes a value the seed just overwrote.
// Keeping those versions would fence out valid later pushes and freeze those keys.
func TestApplyScopeSeedResetsTheVersionFence(t *testing.T) {
	z := replicaZone("heartlands")
	z.applyScopeDelta(delta("world", "war", `"active"`, 9))
	z.applyScopeDelta(delta("region", "siege", `"active"`, 9))
	require.Equal(t, uint64(9), z.scopes.worldVer["war"], "precondition: a version was recorded")

	z.applyScopeSeed(scopeSeedMsg{kind: "world", state: map[string]json.RawMessage{"war": json.RawMessage(`"seeded"`)}})
	z.applyScopeSeed(scopeSeedMsg{kind: "region", state: map[string]json.RawMessage{"siege": json.RawMessage(`"seeded"`)}})

	assert.Empty(t, z.scopes.worldVer, "the world seed must clear the version fence it invalidated")
	assert.Empty(t, z.scopes.regionVer, "the region seed must clear its version fence too")

	// A post-seed push at a LOWER version than the pre-seed one must now apply.
	z.applyScopeDelta(delta("world", "war", `"post-seed"`, 3))
	z.applyScopeDelta(delta("region", "siege", `"post-seed"`, 3))
	assert.JSONEq(t, `"post-seed"`, string(z.scopes.world["war"]),
		"a stale fence surviving the seed would freeze this key until a push exceeded version 9")
	assert.JSONEq(t, `"post-seed"`, string(z.scopes.region["siege"]))
}

// TestApplyScopeDeltaDropsNoGoodPush is the counterweight to every drop assertion above: a fence that
// silently drops valid pushes is worse than no fence at all, and every test in this file except this one
// would still pass if the fence dropped everything.
func TestApplyScopeDeltaDropsNoGoodPush(t *testing.T) {
	z := replicaZone("")
	const n = 200
	for i := 1; i <= n; i++ {
		z.applyScopeDelta(delta("world", "counter", `"v`+strconv.Itoa(i)+`"`, uint64(i)))
	}
	got, ok := z.scopes.world["counter"]
	require.True(t, ok)
	assert.JSONEq(t, `"v`+strconv.Itoa(n)+`"`, string(got),
		"200 strictly-increasing in-order pushes must ALL apply — the last value proves none were dropped")
	assert.Equal(t, uint64(n), z.scopes.worldVer["counter"])
}

// TestApplyScopeSeedCarriesTheVersionFence pins the other half of the seed rule. The seed must RESET the
// fence (a version from the replica's previous life describes a value the seed just overwrote) but it
// must then re-seed it from the snapshot's own row versions — otherwise the fence is empty at exactly the
// boot / drain-adoption moment it exists to cover, and a duplicated or reordered delta arriving during a
// failover would be applied unfenced.
func TestApplyScopeSeedCarriesTheVersionFence(t *testing.T) {
	z := replicaZone("heartlands")
	z.applyScopeDelta(delta("world", "war", `"stale-life"`, 40)) // a version from before the seed

	z.applyScopeSeed(scopeSeedMsg{
		kind:     "world",
		state:    map[string]json.RawMessage{"war": json.RawMessage(`"seeded"`)},
		versions: map[string]uint64{"war": 9},
	})

	assert.Equal(t, uint64(9), z.scopes.worldVer["war"],
		"the seed must install the snapshot's row version, not leave the fence empty and not keep the old 40")

	// A delta at or below the seeded version is stale relative to the snapshot and must be fenced out.
	z.applyScopeDelta(delta("world", "war", `"older"`, 9))
	assert.JSONEq(t, `"seeded"`, string(z.scopes.world["war"]),
		"a delta at the seeded version is the write the snapshot already reflects — it must not re-apply")

	// A newer delta still applies: the fence must not freeze the key.
	z.applyScopeDelta(delta("world", "war", `"newer"`, 10))
	assert.JSONEq(t, `"newer"`, string(z.scopes.world["war"]))
}

// TestApplyScopeSeedWithoutVersionsLeavesTheFenceOpen pins the SAFE degradation. A snapshot source that
// cannot supply versions (an older store, a test double) must leave the fence unset for those keys —
// unfenced means "apply the next delta", never "freeze this key". Freezing on a missing version would
// turn a degraded snapshot into permanently stale world state.
func TestApplyScopeSeedWithoutVersionsLeavesTheFenceOpen(t *testing.T) {
	z := replicaZone("")
	z.applyScopeSeed(scopeSeedMsg{
		kind:  "world",
		state: map[string]json.RawMessage{"war": json.RawMessage(`"seeded"`)},
	})
	assert.Empty(t, z.scopes.worldVer, "no versions supplied => no fence recorded")

	z.applyScopeDelta(delta("world", "war", `"delta"`, 1))
	assert.JSONEq(t, `"delta"`, string(z.scopes.world["war"]),
		"an unfenced seeded key must accept its next delta, whatever the version")
}
