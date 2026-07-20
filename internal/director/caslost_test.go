package director

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// caslost_test.go — #354: a signal whose scope-state write LOST the optimistic CAS must NAK for
// redelivery to the live leader, not ack-and-drop the consequence fleet-wide. Plus the two defects that
// finding uncovered: a cold version cache faking a CAS loss on every restart, and a boss reschedule that
// logged success for a write that never landed.
//
// Every test here is negative-powered — each was confirmed to FAIL against the pre-fix tree.

// leaderDirector builds a director that is already the leader, as Run would for a claimer-less director.
// handleSignal is driven directly in these tests, so the leader store has to be explicit.
func leaderDirector(st ScopeStore) *Director {
	d := New("", st, discardLog())
	d.leader.Store(true)
	return d
}

// signal builds a fresh, dedup-distinct signalMsg.
func signal(event string, seq uint64, payload string) signalMsg {
	return signalMsg{
		event:   event,
		payload: json.RawMessage(payload),
		seq:     seq,
		seqOK:   true,
		source:  "shard-1",
		ack:     make(chan bool, 1),
	}
}

// TestSetSeedsVersionOnCacheMiss pins the cold-cache defect. A director that RESTARTED has an empty
// version cache, so a blind write to a pre-existing key CASed on version 0 and was rejected by the store
// — with no concurrent writer anywhere. That is the exact derived-write pattern the world_script
// idempotency contract recommends, so every restart silently dropped the first write to each key.
//
// This is also the precondition for the NAK below: until ErrCASLost meant "concurrent writer" rather
// than "cold cache", NAKing on it would have requeued a signal on every ordinary restart.
func TestSetSeedsVersionOnCacheMiss(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	_, ok, err := st.SaveWorldState(ctx, "last_boss", []byte(`"gorlak"`), 0)
	require.NoError(t, err)
	require.True(t, ok, "seed write must land")

	// A fresh director — a restart — blind-writes the key without ever reading it.
	d := leaderDirector(st)
	requireSetOK(ctx, t, d, "last_boss", json.RawMessage(`"vexis"`),
		"a blind write from a restarted director must not report a CAS loss: there is no other writer")

	got, ver, found, err := st.LoadWorldState(ctx, "last_boss")
	require.NoError(t, err)
	require.True(t, found)
	assert.JSONEq(t, `"vexis"`, string(got), "the write must actually land, not be silently dropped")
	assert.Equal(t, uint64(2), ver, "the CAS must have bumped the seeded version, not re-inserted")
}

// TestHandleSignalNAKsOnCASLoss is the issue's core claim. A genuine concurrent writer moves the key
// between this director's cached version and its write; the signal must NAK so the live leader re-applies.
func TestHandleSignalNAKsOnCASLoss(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)
	d.WithSignalHandler(func(api *API, _ string, _ json.RawMessage) {
		_ = api.Set("phase", json.RawMessage(`2`))
	})

	// Prime this director's cache at version 1, then let a CONCURRENT writer move the row to version 2.
	requireSetOK(ctx, t, d, "phase", json.RawMessage(`1`))
	_, ok, err := st.SaveWorldState(ctx, "phase", []byte(`99`), 1)
	require.NoError(t, err)
	require.True(t, ok, "the concurrent write must land, or there is no CAS loss to observe")

	m := signal("boss_slain", 7, `{}`)
	d.handleSignal(ctx, m)

	assert.False(t, <-m.ack, "a signal whose write lost the CAS must NAK, not ack-and-drop it fleet-wide")
	assert.Zero(t, d.applied[m.source],
		"a NAK'd signal must NOT advance the per-source high-water — advancing it would suppress the "+
			"redelivery and lose the event anyway, which is the whole defect")
}

// TestNAKedSignalConvergesOnRedelivery answers the sharpest objection to NAKing on a CAS loss: that the
// retry would simply force through the value the CAS just rejected, turning a working fence into a
// clobber. It does not. d.set reloads the winning value before returning, so the redelivery re-RUNS the
// handler, which re-READS fresh state and recomputes — a read-modify-write retry, not a force-write.
func TestNAKedSignalConvergesOnRedelivery(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)
	// A handler that DERIVES its write from current state, so a re-run against fresh state is observable.
	d.WithSignalHandler(func(api *API, _ string, _ json.RawMessage) {
		cur, _ := api.Get("kills")
		var n int
		if len(cur) > 0 {
			_ = json.Unmarshal(cur, &n)
		}
		_ = api.Set("kills", json.RawMessage(strconv.Itoa(n+1)))
	})

	requireSetOK(ctx, t, d, "kills", json.RawMessage(`0`))
	// A concurrent writer jumps the count to 10 and the version to 2.
	_, ok, err := st.SaveWorldState(ctx, "kills", []byte(`10`), 1)
	require.NoError(t, err)
	require.True(t, ok)

	m1 := signal("boss_slain", 3, `{}`)
	d.handleSignal(ctx, m1)
	require.False(t, <-m1.ack, "first attempt loses the CAS and NAKs")

	// The redelivery: same event, same seq (the high-water was not advanced, so it is not suppressed).
	m2 := signal("boss_slain", 3, `{}`)
	d.handleSignal(ctx, m2)
	assert.True(t, <-m2.ack, "the redelivery must converge and ack")

	got, _, _, err := st.LoadWorldState(ctx, "kills")
	require.NoError(t, err)
	assert.JSONEq(t, `11`, string(got),
		"the retry must recompute from the WINNING value (10+1), never re-push the rejected one (0+1)")
	assert.Equal(t, uint64(3), d.applied[m2.source], "the converged apply advances the high-water")
}

// TestPcallCannotHideCASLoss is the mutation that kills the alternative designs. luaSet turns a CAS loss
// into a Lua error, and a script's pcall SWALLOWS it — so OnSignal returns cleanly and any outcome
// derived from error PROPAGATION (a SignalHandler return value) would report "applied" for a lost write.
// Recording the loss on the Director, at the single point every write funnels through, survives pcall.
func TestPcallCannotHideCASLoss(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)

	// The script swallows the error entirely and returns normally.
	d.WithWorldScript(`
		function on_signal(event, payload)
			pcall(function() director.set("phase", 2) end)
		end
	`)
	require.NotNil(t, d.handler, "the world script must have compiled and wired a handler")

	requireSetOK(ctx, t, d, "phase", json.RawMessage(`1`))
	_, ok, err := st.SaveWorldState(ctx, "phase", []byte(`99`), 1)
	require.NoError(t, err)
	require.True(t, ok)

	m := signal("boss_slain", 4, `{}`)
	d.handleSignal(ctx, m)
	assert.False(t, <-m.ack,
		"a CAS loss must NAK even when the script's pcall swallowed the Lua error — the ack decision "+
			"cannot depend on error propagation through content")
}

// TestCASLossThroughComposedScheduleHandler pins composition transparency. WithSchedules wraps the
// previous handler in a closure that discards returns, so a return-value design would need every wrapper
// to propagate the outcome. A Director-recorded flag needs none.
func TestCASLossThroughComposedScheduleHandler(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)
	d.WithSignalHandler(func(api *API, _ string, _ json.RawMessage) {
		_ = api.Set("phase", json.RawMessage(`2`))
	})
	// Compose the scheduler OUTERMOST, as production does. A non-boss.died event falls through to prev.
	d.WithSchedules([]Schedule{{Ref: "gorlak", Proto: "p", Zone: "z", Interval: time.Hour}})

	requireSetOK(ctx, t, d, "phase", json.RawMessage(`1`))
	_, ok, err := st.SaveWorldState(ctx, "phase", []byte(`99`), 1)
	require.NoError(t, err)
	require.True(t, ok)

	m := signal("boss_slain", 5, `{}`) // not BossDiedEvent, so it reaches the wrapped handler
	d.handleSignal(ctx, m)
	assert.False(t, <-m.ack, "a CAS loss raised through a COMPOSED handler must still NAK")
}

// TestCASLossFlagDoesNotLeakAcrossSignals pins the arming discipline. The flag means "THIS signal's
// application lost a write". A stale flag would NAK an unrelated, perfectly-applied later signal forever.
func TestCASLossFlagDoesNotLeakAcrossSignals(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)
	losing := true
	d.WithSignalHandler(func(api *API, _ string, _ json.RawMessage) {
		if losing {
			_ = api.Set("phase", json.RawMessage(`2`))
		}
	})

	requireSetOK(ctx, t, d, "phase", json.RawMessage(`1`))
	_, ok, err := st.SaveWorldState(ctx, "phase", []byte(`99`), 1)
	require.NoError(t, err)
	require.True(t, ok)

	m1 := signal("boss_slain", 6, `{}`)
	d.handleSignal(ctx, m1)
	require.False(t, <-m1.ack, "signal 1 loses the CAS")

	losing = false // signal 2 writes nothing at all
	m2 := signal("quiet", 7, `{}`)
	d.handleSignal(ctx, m2)
	assert.True(t, <-m2.ack, "a later signal that lost nothing must ack — the flag must not leak")
}

// TestTickPathCASLossIsNotObservedByALaterSignal pins the other leak direction. runSchedules writes
// scope state from the TICK, off any signal. A loss there must not be attributed to the next signal to
// arrive, which would NAK an event that applied perfectly.
func TestTickPathCASLossIsNotObservedByALaterSignal(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)
	d.WithSchedules([]Schedule{{Ref: "gorlak", Proto: "p", Zone: "z", Interval: time.Hour}})
	d.WithSignalHandler(func(_ *API, _ string, _ json.RawMessage) {})

	// Drive a tick-path write, then move the schedule key underneath the director so the NEXT tick write
	// loses its CAS — leaving d.writeFailed set outside any signal window.
	d.runSchedules(ctx)
	key := scheduleKey("gorlak")
	_, ver, found, err := st.LoadWorldState(ctx, key)
	require.NoError(t, err)
	require.True(t, found, "the tick must have persisted schedule state")
	_, ok, err := st.SaveWorldState(ctx, key, []byte(`{"active":true}`), ver)
	require.NoError(t, err)
	require.True(t, ok)
	d.saveScheduleState(ctx, "gorlak", ScheduleState{Active: true}) // loses the CAS, sets the flag

	m := signal("unrelated", 8, `{}`)
	d.handleSignal(ctx, m)
	assert.True(t, <-m.ack,
		"a tick-path CAS loss must not NAK an unrelated later signal — handleSignal arms the flag itself")
}

// TestAlreadyAppliedSignalAcksEvenAfterDemotion pins the gate ORDER: dedup first, leader gate second.
// Inverting them would make a demoted director NAK work it had already done, so the promoted leader would
// re-run the handler and re-fire its down-broadcasts.
func TestAlreadyAppliedSignalAcksEvenAfterDemotion(t *testing.T) {
	d := leaderDirector(newMemStore())
	d.applied["shard-1"] = 9

	d.leader.Store(false) // demoted
	m := signal("boss_slain", 9, `{}`)
	d.handleSignal(context.Background(), m)

	assert.True(t, <-m.ack, "an already-applied signal must ack even after demotion (dedup precedes the gate)")
}

// TestHandleSignalNAKsWhenNotLeader pins the consume-then-demote handoff: a signal delivered while this
// director held the lease but drained after losing it must be requeued for the promoted leader, not
// applied by a director that no longer owns the scope. handlePullRequest already defended this boundary;
// the handler path did not.
func TestHandleSignalNAKsWhenNotLeader(t *testing.T) {
	var ran bool
	d := leaderDirector(newMemStore())
	d.WithSignalHandler(func(_ *API, _ string, _ json.RawMessage) { ran = true })

	d.leader.Store(false)
	m := signal("boss_slain", 2, `{}`)
	d.handleSignal(context.Background(), m)

	assert.False(t, <-m.ack, "a non-leader must NAK so the live leader applies it")
	assert.False(t, ran, "a non-leader must not run the handler at all")
	assert.Zero(t, d.applied[m.source], "a NAK'd signal must not advance the high-water")
}

// TestBossDeathRescheduleDoesNotClaimAFailedWrite pins the lying log. AfterDeath persists Active:true; if
// that write is lost, IsDue is false forever and ApplyMissed skips it on restart — the boss never
// respawns again. The old code logged "rescheduled" for exactly that permanent wedge.
func TestBossDeathRescheduleDoesNotClaimAFailedWrite(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)
	d.WithSchedules([]Schedule{{Ref: "gorlak", Proto: "p", Zone: "z", Interval: time.Hour}})

	// Prime the key in the director's cache, then move it underneath so the reschedule write loses.
	requireSetOK(ctx, t, d, scheduleKey("gorlak"), json.RawMessage(`{"active":true}`))
	_, ver, _, err := st.LoadWorldState(ctx, scheduleKey("gorlak"))
	require.NoError(t, err)
	_, ok, err := st.SaveWorldState(ctx, scheduleKey("gorlak"), []byte(`{"active":true}`), ver)
	require.NoError(t, err)
	require.True(t, ok)

	payload, err := json.Marshal(BossDied{Ref: "gorlak"})
	require.NoError(t, err)
	m := signalMsg{event: BossDiedEvent, payload: payload, seq: 11, seqOK: true, source: "shard-1", ack: make(chan bool, 1)}
	d.handleSignal(ctx, m)

	assert.False(t, <-m.ack,
		"a boss reschedule whose write lost the CAS must NAK — acking it wedges that boss's respawn "+
			"permanently, across restarts")
	assert.Zero(t, d.applied[m.source], "and must not advance the high-water past the lost reschedule")
}

// TestBossDeathRescheduleLogsTheFailureNotSuccess pins the CALLER's use of that seam — the half of the fix
// the ack assertions above cannot see. handleSignal NAKs off the writeFailed flag, which d.set sets regardless
// of what onBossDied does with saveScheduleState's bool; so reverting onBossDied to ignore the outcome
// leaves every ack/high-water assertion in this file green while restoring the lying "rescheduled" log
// verbatim. The log IS the deliverable here: a permanently wedged boss respawn whose only operator-visible
// artifact says it succeeded.
func TestBossDeathRescheduleLogsTheFailureNotSuccess(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	d := captureDirector(&buf)
	st, ok := d.store.(*memScopeStore)
	require.True(t, ok, "captureDirector must build on the in-memory store")
	d.WithSchedules([]Schedule{{Ref: "gorlak", Proto: "p", Zone: "z", Interval: time.Hour}})

	// Prime the key in the director's cache, then move it underneath so the reschedule write loses.
	requireSetOK(ctx, t, d, scheduleKey("gorlak"), json.RawMessage(`{"active":true}`))
	_, ver, _, err := st.LoadWorldState(ctx, scheduleKey("gorlak"))
	require.NoError(t, err)
	_, moved, err := st.SaveWorldState(ctx, scheduleKey("gorlak"), []byte(`{"active":true}`), ver)
	require.NoError(t, err)
	require.True(t, moved)

	payload, err := json.Marshal(BossDied{Ref: "gorlak"})
	require.NoError(t, err)
	d.onBossDied(&API{d: d, ctx: ctx}, payload)

	out := buf.String()
	assert.NotContains(t, out, "scheduled boss died; rescheduled",
		"a reschedule whose write LOST the CAS must not report success — that line is the operator's only "+
			"signal, and it named the permanent wedge a completed reschedule")
	assert.Contains(t, out, "boss death reschedule did NOT persist",
		"the failed reschedule must be reported, so an operator can see the schedule is stuck active")
}

// TestBossDeathRescheduleLogsSuccessWhenItLands is the other side of the same assertion: the failure path
// above must not be reached by simply deleting the success log. A test that only asserts an absence can be
// satisfied by removing the thing entirely.
func TestBossDeathRescheduleLogsSuccessWhenItLands(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	d := captureDirector(&buf)
	d.WithSchedules([]Schedule{{Ref: "gorlak", Proto: "p", Zone: "z", Interval: time.Hour}})

	payload, err := json.Marshal(BossDied{Ref: "gorlak"})
	require.NoError(t, err)
	d.onBossDied(&API{d: d, ctx: ctx}, payload)

	out := buf.String()
	assert.Contains(t, out, "scheduled boss died; rescheduled", "a reschedule that LANDED must report success")
	assert.NotContains(t, out, "did NOT persist")
}

// TestNonLeaderDoesNotRecordTheReloadAudit pins the leader gate's POSITION relative to the director-owned
// side effects below it. The audit is written by handleSignal itself, not the content handler, so a gate
// placed after it would let a demoted director record the audit AND NAK — and the promoted leader records
// it again on redelivery, double-counting a fleet content change in the operational record. The ack-only
// assertions in TestHandleSignalNAKsWhenNotLeader cannot see that: the ack is false either way.
func TestNonLeaderDoesNotRecordTheReloadAudit(t *testing.T) {
	var buf bytes.Buffer
	d := captureDirector(&buf)
	d.leader.Store(false)

	m := auditSignal(t, "shard-1:1", contentbus.ReloadAudit{
		Actor: "Ada", Packs: []string{"demo"}, Published: 7, Outcome: "propagated", AtUnixMs: 1234,
	})
	d.handleSignal(context.Background(), m)

	assert.False(t, <-m.ack, "a non-leader must requeue the audit signal for the live leader")
	assert.NotContains(t, buf.String(), "content reload audit",
		"a non-leader must not record the audit it is about to hand back — the promoted leader records it "+
			"on redelivery, and both recording it double-counts the change")
}

// TestSaveScheduleStateReportsOutcome pins the seam the lying log depended on: callers could not tell a
// landed write from a lost one.
func TestSaveScheduleStateReportsOutcome(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)

	assert.True(t, d.saveScheduleState(ctx, "gorlak", ScheduleState{Active: true}),
		"a write that lands must report true")

	_, ver, _, err := st.LoadWorldState(ctx, scheduleKey("gorlak"))
	require.NoError(t, err)
	_, ok, err := st.SaveWorldState(ctx, scheduleKey("gorlak"), []byte(`{"active":false}`), ver)
	require.NoError(t, err)
	require.True(t, ok)

	assert.False(t, d.saveScheduleState(ctx, "gorlak", ScheduleState{Active: true}),
		"a write that lost the CAS must report false")
}

// errStore wraps a ScopeStore and fails SaveWorldState with a non-CAS error — a Postgres blip, a reset
// connection. It is deliberately NOT a CAS loss: ok=false and err!=nil are different store outcomes and
// they took different branches out of d.set.
type errStore struct {
	ScopeStore
	fail error
}

func (e *errStore) SaveWorldState(ctx context.Context, key string, value []byte, expected uint64) (uint64, bool, error) {
	if e.fail != nil {
		return 0, false, e.fail
	}
	return e.ScopeStore.SaveWorldState(ctx, key, value, expected)
}

// TestStoreErrorNAKsLikeACASLoss pins the half of #354's own defect the issue did not describe. The issue
// framed the fleet-wide lost write around ErrCASLost, which needs a failover race to happen at all. A
// plain store error reaches the identical end state — the write does not land, the signal is acked off the
// SHARED durable consumer, the consequence is gone — by a far more common route. An ack predicate that
// only knew about CAS losses would have fixed the rare half of the bug it was filed for.
func TestStoreErrorNAKsLikeACASLoss(t *testing.T) {
	ctx := context.Background()
	st := &errStore{ScopeStore: newMemStore(), fail: errors.New("postgres is down")}
	d := leaderDirector(st)
	d.WithSignalHandler(func(api *API, _ string, _ json.RawMessage) {
		_ = api.Set("phase", json.RawMessage(`2`))
	})

	m := signal("boss_slain", 12, `{}`)
	d.handleSignal(ctx, m)

	assert.False(t, <-m.ack,
		"a signal whose write failed on a STORE ERROR must NAK — the write is just as lost as on a CAS loss")
	assert.Zero(t, d.applied[m.source], "and must not advance the high-water past the lost write")
}

// TestSetRefusesAWriteFromANonLeader pins the single-writer invariant as a property of the WRITE PATH
// rather than an assumption every caller must remember. handleSignal and onTick both gate on leadership
// before dispatch, but leadership can lapse DURING a handler — after those gates have already passed.
func TestSetRefusesAWriteFromANonLeader(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)
	requireSetOK(ctx, t, d, "phase", json.RawMessage(`1`), "a leader's write must land")

	d.leader.Store(false) // the lease lapsed
	err := setErr(ctx, d, "phase", json.RawMessage(`2`))
	assert.ErrorIs(t, err, ErrNotLeader, "a non-leader must be refused at the write path")

	got, _, _, lerr := st.LoadWorldState(ctx, "phase")
	require.NoError(t, lerr)
	assert.JSONEq(t, `1`, string(got), "and the refused value must NOT have reached the store")
}

// TestMidHandlerDemotionNAKsRatherThanWriting is the composite the two guards exist for: a handler that
// begins as leader and loses the lease part-way through. The write must be refused AND the signal NAK'd,
// so the promoted leader applies it rather than this director half-applying and acking.
func TestMidHandlerDemotionNAKsRatherThanWriting(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	d := leaderDirector(st)
	d.WithSignalHandler(func(api *API, _ string, _ json.RawMessage) {
		d.leader.Store(false) // the lease lapses mid-handler, after handleSignal's gate already passed
		_ = api.Set("phase", json.RawMessage(`2`))
	})

	m := signal("boss_slain", 13, `{}`)
	d.handleSignal(ctx, m)

	assert.False(t, <-m.ack, "a mid-handler demotion must NAK, not ack a half-applied signal")
	_, _, found, err := st.LoadWorldState(ctx, "phase")
	require.NoError(t, err)
	assert.False(t, found, "and the demoted director's write must never have landed")
}

// --- #355 Part B: the DOWN broadcast carries the STORE-assigned version ---------------------------

// TestBroadcastStateDownCarriesTheStoreVersion pins producer/fence agreement. The version is asserted
// against what the STORE's CAS actually returned, never a literal — if the two could drift, the replica
// fence would be silently fencing on the wrong number.
func TestBroadcastStateDownCarriesTheStoreVersion(t *testing.T) {
	ctx := context.Background()
	mb := commbus.NewMemBus()
	st := newMemStore()
	dirBus := scopebus.New(mb)

	var mu sync.Mutex
	var got []scopebus.StatePayload
	sub, err := dirBus.Subscribe(scopebus.World(), func(event string, payload json.RawMessage, _ string) {
		if event != scopebus.EventStateSet {
			return
		}
		var p scopebus.StatePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return
		}
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	d := leaderDirector(st).WithScopeBus(dirBus, "world-director-1")
	api := &API{d: d, ctx: ctx}
	require.NoError(t, api.Set("war", json.RawMessage(`"active"`)))
	require.NoError(t, api.Set("war", json.RawMessage(`"ended"`)))

	_, storeVer, found, err := st.LoadWorldState(ctx, "war")
	require.NoError(t, err)
	require.True(t, found)

	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 },
		time.Second, 5*time.Millisecond, "both state deltas must be broadcast down")
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, storeVer, got[1].Version,
		"the broadcast version must be the version the store's CAS assigned, not a publisher-side counter")
	assert.Greater(t, got[1].Version, got[0].Version, "successive writes must carry increasing versions")
}

// TestBroadcastVersionContinuesAcrossARestart is the failover-monotonicity guard, and it is the test that
// fails loudly if anyone ever swaps the store version for a per-director counter. A promoted leader (here,
// a fresh Director over the same store) must CONTINUE the sequence — a counter restarting at 1 would be
// fenced out by every replica holding a higher version, permanently for a rarely-written key.
func TestBroadcastVersionContinuesAcrossARestart(t *testing.T) {
	ctx := context.Background()
	mb := commbus.NewMemBus()
	st := newMemStore()

	var mu sync.Mutex
	var versions []uint64
	sub, err := scopebus.New(mb).Subscribe(scopebus.World(), func(event string, payload json.RawMessage, _ string) {
		if event != scopebus.EventStateSet {
			return
		}
		var p scopebus.StatePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return
		}
		mu.Lock()
		versions = append(versions, p.Version)
		mu.Unlock()
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	// The original leader writes the key twice.
	d1 := leaderDirector(st).WithScopeBus(scopebus.New(mb), "world-director-1")
	api1 := &API{d: d1, ctx: ctx}
	require.NoError(t, api1.Set("war", json.RawMessage(`"a"`)))
	require.NoError(t, api1.Set("war", json.RawMessage(`"b"`)))

	// The promoted leader: a FRESH director over the same store, with an empty version cache.
	d2 := leaderDirector(st).WithScopeBus(scopebus.New(mb), "world-director-2")
	api2 := &API{d: d2, ctx: ctx}
	require.NoError(t, api2.Set("war", json.RawMessage(`"c"`)))

	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(versions) == 3 },
		time.Second, 5*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []uint64{1, 2, 3}, versions,
		"the promoted leader must continue the store's sequence, not restart its own counter")
}

// requireSetOK / setErr adapt d.set's two-value form (it returns the store version since #355) for tests
// that only care whether the write landed.
func requireSetOK(ctx context.Context, t *testing.T, d *Director, key string, value json.RawMessage, msgAndArgs ...any) {
	t.Helper()
	_, err := d.set(ctx, key, value)
	require.NoError(t, err, msgAndArgs...)
}

func setErr(ctx context.Context, d *Director, key string, value json.RawMessage) error {
	_, err := d.set(ctx, key, value)
	return err
}
