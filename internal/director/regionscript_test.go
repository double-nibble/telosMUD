package director

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// regionscript_test.go — #356: a REGION director runs a content-defined Lua script, held to exactly the
// same reserved-name guards as the world script, with its writes landing in its own region's state.
//
// The guards themselves needed no code change — they were already scope-agnostic — which is precisely why
// they need tests here. A property that holds by accident and a property that holds by design look
// identical until someone edits the code, and "the world path is exercised so the region path must be
// fine" is the shape of a vacuous test.

// regionDirector builds a leader region director with the given script wired.
func regionDirector(t *testing.T, regionID, script string) (*Director, *memScopeStore) {
	t.Helper()
	st := newMemStore()
	d := New(regionID, st, discardLog())
	d.leader.Store(true)
	d.WithRegionScript(script)
	return d, st
}

// TestRegionScriptCannotForgeReservedDownEvents pins the guard at region scope. Two of these are live
// vectors there, not formalities: the zone-side dispatch switches on the event NAME with no scope-kind
// check, so a forged scope.state.set writes the region replica in every member zone (bypassing the region
// director's CAS), and a forged content.pull.result prints a fabricated operator advisory to a builder.
func TestRegionScriptCannotForgeReservedDownEvents(t *testing.T) {
	events := []struct {
		event string
		why   string
	}{
		{scopebus.EventStateSet, "forging a state delta bypasses the region director's single-writer CAS"},
		{"content.pull.result", "reaches deliverPullResult and prints a forged operator advisory to a builder"},
		{"content.reload.audit", "forges an entry in the fleet content-change audit record"},
		{SpawnBossEvent, "vacuous at region scope today, but reserved so the namespace is not asymmetric"},
		{BossDiedEvent, "same — reservation asymmetry is a worse trap than the lost capability"},
	}
	for _, tc := range events {
		t.Run(tc.event, func(t *testing.T) {
			// The script records whether the broadcast RAISED, so the assertion reads the guard's effect
			// rather than a log line. pcall is deliberate: it proves the refusal is a real Lua error the
			// script observes, not a silent no-op.
			d, _ := regionDirector(t, "heartlands", `
				function on_signal(event, payload)
					local ok = pcall(function() director.broadcast(event) end)
					director.set("refused", not ok)
				end
			`)
			d.handleSignal(context.Background(), signal(tc.event, 1, `{}`))
			r := d.get(context.Background(), "refused")
			require.True(t, r.found, "the script must have run and recorded an outcome")
			assert.JSONEqf(t, `true`, string(r.value),
				"director.broadcast(%q) must be refused at REGION scope: %s", tc.event, tc.why)
		})
	}
}

// TestRegionScriptCannotWriteReservedScopeKey pins reservedScopeKey at region scope. Vacuous today (no
// region director runs schedules) and kept for the same uniformity reason.
func TestRegionScriptCannotWriteReservedScopeKey(t *testing.T) {
	d, st := regionDirector(t, "heartlands", `
		function on_signal(event, payload)
			director.set("schedule:gorlak", {active = true})
		end
	`)
	d.handleSignal(context.Background(), signal("anything", 1, `{}`))

	_, _, found, err := st.LoadRegionState(context.Background(), "heartlands", "schedule:gorlak")
	require.NoError(t, err)
	assert.False(t, found, "a region script must not be able to write an engine-reserved key")
}

// TestRegionScriptWritesItsOwnRegionState is the test that proves region scope stopped being write-dead.
// Before #356 nothing consumed the region subject and nothing ever wrote region state, so region:get in
// content could only read rows no code path produced.
func TestRegionScriptWritesItsOwnRegionState(t *testing.T) {
	d, st := regionDirector(t, "heartlands", `
		function on_signal(event, payload)
			if event == "region_boss_slain" then director.set("last_boss", payload.boss) end
		end
	`)
	d.handleSignal(context.Background(), signal("region_boss_slain", 1, `{"boss":"vurgoth"}`))

	got, _, found, err := st.LoadRegionState(context.Background(), "heartlands", "last_boss")
	require.NoError(t, err)
	require.True(t, found, "the region script's write must land in REGION state")
	assert.JSONEq(t, `"vurgoth"`, string(got))

	// And it must NOT have gone to world state — the scope routing is the whole point.
	_, _, wFound, err := st.LoadWorldState(context.Background(), "last_boss")
	require.NoError(t, err)
	assert.False(t, wFound, "a region director's write must never land in world state")
}

// TestRegionDirectorBroadcastsOnItsOwnScope pins the routing of the DOWN state broadcast: a region
// director's delta must go to telos.scope.region.<ref>, not the world subject.
func TestRegionDirectorBroadcastsOnItsOwnScope(t *testing.T) {
	mb := commbus.NewMemBus()
	bus := scopebus.New(mb)

	regionSeen := make(chan string, 4)
	worldSeen := make(chan string, 4)
	rs, err := bus.Subscribe(scopebus.Region("heartlands"), func(event string, _ json.RawMessage, _ string) {
		regionSeen <- event
	})
	require.NoError(t, err)
	defer func() { _ = rs.Unsubscribe() }()
	ws, err := bus.Subscribe(scopebus.World(), func(event string, _ json.RawMessage, _ string) {
		worldSeen <- event
	})
	require.NoError(t, err)
	defer func() { _ = ws.Unsubscribe() }()

	st := newMemStore()
	d := New("heartlands", st, discardLog()).WithScopeBus(bus, "director-1")
	d.leader.Store(true)
	d.WithRegionScript(`function on_signal(e, p) director.set("mood", "tense") end`)
	d.handleSignal(context.Background(), signal("anything", 1, `{}`))

	// The transient bus delivers on its own goroutine, so WAIT for the positive rather than polling once —
	// a bare non-blocking read here would pass for the wrong reason (nothing delivered yet) and would keep
	// passing if the broadcast were removed entirely.
	select {
	case ev := <-regionSeen:
		assert.Equal(t, scopebus.EventStateSet, ev, "the region's own scope must carry the state delta")
	case <-time.After(2 * time.Second):
		t.Fatal("the region director's state delta was not broadcast on its region scope")
	}
	// Only now is the negative meaningful: the region delivery proves the bus has drained this publish, so
	// an empty world channel means nothing was ever sent there rather than "not yet".
	select {
	case ev := <-worldSeen:
		t.Fatalf("a region director must not broadcast on the WORLD scope; got %q", ev)
	default:
	}
}

// TestTwoDirectorScriptsAreIsolated pins that N VMs in one process share nothing. A global set in one
// script must not be visible in another, and each script's director table must reach only its own scope.
func TestTwoDirectorScriptsAreIsolated(t *testing.T) {
	a, aStore := regionDirector(t, "heartlands", `
		shared = "from-a"
		function on_signal(e, p) director.set("who", "a") end
	`)
	b, bStore := regionDirector(t, "duskwall", `
		function on_signal(e, p) director.set("leaked", shared == nil and "no" or shared) end
	`)

	a.handleSignal(context.Background(), signal("x", 1, `{}`))
	b.handleSignal(context.Background(), signal("x", 1, `{}`))

	leaked, _, found, err := bStore.LoadRegionState(context.Background(), "duskwall", "leaked")
	require.NoError(t, err)
	require.True(t, found)
	assert.JSONEq(t, `"no"`, string(leaked),
		"a global defined in one director's VM must not be visible in another's — the VMs share no Lua state")

	// Each write landed in its own region only.
	_, _, found, err = aStore.LoadRegionState(context.Background(), "heartlands", "who")
	require.NoError(t, err)
	assert.True(t, found)
	_, _, found, err = aStore.LoadRegionState(context.Background(), "duskwall", "who")
	require.NoError(t, err)
	assert.False(t, found, "director A's write must not reach region duskwall")
}

// TestSetLuaCapsAfterFirstVMIsRefused pins the freeze latch. With one world script the "set the caps
// before compiling" rule was enforced by a doc comment; with N region VMs built in a loop it is a rule
// that can be broken silently, and the symptom would be some scopes honouring the operator's caps and
// others running engine defaults. A boot error is strictly better than a silent split.
func TestSetLuaCapsAfterFirstVMIsRefused(t *testing.T) {
	restoreLuaCaps(t)

	_, err := newLuaDirector(nil, worldScriptKey, `function on_signal() end`)
	require.NoError(t, err, "precondition: a VM compiles")

	err = SetLuaCaps(200000, 50)
	require.Error(t, err, "SetLuaCaps must be refused once a VM has already read the caps")
	assert.Contains(t, err.Error(), "after a script VM was already built")
}

// TestDirectorVMsAreReclaimedWithoutClose pins the finding that #356's stated blocker does not exist, so
// nobody re-files it. The issue asserted that a director losing leadership leaks its gopher-lua VM. It
// does not, for two independent reasons: a Director is never destroyed on a leadership change (the same
// long-lived object campaigns, loses, and campaigns again — losing the lease flips an atomic bool and
// tears down the durable consumer, nothing more), and an un-Closed Runtime holds no goroutine, OS handle
// or timer, so dropping it is fully reclaimable.
//
// Adding the proposed teardown hook would have been worse than the non-problem it targeted:
// Runtime.Close nils the LState, CallGlobal dereferences it without a nil check, and OnSignal has no
// recover — so a late signal after teardown would crash the director process.
func TestDirectorVMsAreReclaimedWithoutClose(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		ld, err := newLuaDirector(nil, worldScriptKey, `function on_signal(e, p) local x = 1 + 1 end`)
		require.NoError(t, err)
		ld.OnSignal(&API{d: New("", newMemStore(), discardLog()), ctx: context.Background()}, "x", nil)
		// Deliberately NOT closed — that is the whole point.
	}
	runtime.GC()
	after := runtime.NumGoroutine()
	assert.LessOrEqual(t, after, before+2,
		"50 un-Closed director VMs must not accumulate goroutines: an LState holds no goroutine, so the "+
			"'leaked VM' this issue was filed for is GC-able memory, not a resource leak")
}

// TestLeadershipLossKeepsTheScriptVM is the regression net for the false premise, stated as behavior: a
// director demoted to standby must KEEP its handler and resume orchestrating when it wins the lease back.
// Anyone who re-adds teardown-on-resign breaks this.
func TestLeadershipLossKeepsTheScriptVM(t *testing.T) {
	d, st := regionDirector(t, "heartlands", `
		function on_signal(e, p) director.set("ran", e) end
	`)
	require.NotNil(t, d.handler, "precondition: the script wired a handler")

	d.leader.Store(false) // demoted to warm standby
	assert.NotNil(t, d.handler, "a demoted director must keep its script handler — it is a standby, not a corpse")

	d.leader.Store(true) // wins the lease back
	d.handleSignal(context.Background(), signal("after_repromotion", 1, `{}`))

	got, _, found, err := st.LoadRegionState(context.Background(), "heartlands", "ran")
	require.NoError(t, err)
	require.True(t, found, "a re-promoted director must still be able to run its script")
	assert.JSONEq(t, `"after_repromotion"`, string(got))
}

// TestRegionScriptFailureNamesItsRegion pins the reason the script key is parameterized at all. It is
// only a log/chunk/breaker label — scope routing comes from the Director's regionID, not from the key —
// so nothing about correctness changes if it is wrong. What changes is that with N region directors in
// one process, every region's failure would be logged as "world_script" and an operator could not tell
// which region tripped. That is the whole deliverable, so it gets an assertion rather than a comment.
func TestRegionScriptFailureNamesItsRegion(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	d := New("heartlands", newMemStore(), log)
	d.leader.Store(true)
	d.WithRegionScript(`function on_signal(e, p) error("boom") end`)
	d.handleSignal(context.Background(), signal("anything", 1, `{}`))

	out := buf.String()
	assert.Contains(t, out, "region_script:heartlands",
		"a region script's failure must name its REGION; without this every region logs as world_script and "+
			"an operator cannot tell which one tripped")
	assert.NotContains(t, out, `script=world_script`,
		"a region script must never be logged under the world script's identity")
}

// TestRegionScriptBreakerTripNamesItsRegion is the second half of the identity assertion, and the half that
// covers the ONE runtime consumer of ld.key that is not a log attribute: the circuit-breaker key passed to
// CallGlobal.
//
// A single failing signal cannot see it. ld.log already carries script=<key> as a logger ATTRIBUTE, so the
// per-call warning names the region no matter what key the breaker is fed — reverting OnSignal to
// CallGlobal(worldScriptKey, ...) leaves the one-failure test green. It takes TRIPPING the breaker, whose
// own log line carries the key it was actually keyed on, to tell the two apart. And that key matters beyond
// the label: LoadGlobals records under ld.key, so a mismatched OnSignal key splits one script's failure
// accounting across two budgets and the load-time disable latch stops gating calls.
//
// It also pins the behavior itself at region scope for the first time: a chronically failing region script
// is QUARANTINED, and the region director survives it.
func TestRegionScriptBreakerTripNamesItsRegion(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	d := New("heartlands", newMemStore(), log)
	d.leader.Store(true)
	d.WithRegionScript(`
		function on_signal(e, p)
			director.set("ran", e)
			error("a deterministic content bug")
		end
	`)

	// A logic error costs 1.0 against a 10.0 budget, so 10 failing signals trip it exactly.
	for i := 0; i < 12; i++ {
		d.handleSignal(context.Background(), signal("boom", uint64(i+1), `{}`))
	}

	out := buf.String()
	require.Contains(t, out, "circuit breaker TRIPPED",
		"a chronically failing region script must be quarantined — the breaker is the reason one bad region "+
			"cannot degrade the tier")
	assert.Contains(t, out, "script=region_script:heartlands",
		"the breaker must be keyed on the REGION's script identity; keyed on world_script it both mislabels "+
			"the alert and splits this script's failure budget away from the one LoadGlobals feeds")
	assert.NotContains(t, out, "world_script",
		"nothing about a region script may be attributed to the world script")
}

// TestWithRegionScriptRefusesTheWorldDirector pins the structural half of the empty-ref BLOCKER. The cmd
// loop skips a ref-less region, but a skip protects only the caller that remembers it. director.New("")
// IS the world director, so a "region script" on one would run against world state under the world's
// lease and the world's durable consumer — and every reserved-name guard in this package would be beside
// the point, because the script simply IS the world script. The reachable cause is mundane: one
// region_defs entry with `ref` omitted, which the ref-charset lint skips by design.
func TestWithRegionScriptRefusesTheWorldDirector(t *testing.T) {
	d := New("", newMemStore(), discardLog())
	d.leader.Store(true)
	d.WithRegionScript(`function on_signal(e, p) director.set("owned", true) end`)

	assert.Nil(t, d.handler,
		"a region script must never be wired onto the world director — that is a region with an empty ref, "+
			"and it would silently become a second world script")
}

// TestConsumerIDIsInjectiveOverRegionRefs pins the durable-consumer collision. Both "." and ":" are legal
// in a region ref, and the old name built by substituting each to "-" was not injective: two distinct
// regions — distinct subjects, distinct leases — collapsed onto one consumer name. Consume calls
// CreateOrUpdateConsumer, so the second director would repoint the shared consumer's filter subject at
// itself and both would pull from it, each leadership flip flipping it back.
func TestConsumerIDIsInjectiveOverRegionRefs(t *testing.T) {
	refs := []string{"heart:lands", "heart-lands", "heart.lands", "heartlands", "heart_lands"}
	seen := map[string]string{}
	for _, ref := range refs {
		id := New(ref, newMemStore(), discardLog()).consumerID()
		if prev, dup := seen[id]; dup {
			t.Fatalf("region refs %q and %q collapse to the same durable consumer %q — one region's "+
				"director would consume the other's signal stream", prev, ref, id)
		}
		seen[id] = ref
		// Real JetStream rejects these in a consumer name, which is what the lossy substitution was for.
		assert.NotContains(t, id, ".", "a consumer name must not contain a dot")
		assert.NotContains(t, id, ":", "a consumer name must not contain a colon")
		assert.NotEqual(t, "director-world", id, "a region must never share the world's consumer name")
	}
}

// TestConsumerIDIsStableAcrossRestarts pins the other half of the contract the hash must not break: the
// name is STABLE per scope, so a restart resumes from the last ack rather than replaying the stream.
func TestConsumerIDIsStableAcrossRestarts(t *testing.T) {
	a := New("heartlands", newMemStore(), discardLog()).consumerID()
	b := New("heartlands", newMemStore(), discardLog()).consumerID()
	assert.Equal(t, a, b, "the same region must always produce the same durable consumer name")
}
