// Command telos-account runs the accounts/auth service (docs/ACCOUNT.md, Phase 14). It is the only service
// that touches OAuth providers + credentials; telos-gate calls its Account gRPC API to redeem link codes,
// verify passphrases, resolve SSH keys, and list/create characters. The world never calls it on the hot path
// — it trusts the signed session assertion (§9). The deployable fourth-and-a-half alongside gate/world/
// director; the website (14.7) attaches to this same service.
//
// Startup: load config -> obs.Init -> open the Postgres store (the account/character tables) -> serve the
// Account gRPC API. SIGINT/SIGTERM gracefully stops the server.
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

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/account"
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/obs"
	"github.com/double-nibble/telosmud/internal/store"
)

func main() {
	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	if cfg.Service == "telos" {
		cfg.Service = "telos-account"
	}
	shutdown := obs.Init(cfg.Service, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The account/character tables are the service's reason for being — without a DSN there is nothing to
	// authenticate against, so this is fatal.
	if cfg.Postgres.DSN == "" {
		slog.Error("telos-account needs a Postgres DSN (accounts have no durable home without it)")
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

	// A freshly-created character starts in the demo pack's start room (Phase 14.8 may let content choose).
	svc := account.New(pool, slog.Default(), "midgaard", "midgaard:room:temple")

	// Link codes (Phase 14.2) live in Redis (cross-process + native TTL). Without Redis the service still
	// boots, but Mint/RedeemLinkCode return Unavailable (the website's Play bridge needs a code store).
	if cfg.Redis.Addr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
		defer func() { _ = rdb.Close() }()
		svc.WithLinkCodes(account.NewRedisLinkCodes(rdb))
		slog.Info("link codes enabled (redis)", "addr", cfg.Redis.Addr)
	} else {
		slog.Warn("no Redis configured: link codes disabled (Mint/RedeemLinkCode unavailable)")
	}

	lis, err := net.Listen("tcp", cfg.AccountListen)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.AccountListen, "err", err)
		os.Exit(1)
	}
	gs := grpc.NewServer()
	accountv1.RegisterAccountServer(gs, svc)

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		gs.GracefulStop()
	}()

	slog.Info("starting", "env", cfg.Env, "listen", cfg.AccountListen)
	if err := gs.Serve(lis); err != nil {
		slog.Error("serve failed", "err", err)
	}
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}
