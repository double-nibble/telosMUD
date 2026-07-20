package world

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// scopeversionwire_test.go — #355 Part B, THE WIRING. scopeversion_test.go pins applyScopeDelta's fence and
// caslost_test.go pins the director's producer stamp, but both are END tests: one hand-builds a
// scopeDeltaMsg, the other reads a StatePayload off the bus. Nothing between them was bound, and the seam
// that joins them — onScopeEvent decoding StatePayload.Version into scopeDeltaMsg.version — is a single
// struct-literal field. Deleting `version: p.Version` there leaves EVERY delta unversioned, which silently
// reverts the whole fix (each push then looks like version 0 => apply-and-do-not-record), and the entire
// world package still passes. Both ends tested and the wire between them not is this repo's recurring defect.
//
// So this drives the real path: a scopebus publish over a MemBus, the shard's OWN subscription (start()),
// the zone inbox, the zone actor's DISPATCH (Zone.handle), applyScopeDelta, and finally the replica the Lua
// surface reads.
//
// The zone actor is deliberately NOT started — the test goroutine drains the inbox and runs Zone.handle
// itself, exactly as drainScopeMessages does in instance_exclusions_test.go. That keeps a single owner of the
// replica (so the assertions are race-free) and keeps the delivery deterministic (a MemBus subscription has
// one ordered delivery goroutine, so the published order IS the inbox order) while still covering every link
// the change touched.

// applyScopeDeltasFromBus pulls every message the shard's subscription posted to z and applies it through the zone's
// own dispatch, returning once the inbox has been quiet for settleWindow.
func applyScopeDeltasFromBus(t *testing.T, z *Zone, settleWindow time.Duration) int {
	t.Helper()
	applied := 0
	for {
		select {
		case m := <-z.inbox:
			z.handle(m)
			if _, isDelta := m.(scopeDeltaMsg); isDelta {
				applied++
			}
		case <-time.After(settleWindow):
			return applied
		}
	}
}

func TestStateDeltaVersionSurvivesTheBusToTheReplica(t *testing.T) {
	lc, err := content.LoadDemoPack()
	require.NoError(t, err)

	mb := commbus.NewMemBus()
	t.Cleanup(func() { _ = mb.Close() })
	bus := scopebus.New(mb)

	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil).
		WithScopeBus(bus, lc.Regions)
	sh.scopes.start()
	t.Cleanup(sh.scopes.stop)

	z := sh.zones["midgaard"]
	require.NotNil(t, z)
	require.Equal(t, "heartlands", z.scopes.regionID, "precondition: midgaard is a heartlands member")

	ctx := context.Background()
	publish := func(scope scopebus.Scope, key, value string, version uint64) {
		body, merr := json.Marshal(scopebus.StatePayload{
			Key: key, Value: json.RawMessage(value), Version: version,
		})
		require.NoError(t, merr)
		require.NoError(t, bus.Signal(ctx, scope, scopebus.EventStateSet, body, "world-director-1"))
	}

	// WORLD: v6, then a REORDERED v5. REGION: the same, so both branches of the wiring are bound.
	publish(scopebus.World(), "war", `"ended"`, 6)
	publish(scopebus.World(), "war", `"active"`, 5)
	publish(scopebus.Region("heartlands"), "siege", `"lifted"`, 6)
	publish(scopebus.Region("heartlands"), "siege", `"ongoing"`, 5)

	require.Equal(t, 4, applyScopeDeltasFromBus(t, z, 500*time.Millisecond),
		"all four deltas must have been ROUTED to the zone; a routing failure would make every assertion "+
			"below vacuously true")

	assert.JSONEq(t, `"ended"`, string(z.scopes.world["war"]),
		"the stale world push won: StatePayload.Version is not reaching applyScopeDelta — the fence is "+
			"inert on the only path production actually uses")
	assert.Equal(t, uint64(6), z.scopes.worldVer["war"],
		"the delta must RECORD the version it arrived with; a recorded 0 means the version was lost in transit")
	assert.JSONEq(t, `"lifted"`, string(z.scopes.region["siege"]),
		"the stale region push won: the region branch drops the version")
	assert.Equal(t, uint64(6), z.scopes.regionVer["siege"])

	// The counterweight: a genuinely newer push over the same wire must still apply. Without it this test
	// would pass just as well against a fence that dropped everything.
	publish(scopebus.World(), "war", `"resumed"`, 7)
	require.Equal(t, 1, applyScopeDeltasFromBus(t, z, 500*time.Millisecond))
	assert.JSONEq(t, `"resumed"`, string(z.scopes.world["war"]),
		"a newer push must still apply — a fence that drops good pushes is worse than no fence")
}

// TestApplyScopeSeedResetsOnlyItsOwnScopeFence closes a gap in
// TestApplyScopeSeedResetsTheVersionFence, which applies BOTH seeds and then asserts both fences are empty.
// That assertion is satisfied just as well by a branch that resets the WRONG map — swap the two assignments
// in applyScopeSeed and every existing test still passes, because after both seeds both maps are empty
// either way.
//
// The swap is not cosmetic. Under it a WORLD seed leaves worldVer untouched, so a replica carrying a
// recorded world version through a re-seed keeps it — which is precisely the stale-fence freeze the reset
// exists to prevent ("a key frozen until some push happens to exceed a version from the replica's previous
// life"). It is latent today only because every current seed lands on a freshly built replica; it goes live
// the moment anything re-seeds a running zone.
//
// So: seed ONE scope, and assert the OTHER scope's fence SURVIVED.
func TestApplyScopeSeedResetsOnlyItsOwnScopeFence(t *testing.T) {
	t.Run("a world seed clears the world fence and leaves the region fence alone", func(t *testing.T) {
		z := replicaZone("heartlands")
		z.applyScopeDelta(delta("world", "war", `"active"`, 9))
		z.applyScopeDelta(delta("region", "siege", `"active"`, 9))

		z.applyScopeSeed(scopeSeedMsg{kind: "world", state: map[string]json.RawMessage{"war": json.RawMessage(`"seeded"`)}})

		assert.Empty(t, z.scopes.worldVer, "the world seed must clear the WORLD fence")
		assert.Equal(t, uint64(9), z.scopes.regionVer["siege"],
			"the world seed must NOT clear the region fence — the region values it did not replace are "+
				"still described by their recorded versions, and dropping them re-opens the stale-push window")
	})

	t.Run("a region seed clears the region fence and leaves the world fence alone", func(t *testing.T) {
		z := replicaZone("heartlands")
		z.applyScopeDelta(delta("world", "war", `"active"`, 9))
		z.applyScopeDelta(delta("region", "siege", `"active"`, 9))

		z.applyScopeSeed(scopeSeedMsg{kind: "region", state: map[string]json.RawMessage{"siege": json.RawMessage(`"seeded"`)}})

		assert.Empty(t, z.scopes.regionVer, "the region seed must clear the REGION fence")
		assert.Equal(t, uint64(9), z.scopes.worldVer["war"],
			"the region seed must NOT clear the world fence")
	})

	t.Run("a region seed on a region-less zone touches no fence at all", func(t *testing.T) {
		z := replicaZone("") // no region
		z.applyScopeDelta(delta("world", "war", `"active"`, 9))

		z.applyScopeSeed(scopeSeedMsg{kind: "region", state: map[string]json.RawMessage{"siege": json.RawMessage(`"x"`)}})

		assert.Equal(t, uint64(9), z.scopes.worldVer["war"],
			"an IGNORED region seed must not clear the world fence on its way out")
	})
}
