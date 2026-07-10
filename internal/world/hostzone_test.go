package world

import (
	"context"
	"sync"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// TestHostZoneAddsAZoneAtRuntime is the Phase-16.4a runtime zone-add primitive: a shard that booted
// hosting only midgaard can, while running, host darkwood too — the standby re-claim a graceful drain
// reuses. The prototypes are already in the cache (defineContent loads ALL zones), so HostZone builds the
// zone from the retained content, registers it for routing, and launches its actor.
func TestHostZoneAddsAZoneAtRuntime(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	// Boot hosting ONLY midgaard; darkwood is a not-yet-hosted zone whose prototypes are still cached.
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)

	if sh.ZoneByID("darkwood") != nil {
		t.Fatal("darkwood hosted before HostZone")
	}

	// Run sets runCtx/runWG asynchronously; retry until the shard is running (HostZone is a no-op that
	// returns an error until then, and idempotent after — so retrying is safe).
	var z *Zone
	waitCond(t, "shard running so HostZone succeeds", func() bool {
		hz, err := sh.HostZone(context.Background(), "darkwood")
		if err == nil {
			z = hz
			return true
		}
		return false
	})

	if z == nil || sh.ZoneByID("darkwood") != z {
		t.Fatal("darkwood not registered for routing after HostZone")
	}
	if len(z.rooms) == 0 {
		t.Fatal("runtime-hosted zone has no rooms — content was not built from the retained pack")
	}
	// Idempotent: a second HostZone returns the SAME live zone, not a rebuilt duplicate.
	if z2, err := sh.HostZone(context.Background(), "darkwood"); err != nil || z2 != z {
		t.Fatalf("HostZone not idempotent: z2=%p err=%v", z2, err)
	}
}

// TestHostZoneErrsBeforeRun and on a content-less shard: HostZone needs a running shard (a run ctx) and
// retained content to build from. Both failure modes return an error rather than panicking.
func TestHostZoneErrsWithoutRunOrContent(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	// Built from content but NOT running: no run ctx yet.
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil)
	if _, err := sh.HostZone(context.Background(), "darkwood"); err == nil {
		t.Fatal("HostZone should error before Run (no run ctx)")
	}

	// A demo shard (NewMultiShard) retains no LoadedContent, so even while running it can't build a new zone.
	demo := NewMultiShard([]string{"midgaard"}, "midgaard", "", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go demo.Run(ctx)
	waitCond(t, "demo shard running", func() bool {
		_, err := demo.HostZone(context.Background(), "darkwood")
		// It IS running once we get the content error (not the not-running error); before that, keep waiting.
		return err != nil && err.Error() != `HostZone "darkwood": shard not running`
	})
	if _, err := demo.HostZone(context.Background(), "darkwood"); err == nil {
		t.Fatal("HostZone should error on a shard with no retained content")
	}
}

// TestHostZoneRefusesAfterShutdown pins the closed-guard: once Run has observed ctx-cancel and is tearing
// down, HostZone must refuse rather than wg.Add onto a WaitGroup that Run's Wait is already draining (the
// shutdown-window Add/Wait race the review flagged).
func TestHostZoneRefusesAfterShutdown(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sh.Run(ctx); close(done) }()

	waitCond(t, "shard running", func() bool { _, err := sh.HostZone(context.Background(), "darkwood"); return err == nil })

	cancel()
	<-done // Run has set closed=true and completed wg.Wait

	if _, err := sh.HostZone(context.Background(), "crypt"); err == nil {
		t.Fatal("HostZone should refuse after the shard began shutting down")
	}
}

// TestHostZoneConcurrentWithRouting hammers the mu-guarded routing readers (zonesList/zoneByID — the same
// accessors Play attach / Handoff.Prepare / hot-reload / scope delivery use) while a HostZone writes the
// map, so `-race` proves no reader touches the raw map. A regression guard for the missed-reader class.
func TestHostZoneConcurrentWithRouting(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = sh.zonesList()
					_ = sh.ZoneByID("darkwood")
				}
			}
		}()
	}

	waitCond(t, "HostZone darkwood while routing reads race it", func() bool {
		_, err := sh.HostZone(context.Background(), "darkwood")
		return err == nil
	})
	close(stop)
	wg.Wait()
}
