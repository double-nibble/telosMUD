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
	shard := buildShard(ctx, cfg, zoneID)
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
func buildShard(ctx context.Context, cfg config.Config, zoneID string) *world.Shard {
	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		slog.Warn("redis unavailable; single-shard mode (cross-shard exits sealed)",
			"addr", cfg.Redis.Addr, "err", err)
		return world.NewDemoShard()
	}
	dir := directory.NewRedis(rdb, "telos")
	if err := dir.RegisterZone(ctx, zoneID, cfg.ShardAddr); err != nil {
		slog.Error("zone registration failed", "zone", zoneID, "err", err)
		os.Exit(1)
	}
	slog.Info("registered zone", "zone", zoneID, "shard_addr", cfg.ShardAddr, "shard_id", cfg.ShardID)
	return world.NewShard(zoneID, cfg.ShardAddr, dir, world.GRPCDialer())
}
