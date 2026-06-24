// Command telos-world runs a world-simulation shard: the zone actor loop plus the
// gRPC Play server. Phase 1 serves one hardcoded two-room zone (docs/ROADMAP.md).
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"github.com/double-nibble/telosmud/internal/config"
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

	shard := world.NewDemoShard()
	go shard.Run(ctx)

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
