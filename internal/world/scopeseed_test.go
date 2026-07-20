package world

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// fakeScopeSnapshot is an in-memory ScopeSnapshotSource for the #44 seed tests.
type fakeScopeSnapshot struct {
	world  map[string][]byte
	region map[string]map[string][]byte
}

func (f fakeScopeSnapshot) SnapshotWorldState(context.Context) (map[string]ScopeValue, error) {
	return asScopeValues(f.world), nil
}

func (f fakeScopeSnapshot) SnapshotRegionState(_ context.Context, regionID string) (map[string]ScopeValue, error) {
	return asScopeValues(f.region[regionID]), nil
}

// TestApplyScopeSeed pins the seed apply: a full snapshot replaces the zone's replica for its scope (dropping
// null values), and a region seed for a region-less zone is ignored.
func TestApplyScopeSeed(t *testing.T) {
	z := newZone("midgaard")
	z.scopes.regionID = "heartlands"

	z.applyScopeSeed(scopeSeedMsg{kind: "world", state: map[string]json.RawMessage{
		"invasion_active": json.RawMessage(`true`),
		"phase":           json.RawMessage(`3`),
		"cleared":         json.RawMessage(`null`), // a null value is dropped, not stored
	}})
	z.applyScopeSeed(scopeSeedMsg{kind: "region", state: map[string]json.RawMessage{
		"mood": json.RawMessage(`"tense"`),
	}})

	if string(z.scopes.world["invasion_active"]) != "true" || string(z.scopes.world["phase"]) != "3" {
		t.Fatalf("world replica not seeded: %v", z.scopes.world)
	}
	if _, ok := z.scopes.world["cleared"]; ok {
		t.Fatal("a null seed value must be dropped, not stored")
	}
	if string(z.scopes.region["mood"]) != `"tense"` {
		t.Fatalf("region replica not seeded: %v", z.scopes.region)
	}

	// A region-less zone ignores a region seed.
	z2 := newZone("crypt")
	z2.applyScopeSeed(scopeSeedMsg{kind: "region", state: map[string]json.RawMessage{"x": json.RawMessage(`1`)}})
	if len(z2.scopes.region) != 0 {
		t.Fatalf("a region-less zone accepted a region seed: %v", z2.scopes.region)
	}
}

// TestSeedFromSnapshotSeedsHostedZone is the #44 integration: with a scope snapshot source wired, the shard's
// seedFromSnapshot posts a world + region seed to a hosted member zone, so after processing them the zone's
// replica reflects the authoritative store state it would have MISSED had it been down for the broadcast.
func TestSeedFromSnapshotSeedsHostedZone(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	mb := commbus.NewMemBus()
	t.Cleanup(func() { _ = mb.Close() })

	fake := fakeScopeSnapshot{
		world:  map[string][]byte{"invasion_active": []byte("true")},
		region: map[string]map[string][]byte{"heartlands": {"mood": []byte(`"tense"`)}},
	}
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil).
		WithScopeBus(scopebus.New(mb), lc.Regions).
		WithScopeSnapshot(fake)

	z := sh.zoneByID("midgaard")
	if z.scopes.regionID != "heartlands" {
		t.Fatalf("midgaard regionID = %q, want heartlands (region membership not stamped)", z.scopes.regionID)
	}

	sh.scopes.seedFromSnapshot() // posts scopeSeedMsg(s) to the zone inbox (before any live subscription)

	// Drain + apply the posted seeds (the zone is not Run here; this is what its loop would do).
	for {
		select {
		case m := <-z.inbox:
			if s, ok := m.(scopeSeedMsg); ok {
				z.applyScopeSeed(s)
			}
			continue
		default:
		}
		break
	}

	if string(z.scopes.world["invasion_active"]) != "true" {
		t.Fatalf("world state not seeded on join: %v", z.scopes.world)
	}
	if string(z.scopes.region["mood"]) != `"tense"` {
		t.Fatalf("region state not seeded on join: %v", z.scopes.region)
	}
}

// asScopeValues wraps a plain key->bytes fixture as the versioned snapshot shape the store returns
// (#355). Versions start at 1 and ascend by insertion-independent key order, which is enough for the
// seed-fence tests: what matters is that the versions are non-zero and distinct, not their exact values.
func asScopeValues(in map[string][]byte) map[string]ScopeValue {
	out := make(map[string]ScopeValue, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		out[k] = ScopeValue{Value: in[k], Version: uint64(i + 1)}
	}
	return out
}
