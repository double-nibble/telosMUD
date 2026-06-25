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

	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/obs"
	"github.com/double-nibble/telosmud/internal/world"
)

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

	// Build the shard. With Redis reachable, register this shard's zone in the
	// directory and wire cross-shard handoff; otherwise fall back to a single-shard
	// world whose cross-shard exits are sealed (so a bare run still works).
	zoneID := "midgaard"
	if len(cfg.Zones) > 0 {
		zoneID = cfg.Zones[0]
	}
	shard := buildShard(ctx, stop, cfg, zoneID)
	go shard.Run(ctx) // the zone actor loop owns all world state from here on

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

// buildShard wires the world shard. With Redis reachable it registers the shard's
// zone in the directory and enables cross-shard handoff; otherwise it logs a warning
// and returns a single-shard world whose cross-shard exits are sealed (so a bare run
// without backing services still works).
func buildShard(ctx context.Context, stop func(), cfg config.Config, zoneID string) *world.Shard {
	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		slog.Warn("redis unavailable; single-shard mode (cross-shard exits sealed)",
			"addr", cfg.Redis.Addr, "err", err)
		return world.NewDemoShard()
	}
	dir := directory.NewRedis(rdb, "telos")

	// Claim an EXCLUSIVE lease on the zone. A live, different shard already owning it
	// is a misconfiguration we refuse to start with — rather than silently both
	// claiming it and becoming two writers for one zone.
	ok, err := dir.ClaimZone(ctx, zoneID, cfg.ShardAddr, directory.DefaultZoneLease)
	if err != nil {
		slog.Error("zone claim failed", "zone", zoneID, "err", err)
		os.Exit(1)
	}
	if !ok {
		owner, _ := dir.ShardForZone(ctx, zoneID)
		slog.Error("zone already claimed by another live shard; refusing to start", "zone", zoneID, "owner", owner)
		os.Exit(1)
	}
	slog.Info("claimed zone", "zone", zoneID, "shard_addr", cfg.ShardAddr, "shard_id", cfg.ShardID, "lease", directory.DefaultZoneLease)

	// Keep the lease alive while we run; release it on shutdown so another shard can
	// take over immediately instead of waiting out the lease. stop fences us if we
	// ever lose the lease.
	go renewZoneLease(ctx, stop, dir, zoneID, cfg.ShardAddr)

	return world.NewShard(zoneID, cfg.ShardAddr, dir, world.GRPCDialer())
}

// renewZoneLease heartbeats this shard's zone claim until ctx is cancelled, then
// releases it. If a renewal ever reports the lease was lost to ANOTHER shard, it
// fences this process (stop) — a shard that no longer owns its zone must not keep
// writing, or we are back to two writers. Each renewal has its own short timeout so a
// slow Redis can't silently stall the heartbeat past the lease.
func renewZoneLease(ctx context.Context, stop func(), dir *directory.Redis, zoneID, shardAddr string) {
	t := time.NewTicker(directory.DefaultZoneLease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = dir.ReleaseZone(rctx, zoneID, shardAddr)
			cancel()
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			ok, err := dir.ClaimZone(rctx, zoneID, shardAddr, directory.DefaultZoneLease)
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
