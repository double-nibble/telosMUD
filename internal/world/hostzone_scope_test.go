package world

import (
	"context"
	"testing"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// TestHostZoneRegistersScopeReplica pins the 16.4a scope-registration fix: a zone brought up at RUNTIME
// (HostZone / a drain adoption) is registered with scope replication — its region-id replica is stamped and
// it joins the region delivery map — so a region scope delta reaches it, not just the boot zones. The demo
// "heartlands" region = [midgaard, darkwood]; the shard boots hosting the region-LESS crypt, then hosts
// midgaard at runtime and must pick up its region.
func TestHostZoneRegistersScopeReplica(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	mb := commbus.NewMemBus()
	t.Cleanup(func() { _ = mb.Close() })
	zoneBus := scopebus.New(mb)

	sh := NewShardFromContent(lc, []string{"crypt"}, "crypt", "", nil, nil).
		WithScopeBus(zoneBus, lc.Regions)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)

	var z *Zone
	waitCond(t, "shard running so HostZone succeeds", func() bool {
		hz, err := sh.HostZone("midgaard")
		if err == nil {
			z = hz
			return true
		}
		return false
	})

	if z.scopes.regionID != "heartlands" {
		t.Fatalf("runtime-hosted midgaard regionID = %q, want heartlands (scope replica not stamped)", z.scopes.regionID)
	}
	sh.scopes.mu.RLock()
	got := sh.scopes.zoneRegion["midgaard"]
	subscribed := sh.scopes.regions["heartlands"]
	sh.scopes.mu.RUnlock()
	if got != "heartlands" {
		t.Fatalf("midgaard not in the region delivery map (got %q); a region delta won't route to it", got)
	}
	if !subscribed {
		t.Fatal("shard did not subscribe to the heartlands region for the runtime-hosted zone")
	}
}
