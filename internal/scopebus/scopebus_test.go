package scopebus

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/commbus"
)

type recvEvent struct {
	event   string
	payload string
	source  string
}

func recv(t *testing.T, ch <-chan recvEvent) (recvEvent, bool) {
	t.Helper()
	select {
	case e := <-ch:
		return e, true
	case <-time.After(time.Second):
		return recvEvent{}, false
	}
}

func TestScopeSubject(t *testing.T) {
	cases := []struct {
		scope Scope
		want  string
		err   bool
	}{
		{World(), "telos.scope.world", false},
		{Region("duskwall"), "telos.scope.region.duskwall", false},
		{ZoneScope("midgaard"), "telos.scope.zone.midgaard", false},
		{Scope{Kind: "world", ID: "x"}, "", true}, // world takes no id
		{Region("bad id!"), "", true},             // invalid charset
		{Region(""), "", true},                    // empty id
		{Scope{Kind: "bogus"}, "", true},
	}
	for _, c := range cases {
		got, err := c.scope.Subject()
		if c.err {
			assert.Error(t, err, "%+v", c.scope)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, c.want, got)
	}
}

func TestScopeBusSignalAndSubscribe(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	b := New(core)
	ctx := context.Background()

	got := make(chan recvEvent, 8)
	sub, err := b.Subscribe(World(), func(event string, payload json.RawMessage, source string) {
		got <- recvEvent{event, string(payload), source}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, b.Signal(ctx, World(), "invasion.start", json.RawMessage(`{"n":1}`), "world-director"))
	e, ok := recv(t, got)
	require.True(t, ok, "world event not delivered")
	assert.Equal(t, "invasion.start", e.event)
	assert.JSONEq(t, `{"n":1}`, e.payload)
	assert.Equal(t, "world-director", e.source)
}

func TestScopeBusRegionIsolation(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	b := New(core)
	ctx := context.Background()

	got := make(chan recvEvent, 8)
	sub, err := b.Subscribe(Region("duskwall"), func(event string, payload json.RawMessage, source string) {
		got <- recvEvent{event, string(payload), source}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// An event to a DIFFERENT region must NOT reach the duskwall subscriber.
	require.NoError(t, b.Signal(ctx, Region("ironhold"), "noise", nil, "z"))
	// The duskwall event does.
	require.NoError(t, b.Signal(ctx, Region("duskwall"), "city_liberated", json.RawMessage(`{"hero":"kurt"}`), "duskwall-director"))

	e, ok := recv(t, got)
	require.True(t, ok, "duskwall event not delivered")
	assert.Equal(t, "city_liberated", e.event, "region isolation broken — got %q (a foreign region's event leaked)", e.event)

	// No second event (ironhold's must not have arrived).
	if _, ok := recv(t, got); ok {
		t.Fatal("a foreign region's event leaked to the duskwall subscriber")
	}
}

func TestScopeBusRejectsBadScopeAndEmptyEvent(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	b := New(core)
	ctx := context.Background()

	assert.Error(t, b.Signal(ctx, Region("bad!"), "ev", nil, "s"), "a malformed scope id must be refused")
	assert.Error(t, b.Signal(ctx, World(), "  ", nil, "s"), "an empty event name must be refused")
}
