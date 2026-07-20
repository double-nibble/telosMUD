package world

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// scopeforge_test.go — a zone script must not be able to emit a DIRECTOR-only event name.
//
// Signal-UP and broadcast-DOWN share ONE subject per scope. A durable signal-up publish is an ordinary
// NATS publish to telos.scope.<kind>.<id>, and every shard CORE-subscribes that same subject for
// down-broadcasts — so a zone signalling `scope.state.set` upward had it delivered to every replica as
// though a director had written it, bypassing the director, its lease and its CAS. The director-side
// reserved-name guard never saw it, because the frame never went through the director.
//
// Since the delta carries a fence version, the consequence is worse than a wrong value: a forged version
// far ahead of the real one makes every subsequent LEGITIMATE director write drop as stale, and there is
// no read-through anywhere to recover the key.

// TestReservedDownEventsAreSharedByBothEnds pins that the two ends of the bus agree on the list. The
// director restating it locally is how they would drift.
func TestReservedDownEventsAreSharedByBothEnds(t *testing.T) {
	reserved := []string{scopebus.EventStateSet, "content.pull.result", "content.reload.audit"}
	for _, ev := range reserved {
		assert.Truef(t, scopebus.ReservedDownEvent(ev), "%q must be reserved to the DOWN direction", ev)
	}
	// A zone's ordinary signal-up names, and the director's scheduler names, must stay usable UPWARD —
	// boss.died in particular is a legitimate signal-up the scheduler consumes, so over-reserving it here
	// would break the shipped spawn feature. A guard too broad is as bad as one too narrow.
	for _, ev := range []string{"boss_slain", "boss.died", "spawn.boss", "region_raid", "contented"} {
		assert.Falsef(t, scopebus.ReservedDownEvent(ev), "%q must remain emittable as a signal-UP", ev)
	}
}

// TestZoneCannotSignalAReservedDownEvent is the end-to-end guard, driven through the real signal-up
// publisher and a real transient subscriber on the same subject — which is exactly the aliasing that made
// the forge work.
func TestZoneCannotSignalAReservedDownEvent(t *testing.T) {
	lc, err := content.LoadDemoPack()
	require.NoError(t, err)
	s := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil)
	bus := scopebus.New(commbus.NewMemBus()).WithDurable(commbus.NewMemJetStream(), "world-test")
	s.WithScopeBus(bus, lc.Regions)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.scopes.signalLoop(ctx)

	events := make(chan scopebus.DurableEvent, 8)
	c, err := bus.SubscribeDurable(scopebus.World(), "dir-world", func(ev scopebus.DurableEvent) bool {
		events <- ev
		return true
	})
	require.NoError(t, err)
	defer func() { _ = c.Stop() }()

	z := s.zoneByID("midgaard")
	require.NotNil(t, z, "precondition: the shard hosts the zone")

	// The forge attempt, then a legitimate signal. Ordering is the point: the legitimate one arriving
	// proves the path is live and drained, so the forged one's absence is a REFUSAL rather than a race.
	src := `
		signal_world("` + scopebus.EventStateSet + `", {key = "pvp", value = true})
		signal_world("boss_slain", {boss = "vurgoth"})
	`
	require.NoError(t, z.lua.runChunk("forge", src),
		"the refusal must be silent to the SCRIPT, never a Lua error")

	select {
	case ev := <-events:
		assert.NotEqual(t, scopebus.EventStateSet, ev.Event,
			"a zone script forged a reserved DOWN event onto the scope subject: signal-up and "+
				"broadcast-down share one subject, so every replica would apply it as an authoritative "+
				"director write, bypassing the director's lease and CAS entirely")
		assert.Equal(t, "boss_slain", ev.Event, "the legitimate signal must still get through")
	case <-time.After(2 * time.Second):
		t.Fatal("the legitimate signal never arrived — this test proved nothing about the forged one")
	}
}

// TestImplausibleVersionGapIsLogged pins the operator-visible half. The gap is LOGGED, not rejected: a
// genuine large gap (a long transient outage) must still apply, and silently freezing a key would be
// worse than the gap. The forged path is closed at its source instead.
func TestImplausibleVersionGapIsLogged(t *testing.T) {
	z, buf := replicaZoneWithLog()
	z.applyScopeDelta(scopeDeltaMsg{kind: "world", key: "pvp", value: json.RawMessage(`false`), version: 3})
	z.applyScopeDelta(scopeDeltaMsg{kind: "world", key: "pvp", value: json.RawMessage(`true`), version: 1 << 40})

	assert.Contains(t, buf.String(), "jumped implausibly far ahead",
		"a version jump no sequence of real director writes could produce must be surfaced to an operator")

	// And an ordinary gap must NOT warn — an alert that fires on normal catch-up is an alert nobody reads.
	z2, buf2 := replicaZoneWithLog()
	z2.applyScopeDelta(scopeDeltaMsg{kind: "world", key: "pvp", value: json.RawMessage(`false`), version: 3})
	z2.applyScopeDelta(scopeDeltaMsg{kind: "world", key: "pvp", value: json.RawMessage(`true`), version: 900})
	assert.NotContains(t, buf2.String(), "jumped implausibly far ahead",
		"a replica that missed a few hundred writes is catching up, not being attacked")
}

// replicaZoneWithLog builds a bare zone with a capturing logger and an initialised scope replica.
func replicaZoneWithLog() (*Zone, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	z := &Zone{scopes: newScopeReplica(), log: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	return z, buf
}
