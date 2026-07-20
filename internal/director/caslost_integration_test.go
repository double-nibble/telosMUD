package director

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// caslost_integration_test.go — #354 ABOVE the unit tier. caslost_test.go drives handleSignal DIRECTLY, so
// it pins the ack DECISION but never proves the decision reaches the transport: that a `false` on the ack
// channel becomes a real NAK on the shared durable consumer, that the consumer REDELIVERS it, that the
// redelivery converges, and that the message is not parked on the way. Those are properties of the
// director + scopebus + JetStream SEAM, and the whole defect in #354 was a seam defect — an ack consumed
// the event fleet-wide.
//
// The stack here is real except for the broker: a real Director running its actor loop, a real
// scopebus.Bus, and commbus.MemJetStream (which models redelivery, the NakBackoff SCHEDULE, and parking
// without sleeping any of it). Deterministic and hermetic — no docker, no sleeps.

// racingStore injects the ONE race #354 exists for: a stale leader and the promoted leader both writing.
// The first save to a raced key lets the OTHER writer land first at exactly the version this director
// expected, so the director's own CAS then loses. Deterministic — one injection point, no goroutines, no
// timing. Every later save to that key proceeds normally, so a redelivery can converge.
type racingStore struct {
	*memScopeStore
	mu     sync.Mutex
	raced  map[string]bool
	winner []byte // what the promoted leader wrote
}

func newRacingStore(winner []byte) *racingStore {
	return &racingStore{memScopeStore: newMemStore(), raced: map[string]bool{}, winner: winner}
}

func (r *racingStore) SaveWorldState(ctx context.Context, key string, value []byte, expected uint64) (uint64, bool, error) {
	r.mu.Lock()
	first := !r.raced[key]
	r.raced[key] = true
	r.mu.Unlock()
	if first {
		// The competing writer gets there first, at the version this director was about to CAS on.
		if _, ok, err := r.memScopeStore.SaveWorldState(ctx, key, r.winner, expected); err != nil || !ok {
			return 0, false, err
		}
	}
	return r.memScopeStore.SaveWorldState(ctx, key, value, expected)
}

// TestSignalNAKOnCASLossRedeliversThroughTheDurableConsumer is the end-to-end claim of #354: a signal whose
// scope-state write loses the CAS must come BACK on the shared durable consumer and be applied against the
// winner's value — not be consumed fleet-wide.
//
// It pins four things the direct-call tests structurally cannot:
//   - the NAK reaches the TRANSPORT (the consumer records exactly one redelivery, not zero and not a storm);
//   - the transport REDELIVERS it rather than dropping it (the handler runs twice);
//   - the redelivery CONVERGES on the winner's value (10 -> 11), so the retry is a re-read, not a re-push;
//   - the message is never PARKED (a NAK loop that exhausts MaxDeliver would lose the event just as
//     silently as the ack did, which would be the same bug wearing a different hat).
//
// It also pins that a LOST write is never broadcast DOWN: the losing value must never reach a zone's
// read-replica, or the fleet caches a value the store rejected. That property has no unit-tier coverage
// (the direct-call tests wire no bus), and dropping it is invisible to every other test in the package.
func TestSignalNAKOnCASLossRedeliversThroughTheDurableConsumer(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	store := newRacingStore([]byte(`10`)) // the promoted leader's winning value

	var mu sync.Mutex
	var handlerRuns int
	d := New("", store, discardLog()).
		WithScopeBus(dirBus, "world-director-1").
		WithSignalHandler(func(api *API, event string, _ json.RawMessage) {
			if event != "boss_slain" {
				return
			}
			mu.Lock()
			handlerRuns++
			mu.Unlock()
			// A DERIVED write (the world_script idempotency contract): re-read, recompute, write. A retry
			// that re-pushed the rejected value instead would land 1 here, not 11.
			n := 0
			if raw, ok := api.Get("bosses_slain"); ok {
				_ = json.Unmarshal(raw, &n)
			}
			nb, _ := json.Marshal(n + 1)
			_ = api.Set("bosses_slain", nb)
		}).
		WithTick(time.Hour) // no scheduler/tick noise; the signal path is under test

	// Stand in for the member zones' read-replicas: record every state delta broadcast DOWN, in order.
	var downMu sync.Mutex
	var downValues []string
	sub, err := dirBus.Subscribe(scopebus.World(), func(event string, payload json.RawMessage, _ string) {
		if event != scopebus.EventStateSet {
			return
		}
		var p scopebus.StatePayload
		if err := json.Unmarshal(payload, &p); err != nil || p.Key != "bosses_slain" {
			return
		}
		downMu.Lock()
		downValues = append(downValues, string(p.Value))
		downMu.Unlock()
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)

	require.NoError(t, zoneBus.SignalDurable(ctx, scopebus.World(), "boss_slain", json.RawMessage(`{"boss":"vurgoth"}`)))

	// Converged: the redelivery applied against the WINNER (10) and persisted 11.
	waitFor(t, "the redelivered signal converges to 11", func() bool {
		raw, _, found, _ := store.LoadWorldState(ctx, "bosses_slain")
		return found && string(raw) == "11"
	})

	// The NAK reached the transport: exactly one redelivery was scheduled for this consumer, and the
	// message was neither parked nor dropped as poison.
	assert.Len(t, js.NakDelays(d.consumerID()), 1,
		"the CAS loss must produce exactly ONE transport-level NAK — zero means the ack decision never "+
			"reached the consumer, more than one means the retry is not converging")
	assert.Empty(t, js.Parked(d.consumerID()),
		"a converging retry must never exhaust the redelivery budget — a parked message is the fleet-wide "+
			"loss #354 exists to prevent, just via the other door")
	assert.Empty(t, js.Poisoned(d.consumerID()))

	mu.Lock()
	runs := handlerRuns
	mu.Unlock()
	assert.Equal(t, 2, runs, "the handler must run once per delivery: the losing attempt and the converged retry")

	// The losing write must never have been pushed DOWN to the zones.
	downMu.Lock()
	got := append([]string(nil), downValues...)
	downMu.Unlock()
	assert.Equal(t, []string{"11"}, got,
		"only the write that LANDED may be broadcast down; broadcasting the CAS-losing value would seed "+
			"every zone read-replica with a value the store rejected")
}

// TestNonLeaderSignalIsRequeuedThroughTheDurableConsumer pins the #354 leader gate at the transport tier: a
// director that is subscribed but NOT the leader must requeue, and — because the NAK budget is finite — a
// standby that never regains leadership must PARK the message rather than spin forever or silently drop it.
// Parking is the bounded, observable outcome the issue's "no poison-spin" argument depends on.
func TestNonLeaderSignalIsRequeuedThroughTheDurableConsumer(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	var mu sync.Mutex
	var ran int
	d := New("", newMemStore(), discardLog()).
		WithScopeBus(dirBus, "world-director-1").
		WithSignalHandler(func(_ *API, _ string, _ json.RawMessage) {
			mu.Lock()
			ran++
			mu.Unlock()
		}).
		WithTick(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)

	// Subscribe as leader (Run does that for a claimer-less director), then lose the lease before the
	// event arrives: the consume-then-demote handoff the gate defends.
	waitFor(t, "the durable consumer to start", func() bool { return d.IsLeader() })
	d.leader.Store(false)

	require.NoError(t, zoneBus.SignalDurable(ctx, scopebus.World(), "boss_slain", nil))

	waitFor(t, "the standby to exhaust its redelivery budget and park", func() bool {
		return len(js.Parked(d.consumerID())) == 1
	})
	assert.Len(t, js.NakDelays(d.consumerID()), commbus.DefaultMaxDeliver,
		"every attempt by a non-leader must NAK on the backoff schedule, bounded by MaxDeliver")

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, ran, "a non-leader must never run the orchestration handler, on any delivery attempt")
}
