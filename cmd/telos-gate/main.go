// Command telos-gate is the edge service: it terminates telnet/TLS/SSH
// connections, runs protocol/GMCP negotiation, and proxies players to world
// shards over the gRPC Play stream.
//
// Phase 0: boot config + observability and block until shutdown. The telnet
// listener and Play client arrive in Phase 1 — see docs/ROADMAP.md.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/double-nibble/telosmud/internal/config"
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

	slog.Info("starting", "env", cfg.Env)
	// TODO(phase1): start the telnet listener and the gRPC Play client.

	<-ctx.Done()
	slog.Info("shutting down")
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}
