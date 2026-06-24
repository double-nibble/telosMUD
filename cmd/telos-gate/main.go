// Command telos-gate is the edge service: it terminates telnet connections and
// proxies players to world shards over the gRPC Play stream. TLS/SSH, GMCP, and
// real auth arrive in later phases (docs/ACCOUNT.md, GMCP.md).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/gate"
	"github.com/double-nibble/telosmud/internal/obs"
)

func main() {
	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	if cfg.Service == "telos" {
		cfg.Service = "telos-gate"
	}
	shutdown := obs.Init(cfg.Service, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cc, err := grpc.NewClient(cfg.WorldTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("world client failed", "target", cfg.WorldTarget, "err", err)
		os.Exit(1)
	}
	defer cc.Close()

	srv := gate.New(cfg.GateListen, directory.Static{Addr: cfg.WorldTarget}, playv1.NewPlayClient(cc))
	slog.Info("starting", "env", cfg.Env, "listen", cfg.GateListen, "world", cfg.WorldTarget)
	if err := srv.ListenAndServe(ctx); err != nil {
		slog.Error("gate serve failed", "err", err)
	}
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}
