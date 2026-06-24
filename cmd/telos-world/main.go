// Command telos-world runs a world-simulation shard.
//
// Phase 0: boot config + observability and block until shutdown. The shard
// runtime (zones, the gRPC Play server, directory registration) arrives in
// Phase 1 — see docs/ROADMAP.md.
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
		cfg.Service = "telos-world"
	}
	shutdown := obs.Init(cfg.Service, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting", "env", cfg.Env, "nats", cfg.NATS.URL, "postgres", redact(cfg.Postgres.DSN))
	// TODO(phase1): start the shard — zones, gRPC Play server, directory.

	<-ctx.Done()
	slog.Info("shutting down")
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

// redact hides credentials in a DSN for logging.
func redact(dsn string) string {
	if dsn == "" {
		return ""
	}
	return "set"
}
