package placement

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memClaimer is an in-memory ZoneClaimer: a zone is owned by the FIRST shard to claim it (the time-fenced
// CAS, modelled without time — a second claimer of an owned zone loses). A claim of an already-owned zone
// by the SAME shard renews (ok). failOn forces an error for a named zone (the never-fatal path).
type memClaimer struct {
	owner  map[string]string
	failOn map[string]bool
}

func newMemClaimer() *memClaimer {
	return &memClaimer{owner: map[string]string{}, failOn: map[string]bool{}}
}

func (m *memClaimer) ClaimZone(_ context.Context, zone, shard string, _ time.Duration) (bool, error) {
	if m.failOn[zone] {
		return false, errors.New("claim error")
	}
	if cur, ok := m.owner[zone]; ok {
		return cur == shard, nil // already owned: ok only if WE own it (renew)
	}
	m.owner[zone] = shard
	return true, nil
}

func TestClaimFromPoolFirstServerWinsAll(t *testing.T) {
	c := newMemClaimer()
	pool := []string{"midgaard", "darkwood", "crypt"}
	won, errs := ClaimFromPool(context.Background(), c, "shard-a", pool, time.Second)
	require.Nil(t, errs)
	assert.ElementsMatch(t, pool, won, "the first server claims the whole pool")
}

func TestClaimFromPoolSecondServerIsStandbyThenSplits(t *testing.T) {
	c := newMemClaimer()
	pool := []string{"midgaard", "darkwood", "crypt"}

	// Server A boots first and wins everything.
	wonA, _ := ClaimFromPool(context.Background(), c, "shard-a", pool, time.Second)
	require.Len(t, wonA, 3)

	// Server B boots into a SATURATED fleet: it wins nothing (a standby).
	wonB, _ := ClaimFromPool(context.Background(), c, "shard-b", pool, time.Second)
	assert.Empty(t, wonB, "a server that wins no zone is a standby")

	// midgaard's owner (A) dies: its lease frees up (modelled by clearing the owner). B re-claims it.
	delete(c.owner, "midgaard")
	wonB2, _ := ClaimFromPool(context.Background(), c, "shard-b", pool, time.Second)
	assert.Equal(t, []string{"midgaard"}, wonB2, "the standby re-claims an orphaned zone (decentralized failover)")
}

func TestClaimFromPoolSkipsClaimErrors(t *testing.T) {
	c := newMemClaimer()
	c.failOn["darkwood"] = true
	pool := []string{"midgaard", "darkwood", "crypt"}
	won, errs := ClaimFromPool(context.Background(), c, "shard-a", pool, time.Second)
	assert.ElementsMatch(t, []string{"midgaard", "crypt"}, won, "a claim error skips the zone, not the boot")
	require.Contains(t, errs, "darkwood")
}

// --- Plan (the coordinator decision engine) ----------------------------------------------------

func movesString(moves []Move) []string {
	out := make([]string, 0, len(moves))
	for _, m := range moves {
		from := m.From
		if from == "" {
			from = "<unclaimed>"
		}
		out = append(out, m.Zone+":"+from+"->"+m.To)
	}
	sort.Strings(out)
	return out
}

func TestPlanAssignsUnclaimedZones(t *testing.T) {
	pool := []string{"midgaard", "darkwood", "crypt"}
	// Nothing claimed yet, one live shard: it should be assigned everything.
	moves := Plan([]string{"shard-a"}, map[string]string{}, pool)
	assert.ElementsMatch(t,
		[]string{"crypt:<unclaimed>->shard-a", "darkwood:<unclaimed>->shard-a", "midgaard:<unclaimed>->shard-a"},
		movesString(moves))
}

func TestPlanSpreadsUnclaimedEvenly(t *testing.T) {
	pool := []string{"z1", "z2", "z3", "z4"}
	moves := Plan([]string{"shard-a", "shard-b"}, map[string]string{}, pool)
	// 4 unclaimed zones across 2 idle shards => 2 each.
	perShard := map[string]int{}
	for _, m := range moves {
		require.Equal(t, "", m.From, "an unclaimed assignment has no source")
		perShard[m.To]++
	}
	assert.Equal(t, 2, perShard["shard-a"])
	assert.Equal(t, 2, perShard["shard-b"])
}

func TestPlanRebalancesBusyShardToNewcomer(t *testing.T) {
	pool := []string{"z1", "z2", "z3", "z4"}
	// shard-a owns all 4; shard-b just joined (empty). Rebalance should drain 2 off a to b (or 1, until
	// the gap is within the threshold of 1 — from 4/0, move until 3/1? no: gap 4>1 -> move -> 3/1 gap 2>1
	// -> move -> 2/2 gap 0). So two drains a->b.
	assignment := map[string]string{"z1": "shard-a", "z2": "shard-a", "z3": "shard-a", "z4": "shard-a"}
	moves := Plan([]string{"shard-a", "shard-b"}, assignment, pool)
	require.Len(t, moves, 2, "drain until balanced (4/0 -> 2/2)")
	for _, m := range moves {
		assert.Equal(t, "shard-a", m.From, "a rebalance drains FROM the busy shard")
		assert.Equal(t, "shard-b", m.To)
	}
}

func TestPlanStableWhenBalanced(t *testing.T) {
	pool := []string{"z1", "z2", "z3", "z4"}
	assignment := map[string]string{"z1": "shard-a", "z2": "shard-a", "z3": "shard-b", "z4": "shard-b"}
	moves := Plan([]string{"shard-a", "shard-b"}, assignment, pool)
	assert.Empty(t, moves, "an already-even spread (2/2) is left alone — hysteresis, no thrash")
}

func TestPlanReclaimsDeadShardsZones(t *testing.T) {
	pool := []string{"z1", "z2", "z3"}
	// shard-c owned z3 but is NOT in the live set (it crashed): z3 is unclaimed and must be reassigned.
	assignment := map[string]string{"z1": "shard-a", "z2": "shard-b", "z3": "shard-c"}
	moves := Plan([]string{"shard-a", "shard-b"}, assignment, pool)
	require.Len(t, moves, 1)
	assert.Equal(t, "z3", moves[0].Zone)
	assert.Equal(t, "", moves[0].From, "a dead shard's zone is reassigned as unclaimed (failover), not drained")
	assert.Contains(t, []string{"shard-a", "shard-b"}, moves[0].To)
}

func TestPlanNoLiveShards(t *testing.T) {
	assert.Nil(t, Plan(nil, map[string]string{}, []string{"z1"}), "nothing can be hosted with no live shards")
}

// --- Observe (the coordinator's input gathering) -----------------------------------------------

type fakeFleet struct {
	live   []string
	owners map[string]string // zone -> owner (absent => unclaimed)
}

func (f fakeFleet) ListShards(context.Context) ([]string, error) { return f.live, nil }
func (f fakeFleet) ShardForZone(_ context.Context, zone string) (string, bool, error) {
	o, ok := f.owners[zone]
	return o, ok, nil
}

func TestObserveAndPlanReassignOrphans(t *testing.T) {
	pool := []string{"z1", "z2", "z3"}
	// z3's owner shard-c is NOT live (it crashed); z1/z2 are owned by live shards.
	fleet := fakeFleet{
		live:   []string{"shard-a", "shard-b"},
		owners: map[string]string{"z1": "shard-a", "z2": "shard-b", "z3": "shard-c"},
	}
	live, assignment, err := Observe(context.Background(), fleet, pool)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"shard-a", "shard-b"}, live)
	assert.Equal(t, "shard-c", assignment["z3"], "Observe records the (dead) owner; Plan judges liveness")

	moves := Plan(live, assignment, pool)
	require.Len(t, moves, 1, "the dead shard's zone is the only one to reassign")
	assert.Equal(t, "z3", moves[0].Zone)
	assert.Equal(t, "", moves[0].From)
}
