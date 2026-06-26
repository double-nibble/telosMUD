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
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
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

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		slog.Warn("redis unavailable; single-shard mode (cross-shard exits sealed)",
			"addr", cfg.Redis.Addr, "err", err)
		return world.NewShardFromContent(lc, zones, home, "", nil, nil)
	}
	dir := directory.NewRedis(rdb, "telos")

	// Publish where THIS shard is reachable (shard-id -> endpoint) BEFORE claiming any
	// zone, so the moment a zone names us as owner, peers can resolve us to a live
	// address. Then heartbeat it and drop it on shutdown.
	if err := dir.RegisterShard(ctx, cfg.ShardID, cfg.ShardAddr, directory.DefaultShardLease); err != nil {
		slog.Error("shard registration failed", "shard_id", cfg.ShardID, "err", err)
		os.Exit(1)
	}
	slog.Info("registered shard", "shard_id", cfg.ShardID, "endpoint", cfg.ShardAddr, "lease", directory.DefaultShardLease)
	go renewShardRegistration(ctx, dir, cfg.ShardID, cfg.ShardAddr)

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
		go renewZoneLease(ctx, stop, dir, zoneID, cfg.ShardID)
	}

	return world.NewShardFromContent(lc, zones, home, cfg.ShardAddr, dir, world.GRPCDialer())
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
