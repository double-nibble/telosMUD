// Command telos-world runs a world-simulation shard: the zone actor loop plus the
// gRPC Play server. Phase 1 serves one hardcoded two-room zone (docs/ROADMAP.md).
//
// Startup order: load config -> obs.Init (installs the slog default logger; honors
// DEBUG=1 to enable Debug-level world tracing) -> start the zone actor goroutine ->
// serve gRPC. SIGINT/SIGTERM cancels ctx, which both stops the zone loop and
// gracefully drains the gRPC server.
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"github.com/double-nibble/telosmud/db"
	"github.com/double-nibble/telosmud/internal/checkpoint"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/obs"
	"github.com/double-nibble/telosmud/internal/store"
	"github.com/double-nibble/telosmud/internal/world"
)

// enabledPacks is the content packs a world shard loads. v1 ships only the demo pack;
// strip-the-stdlib / per-deploy pack selection (a config field) is a later concern.
var enabledPacks = []string{content.DemoPack}

func main() {
	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	if cfg.Service == "telos" {
		cfg.Service = "telos-world"
	}
	shutdown := obs.Init(cfg.Service, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Build the shard. With Redis reachable, register every zone this shard hosts in
	// the directory and wire cross-shard handoff; otherwise fall back to a single-shard
	// world whose cross-shard exits are sealed (so a bare run still works).
	zones := cfg.Zones
	if len(zones) == 0 {
		zones = []string{"midgaard"}
	}
	shard := buildShard(ctx, stop, cfg, zones)
	go shard.Run(ctx) // each zone actor loop owns its world state from here on

	lis, err := net.Listen("tcp", cfg.WorldListen)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.WorldListen, "err", err)
		os.Exit(1)
	}
	gs := grpc.NewServer()
	shard.Register(gs)

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		// Shutdown durability rests on the per-player save-on-Detach flush (a clean client
		// disconnect runs leave -> flush while everything is still live). The Shard.Drain hook —
		// a bulk flush of every live player — is built (PERSISTENCE.md §6) but its TRIGGER is
		// Phase 10: a true graceful drain must flush BEFORE ctx cancels the zone+saver goroutines,
		// which the placement controller will coordinate. Calling it here post-cancel would be
		// best-effort only, so we leave the trigger to Phase 10 and rely on per-session flushes.
		gs.GracefulStop()
	}()

	slog.Info("starting", "env", cfg.Env, "listen", cfg.WorldListen)
	if err := gs.Serve(lis); err != nil {
		slog.Error("serve failed", "err", err)
	}
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

// buildShard wires the world shard. With Redis reachable it registers EVERY zone this
// shard hosts in the directory, claims a lease on each, and enables cross-shard
// handoff; otherwise it logs a warning and returns a single-shard world whose
// cross-shard exits are sealed (so a bare run without backing services still works).
// home (the spawn zone for fresh logins) is the first configured zone.
func buildShard(ctx context.Context, stop func(), cfg config.Config, zones []string) *world.Shard {
	home := zones[0]

	// Load content BEFORE building any zone (docs/PHASE4-PLAN.md §3). This is synchronous boot
	// I/O on the construction goroutine — never on a zone goroutine — so blocking is fine. If
	// Postgres is unreachable the shard boots EMPTY (the bare-engine invariant), exactly as it
	// degrades to single-shard when Redis is down. The demo world lives only in the DB/YAML, so
	// nothing demo is compiled into this path.
	lc := loadContent(ctx, cfg)

	// Open the long-lived Postgres pool (slice 4.2 character ladder + slice 4.3 hot-reload re-read).
	// It is OPTIONAL: if Postgres is unreachable it stays nil and characters are EPHEMERAL and hot
	// reload is DISABLED (today's behavior) — the bare-engine boot degrades, never crashes. Separate
	// from the content-load pool above (which is closed after the boot load); this one lives for the
	// shard's lifetime. The same *store.Pool serves both world.CharacterStore and
	// content.DefinitionSource, so the hot-reload single-ref re-read reuses it.
	livePool := openLivePool(ctx, cfg)
	var charStore world.CharacterStore
	var defSource content.DefinitionSource
	if livePool != nil {
		charStore = livePool
		defSource = livePool
	}

	// Optional content bus for hot reload (slice 4.3). NATS unreachable => hot reload DISABLED
	// (boot-load still works); never fatal, exactly like Redis/Postgres being down.
	bus := openContentBus(cfg)

	// Optional comms bus (Phase 8.3): the world is the comms SOURCE — it publishes channel lines through
	// a RoleWorld commbus handle (commbus.OpenWorld ONLY — never OpenGate). NATS unreachable => a Disabled
	// no-op bus => channels degrade to "temporarily offline" for the speaker, never a boot failure
	// (the never-fatal rule, mirroring openContentBus + the gate's commbus.OpenGate).
	comms := commbus.OpenWorld(cfg.NATS.URL, func(err error) {
		if err != nil {
			slog.Warn("nats unavailable; comms disabled (world source)", "url", cfg.NATS.URL, "err", err)
			return
		}
		slog.Info("comms bus ready (world source)", "url", cfg.NATS.URL)
	})

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		slog.Warn("redis unavailable; single-shard mode (cross-shard exits sealed)",
			"addr", cfg.Redis.Addr, "err", err)
		// Redis down: no checkpoint tier, but the Postgres tier (if up) still gives save-on-logout
		// durability — a character survives a restart, just with a wider crash window.
		return world.NewShardFromContent(lc, zones, home, "", nil, nil).
			WithPersistence(charStore, nil).
			WithHotReload(defSource, bus, enabledPacks).
			WithComms(comms)
	}
	dir := directory.NewRedis(rdb, "telos")
	ckpt := checkpoint.NewRedis(rdb, "telos") // ~10s Redis checkpoint tier of the ladder

	// Publish where THIS shard is reachable (shard-id -> endpoint) BEFORE claiming any
	// zone, so the moment a zone names us as owner, peers can resolve us to a live
	// address. Then heartbeat it and drop it on shutdown.
	if err := dir.RegisterShard(ctx, cfg.ShardID, cfg.ShardAddr, directory.DefaultShardLease); err != nil {
		slog.Error("shard registration failed", "shard_id", cfg.ShardID, "err", err)
		os.Exit(1)
	}
	slog.Info("registered shard", "shard_id", cfg.ShardID, "endpoint", cfg.ShardAddr, "lease", directory.DefaultShardLease)
	go renewShardRegistration(ctx, dir, cfg.ShardID, cfg.ShardAddr) //nolint:gosec // G118: ctx is the shard's main lifetime ctx (cancelled on shutdown) — exactly what this renewal goroutine should follow.

	// Claim an EXCLUSIVE lease on EACH zone, owned by this shard's id. A live, different
	// shard already owning one is a misconfiguration we refuse to start with — rather
	// than silently both claiming it and becoming two writers for one zone.
	for _, zoneID := range zones {
		ok, err := dir.ClaimZone(ctx, zoneID, cfg.ShardID, directory.DefaultZoneLease)
		if err != nil {
			slog.Error("zone claim failed", "zone", zoneID, "err", err)
			os.Exit(1)
		}
		if !ok {
			owner, _ := dir.ShardForZone(ctx, zoneID)
			slog.Error("zone already claimed by another live shard; refusing to start", "zone", zoneID, "owner_shard", owner)
			os.Exit(1)
		}
		slog.Info("claimed zone", "zone", zoneID, "shard_id", cfg.ShardID, "lease", directory.DefaultZoneLease)

		// Keep each lease alive while we run; release it on shutdown so another shard can
		// take over immediately instead of waiting out the lease. stop fences us if we
		// ever lose any lease.
		go renewZoneLease(ctx, stop, dir, zoneID, cfg.ShardID) //nolint:gosec // G118: ctx is the shard's main lifetime ctx (cancelled on shutdown) — exactly what this lease goroutine should follow.
	}

	return world.NewShardFromContent(lc, zones, home, cfg.ShardAddr, dir, world.GRPCDialer()).
		WithPersistence(charStore, ckpt).
		WithHotReload(defSource, bus, enabledPacks).
		WithComms(comms)
}

// openLivePool opens the long-lived Postgres pool the shard keeps for its lifetime: it backs both
// the durable character store (the async saver + login read) and the hot-reload single-ref re-read
// (content.DefinitionSource). It is OPTIONAL and never fatal: an unreachable database returns nil,
// so characters stay ephemeral AND hot reload is disabled (the bare-engine degradation) rather than
// the world failing to boot. Returns the concrete *store.Pool (nil on failure) so the caller can
// keep the CharacterStore and DefinitionSource interfaces truly nil when there is no pool — a typed
// nil in an interface would be non-nil and defeat the disabled-fallback guards.
func openLivePool(ctx context.Context, cfg config.Config) *store.Pool {
	pool, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		slog.Warn("postgres unavailable; characters ephemeral and hot reload disabled", "err", err)
		return nil
	}
	slog.Info("postgres ready (durable characters + hot reload)")
	return pool
}

// openContentBus connects the content hot-reload bus (NATS). OPTIONAL and never fatal: an
// unreachable broker returns a nil Bus, so hot reload is simply disabled (boot-load still works).
// Returns the contentbus.Bus interface so the disabled path is a true nil interface (a typed nil
// *NATSBus would be non-nil and slip past WithHotReload's nil guard).
func openContentBus(cfg config.Config) contentbus.Bus {
	bus, err := contentbus.Connect(cfg.NATS.URL)
	if err != nil {
		slog.Warn("nats unavailable; content hot reload disabled", "url", cfg.NATS.URL, "err", err)
		return nil
	}
	slog.Info("content bus ready (hot reload enabled)", "url", cfg.NATS.URL)
	return bus
}

// loadContent reads the enabled packs from Postgres, optionally running migrations first
// (opt-in via TELOS_DB_AUTOMIGRATE, advisory-locked so multi-shard boots serialize). If the
// database is unreachable it logs a warning and returns EMPTY content — the engine boots with
// zero rooms (bare-engine invariant), and a login is rejected cleanly rather than panicking.
// Postgres is the production source; the embedded YAML pack is the unit-test/dev source.
func loadContent(ctx context.Context, cfg config.Config) *content.LoadedContent {
	if db.AutoMigrateEnabled() {
		if err := db.Migrate(ctx, cfg.Postgres.DSN); err != nil {
			slog.Warn("auto-migrate failed; continuing (boot may be empty)", "err", err)
		} else {
			slog.Info("auto-migrate applied", "guard", db.AutoMigrateEnv)
		}
	}
	pool, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		slog.Warn("postgres unavailable; booting empty world (bare-engine)", "err", err)
		empty, _ := content.Load(ctx, nil, nil)
		return empty
	}
	defer pool.Close()
	lc, err := content.Load(ctx, pool, enabledPacks)
	if err != nil {
		slog.Warn("content load failed; booting empty world", "err", err)
		empty, _ := content.Load(ctx, nil, nil)
		return empty
	}
	if lc.Empty() {
		slog.Warn("no content loaded (packs absent in DB?); booting empty world", "packs", enabledPacks)
	} else {
		slog.Info("content loaded from postgres", "packs", enabledPacks, "zones", len(lc.Zones))
	}
	return lc
}

// renewShardRegistration heartbeats this shard's id->endpoint registration until ctx is
// cancelled, then deregisters it so peers stop resolving a dead address immediately.
// Losing the registration is not fatal (unlike a zone lease): the next tick re-publishes
// it; it only needs to stay live so zone owners remain dialable.
func renewShardRegistration(ctx context.Context, dir *directory.Redis, shardID, endpoint string) {
	t := time.NewTicker(directory.DefaultShardLease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = dir.DeregisterShard(rctx, shardID, endpoint)
			cancel()
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := dir.RegisterShard(rctx, shardID, endpoint, directory.DefaultShardLease)
			cancel()
			if err != nil {
				slog.Warn("shard registration renewal error", "shard_id", shardID, "err", err)
			}
		}
	}
}

// renewZoneLease heartbeats this shard's zone claim until ctx is cancelled, then
// releases it. If a renewal ever reports the lease was lost to ANOTHER shard, it
// fences this process (stop) — a shard that no longer owns its zone must not keep
// writing, or we are back to two writers. Each renewal has its own short timeout so a
// slow Redis can't silently stall the heartbeat past the lease.
func renewZoneLease(ctx context.Context, stop func(), dir *directory.Redis, zoneID, shardID string) {
	t := time.NewTicker(directory.DefaultZoneLease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = dir.ReleaseZone(rctx, zoneID, shardID)
			cancel()
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			ok, err := dir.ClaimZone(rctx, zoneID, shardID, directory.DefaultZoneLease)
			cancel()
			switch {
			case err != nil:
				// Transient (Redis blip): keep trying. If it persists past the lease the
				// claim lapses and the next renewal returns !ok, fencing us below.
				slog.Warn("zone lease renewal error", "zone", zoneID, "err", err)
			case !ok:
				// Another shard now owns this zone; we must stop writing immediately.
				slog.Error("lost zone lease to another shard; fencing this shard", "zone", zoneID)
				stop()
				return
			}
		}
	}
}
