// Command telos-director runs the orchestration tier (docs/WORLD-EVENTS.md §3, Phase 10): the
// supra-zone director actors that own region/world state. It is the fourth deployable alongside
// telos-gate, telos-world, and (Phase 14) telos-account — hosted OUT-OF-BAND from the simulation shards
// so orchestration never competes with zone ticks for CPU.
//
// Startup: load config -> obs.Init -> open the scope-state store (Postgres) -> open the directory
// (Redis) for LEADER ELECTION -> build + run the world director under leader election. SIGINT/SIGTERM
// cancels ctx, which stops the director loop and RESIGNS its scope lease so a standby takes over
// immediately. Region directors (one per region_defs) join here once region content exists (10.3+).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentpull"
	"github.com/double-nibble/telosmud/internal/director"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/obs"
	"github.com/double-nibble/telosmud/internal/placement"
	"github.com/double-nibble/telosmud/internal/scopebus"
	"github.com/double-nibble/telosmud/internal/store"
)

func main() {
	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	if cfg.Service == "telos" {
		cfg.Service = "telos-director"
	}
	shutdown := obs.Init(cfg.Service, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The scope-state store (Postgres) is the director's reason for being — it owns + persists
	// region/world state. Without a DSN there is nothing durable to direct, so this is fatal.
	if cfg.Postgres.DSN == "" {
		slog.Error("telos-director needs a Postgres DSN (scope state has no durable home without it)")
		os.Exit(1)
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, 15*time.Second)
	pool, err := store.Open(openCtx, cfg.Postgres.DSN)
	cancelOpen()
	if err != nil {
		slog.Error("store open failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Leader election needs Redis (the lease). Without it a single director is always the leader — fine
	// for a single-process dev run, unsafe for a multi-instance deployment (no failover arbitration).
	var claimer director.LeaseClaimer
	var dir *directory.Redis // the fleet view the placement coordinator watches (nil without Redis)
	instanceID := directorInstanceID(cfg)
	if cfg.Redis.Addr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
		defer func() { _ = rdb.Close() }()
		dir = directory.NewRedis(rdb, "telos")
		claimer = dir
		slog.Info("leader election enabled", "instance", instanceID)
	} else {
		slog.Warn("no Redis configured: running as a single always-leader director (no failover)")
	}

	// Scoped event bus (Phase 10.4): the director CONSUMES its scope's signal-up events (durable) and
	// BROADCASTS state + remote effects DOWN (transient). The transient half is a RoleWorld commbus handle
	// (the scope subjects are not ACL-guarded; only chan/tell are); the durable half is the WORLD_EVENTS
	// JetStream stream. Both degrade to Disabled when NATS is down (orchestration input/output goes quiet,
	// never a boot failure). The instanceID seeds the down-broadcast author + the durable idempotency keys.
	scopeComms := commbus.OpenWorld(cfg.NATS.URL, func(err error) {
		if err != nil {
			slog.Warn("nats unavailable; director scope broadcast disabled", "url", cfg.NATS.URL, "err", err)
		}
	})
	scopeJS := commbus.OpenScopeJetStream(cfg.NATS.URL, func(err error) {
		if err != nil {
			slog.Warn("scope jetstream unavailable; director signal-up consume disabled", "url", cfg.NATS.URL, "err", err)
		}
	})
	scopeBus := scopebus.New(scopeComms).WithDurable(scopeJS, instanceID)

	// Load the scheduled-spawn definitions (Phase 12.4): the director owns the long-timer boss schedules
	// (a weekly world boss). It loads them from content (Postgres) at boot and drives them on its tick,
	// persisting each schedule's next-spawn time in scope state (restart-safe). An empty/unreachable load
	// yields no schedules — the director simply has no scheduled bosses.
	var schedules []director.Schedule
	if lc, err := content.Load(ctx, pool, []string{content.DemoPack}); err == nil {
		for _, sc := range lc.SpawnSchedules {
			schedules = append(schedules, director.BuildSchedule(sc))
		}
		if len(schedules) > 0 {
			slog.Info("loaded spawn schedules", "count", len(schedules))
		}
	} else {
		slog.Warn("could not load spawn schedules (none scheduled)", "err", err)
	}

	// Build + run the WORLD director. Region directors (one per region_defs) join here once region
	// content lands (10.3+); for now the world scope is the deployable. The signal HANDLER (the
	// orchestration "director script") is content-defined — not yet authored — so the director currently
	// drains + acks signals (the write/broadcast machinery is live via the API; the built-in logic plugs
	// in here when director-script content lands). WithSchedules wires the Phase-12.4 boss scheduler.
	world := director.New("", pool, slog.Default()).
		WithScopeBus(scopeBus, instanceID).
		WithSchedules(schedules)
	if claimer != nil {
		world.WithElection(claimer, instanceID)
	}
	// Coordinated content pull (#212 slice 4 PR E): wire the puller only when a content store is
	// configured, so an in-game `pull <version>` installs a published version fleet-wide. Without a
	// content store the world director simply doesn't run coordinated pulls (the request is logged + dropped).
	if cfg.Content.URL != "" {
		world.WithContentPuller(contentPuller{cfg: cfg})
		slog.Info("coordinated content pull enabled (director)", "content_url", cfg.Content.URL)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		world.Run(ctx) // returns on ctx cancel, resigning its lease
	}()

	// Zone-placement COORDINATOR (docs/PLACEMENT.md §2/§4, Phase 10.6b): while this director is the leader,
	// observe the live fleet + the zone assignment and compute the desired rebalancing PLAN. The coordinator
	// is an OPTIMIZER, not a dependency — liveness (claim-from-pool, failover) is decentralized in the world
	// servers, so this only nudges toward balance. It currently OBSERVES + PLANS + LOGS the recommended
	// moves; executing a move drains a zone's live players via the cross-shard handoff (Shard.Drain) — the
	// documented next integration. Runs only with Redis (the fleet view) + a configured pool.
	if dir != nil && len(cfg.Zones) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runPlacementCoordinator(ctx, dir, world, cfg.Zones)
		}()
	}

	slog.Info("starting", "env", cfg.Env, "instance", instanceID)
	<-ctx.Done()
	slog.Info("shutting down")
	wg.Wait() // let the director loop resign its lease + exit cleanly
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

// placementCoordinatorTick is how often the leader recomputes the placement plan. A moderate cadence: the
// directory leases (15s) already give liveness; balance is a slow-changing optimization, not a hot path.
const placementCoordinatorTick = 10 * time.Second

// runPlacementCoordinator is the leader-only observe→plan→report loop. It honors leadership (a standby
// director does nothing) and surfaces the desired moves; the drain executor is the documented next step.
func runPlacementCoordinator(ctx context.Context, dir *directory.Redis, d *director.Director, pool []string) {
	ticker := time.NewTicker(placementCoordinatorTick)
	defer ticker.Stop()
	fleet := fleetView{dir: dir}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !d.IsLeader() {
				continue // only the leader coordinates placement
			}
			live, assignment, err := placement.Observe(ctx, fleet, pool)
			if err != nil {
				slog.Warn("placement: fleet observe failed", "err", err)
				continue
			}
			moves := placement.Plan(live, assignment, pool)
			if len(moves) == 0 {
				continue // balanced + fully claimed: nothing to do
			}
			for _, m := range moves {
				if m.From == "" {
					slog.Info("placement: zone needs (re)assignment", "zone", m.Zone, "to", m.To)
				} else {
					slog.Info("placement: rebalance drain recommended", "zone", m.Zone, "from", m.From, "to", m.To)
				}
			}
		}
	}
}

// fleetView adapts *directory.Redis to placement.Fleet: ListShards is direct; ShardForZone maps the
// directory's ErrNotFound (an unclaimed zone) to found=false so the coordinator reassigns it.
type fleetView struct{ dir *directory.Redis }

func (f fleetView) ListShards(ctx context.Context) ([]string, error) { return f.dir.ListShards(ctx) }

func (f fleetView) ShardForZone(ctx context.Context, zone string) (string, bool, error) {
	owner, err := f.dir.ShardForZone(ctx, zone)
	if errors.Is(err, directory.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return owner, true, nil
}

// directorInstanceID is this director process's stable-per-run identity for the lease owner field. It
// prefers the configured shard id (operator-set), else a hostname+random id so two instances on one host
// never collide.
func directorInstanceID(cfg config.Config) string {
	if cfg.ShardID != "" {
		return "director-" + cfg.ShardID
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("director-%s-%s", host, uuid.NewString()[:8])
}

// contentPuller adapts the shared install pipeline (internal/contentpull) to the director's
// ContentPuller interface (#212 slice 4 PR E): a coordinated `pull <version>` resolves that version from
// the configured content store and imports it. The requested version overrides the config's pinned one.
type contentPuller struct{ cfg config.Config }

func (p contentPuller) Pull(ctx context.Context, version, _ string) error {
	_, err := contentpull.Pull(ctx, contentpull.Options{
		ContentURL:  p.cfg.Content.URL,
		Version:     version,
		Token:       p.cfg.Content.Token,
		CacheDir:    p.cfg.Content.CacheDir,
		PostgresDSN: p.cfg.Postgres.DSN,
		NATSURL:     p.cfg.NATS.URL,
	})
	return err
}
