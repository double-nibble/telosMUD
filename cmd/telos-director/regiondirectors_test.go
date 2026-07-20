package main

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/director"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// regiondirectors_test.go — the net for the #356 cmd WIRING, which was the one part of that change with no
// test at all. Everything else it touched (the guards, the scope routing, the body round-trip) is unit
// tested inside its own package; the loop that decides WHICH directors exist, what scope each owns and
// which lease each campaigns for was reachable only by reading main().
//
// That gap mattered because the wiring carries the change's sharpest failure mode: director.New("") is the
// WORLD director, so anything that lets an empty ref through the loop mints a second world director rather
// than a region one.

// fakeScopeStore is an inert director.ScopeStore — these tests exercise construction and election, never
// scope state.
type fakeScopeStore struct{}

func (fakeScopeStore) LoadWorldState(context.Context, string) ([]byte, uint64, bool, error) {
	return nil, 0, false, nil
}

func (fakeScopeStore) SaveWorldState(context.Context, string, []byte, uint64) (uint64, bool, error) {
	return 1, true, nil
}

func (fakeScopeStore) LoadRegionState(context.Context, string, string) ([]byte, uint64, bool, error) {
	return nil, 0, false, nil
}

func (fakeScopeStore) SaveRegionState(context.Context, string, string, []byte, uint64) (uint64, bool, error) {
	return 1, true, nil
}

// recordingClaimer records every lease id campaigned for, and enforces the SAME rule the real Redis script
// does: a claim succeeds when the lease is free, expired, or ALREADY THIS OWNER'S. That last clause is the
// one that makes an empty-ref region dangerous, so a fake that arbitrated purely on the key would hide the
// very thing this test exists to catch.
type recordingClaimer struct {
	mu     sync.Mutex
	owners map[string]string
	claims []string
}

func newRecordingClaimer() *recordingClaimer {
	return &recordingClaimer{owners: map[string]string{}}
}

func (c *recordingClaimer) ClaimLease(_ context.Context, leaseID, owner string, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims = append(c.claims, leaseID)
	if cur, held := c.owners[leaseID]; held && cur != owner {
		return false, nil
	}
	c.owners[leaseID] = owner
	return true, nil
}

func (c *recordingClaimer) ReleaseLease(_ context.Context, leaseID, owner string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owners[leaseID] == owner {
		delete(c.owners, leaseID)
	}
	return nil
}

// claimed returns the DISTINCT lease ids seen, sorted.
func (c *recordingClaimer) claimed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, id := range c.claims {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// runBriefly runs each director just long enough to campaign once, then cancels and waits. Campaigning is
// the only externally observable proof of which SCOPE a director was built for — leaseID is unexported, but
// the claimer sees it.
func runBriefly(t *testing.T, ds []*director.Director) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, d := range ds {
		wg.Add(1)
		go func(d *director.Director) {
			defer wg.Done()
			d.Run(ctx)
		}(d)
	}
	// Every director campaigns synchronously at the top of Run, before its first tick.
	require.Eventually(t, func() bool {
		for _, d := range ds {
			if !d.IsLeader() {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "every built director should have campaigned")
	cancel()
	wg.Wait()
}

func testBus() *scopebus.Bus { return scopebus.New(commbus.NewMemBus()) }

// TestBuildRegionDirectorsOwnsOneRegionScopeEach pins the wiring's core claim: one director per region,
// each campaigning for its OWN region lease. If the loop ever passed "" (or any other ref) to director.New,
// the claimed lease set changes and this fails — the mutation that would otherwise produce phantom world
// directors with no test noticing.
func TestBuildRegionDirectorsOwnsOneRegionScopeEach(t *testing.T) {
	c := newRecordingClaimer()
	ds := buildRegionDirectors([]content.RegionDTO{
		{Ref: "heartlands", Name: "The Heartlands", Zones: []string{"midgaard"}},
		{Ref: "duskwall", Name: "Duskwall"},
	}, fakeScopeStore{}, testBus(), c, "director-1", discardLog())

	require.Len(t, ds, 2, "one director per region_defs entry")
	runBriefly(t, ds)

	assert.Equal(t, []string{"director:region:duskwall", "director:region:heartlands"}, c.claimed(),
		"each region director must campaign for its OWN region-scoped lease; a world lease here means the "+
			"loop built a WORLD director for a region entry")
	assert.NotContains(t, c.claimed(), "director:world",
		"a region director must never contend for the world director's lease")
}

// TestBuildRegionDirectorsSkipsAnEmptyRef is the guard for the hazard the region loop introduced.
// director.New("") IS the world director, so an unref'd region_defs entry would put a SECOND world director
// in this process: same lease id, same instance id, same durable consumer name. ClaimLease succeeds for an
// owner that already holds the lease, so the CAS cannot arbitrate them and BOTH believe they lead — two
// writers on world scope, and one durable consumer name split across two subscriptions.
//
// The ref-charset lint does not catch it (it skips empty tokens by design) and the loader accepts a region
// with no ref, so this sink is where it has to be caught.
func TestBuildRegionDirectorsSkipsAnEmptyRef(t *testing.T) {
	c := newRecordingClaimer()
	ds := buildRegionDirectors([]content.RegionDTO{
		{Ref: "", Name: "The Heartlands", Zones: []string{"midgaard"}}, // ref omitted in YAML
		{Ref: "duskwall", Name: "Duskwall"},
	}, fakeScopeStore{}, testBus(), c, "director-1", discardLog())

	require.Len(t, ds, 1, "the ref-less region must not produce a director")
	runBriefly(t, ds)

	assert.Equal(t, []string{"director:region:duskwall"}, c.claimed(),
		"an empty region ref must never mint a second WORLD director (director:world) in this process")
}

// TestBuildRegionDirectorsWiresEachRegionsOwnScript pins that r.Script actually reaches the director it
// belongs to, under that region's key. Without this, dropping WithRegionScript from the loop entirely — or
// wiring every region the same script — leaves every other test in this change still green, because they
// all construct their directors directly.
func TestBuildRegionDirectorsWiresEachRegionsOwnScript(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ds := buildRegionDirectors([]content.RegionDTO{
		{Ref: "heartlands", Script: `function on_signal(e, p) director.set("x", 1) end`},
		{Ref: "duskwall", Script: `function on_signal( -- unclosed`},
		{Ref: "quietvale"}, // no script: still gets a director, just no orchestration
	}, fakeScopeStore{}, testBus(), nil, "director-1", log)

	require.Len(t, ds, 3, "a region with no script still gets a director — it owns that region's state")

	out := buf.String()
	assert.Contains(t, out, "script=region_script:heartlands",
		"the region's own script must be compiled under its own region key")
	assert.Contains(t, out, "script=region_script:duskwall",
		"a BROKEN region script must be reported against the region that owns it")
	assert.Contains(t, out, "director script compile failed",
		"a broken region script must be logged, not swallowed")
	assert.NotContains(t, out, "script=region_script:quietvale",
		"a region with no script must not compile one")
	assert.NotContains(t, out, "world_script",
		"no region may be wired under the world script's identity")
}

// TestOneBadRegionScriptDoesNotStopTheTier pins the fail-open contract at the level that matters now. With
// one world script a compile failure cost that one scope; with N regions, treating it as fatal would let a
// single typo in one region's Lua take the whole orchestration tier — and every other region's — down.
func TestOneBadRegionScriptDoesNotStopTheTier(t *testing.T) {
	c := newRecordingClaimer()
	ds := buildRegionDirectors([]content.RegionDTO{
		{Ref: "broken", Script: `function on_signal( -- unclosed`},
		{Ref: "healthy", Script: `function on_signal(e, p) end`},
	}, fakeScopeStore{}, testBus(), c, "director-1", discardLog())

	require.Len(t, ds, 2, "a region whose script failed to compile still owns its scope")
	runBriefly(t, ds)
	assert.Equal(t, []string{"director:region:broken", "director:region:healthy"}, c.claimed(),
		"a broken script must not cost its region the scope, nor any other region its director")
}

// TestDemoFixtureRegionsBuildDirectors closes the loop from CONTENT to WIRING: the shipped fixture's region
// is loaded by the real loader and fed to the real builder. It is what proves the demo region script is not
// merely valid YAML — it compiles in the sandbox and the region it belongs to gets a director that owns the
// matching scope.
func TestDemoFixtureRegionsBuildDirectors(t *testing.T) {
	lc, err := content.Load(context.Background(), content.EmbeddedSource{}, []string{content.DemoPack})
	require.NoError(t, err)
	require.NotEmpty(t, lc.Regions, "the demo fixture defines at least one region")

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c := newRecordingClaimer()
	ds := buildRegionDirectors(lc.Regions, fakeScopeStore{}, testBus(), c, "director-1", log)

	require.Len(t, ds, len(lc.Regions))
	runBriefly(t, ds)

	for _, r := range lc.Regions {
		assert.Contains(t, c.claimed(), "director:region:"+r.Ref)
		if r.Script != "" {
			assert.Contains(t, buf.String(), "script=region_script:"+r.Ref,
				"the fixture's region script must COMPILE — a compile failure logs the same key, so also assert "+
					"the tier never reported one")
		}
	}
	assert.NotContains(t, buf.String(), "compile failed",
		"no shipped region script may fail to compile")
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError + 4}))
}
