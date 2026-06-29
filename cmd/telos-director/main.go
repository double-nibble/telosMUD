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
	"github.com/double-nibble/telosmud/internal/director"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/obs"
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
	instanceID := directorInstanceID(cfg)
	if cfg.Redis.Addr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
		defer func() { _ = rdb.Close() }()
		claimer = directory.NewRedis(rdb, "telos")
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

	// Build + run the WORLD director. Region directors (one per region_defs) join here once region
	// content lands (10.3+); for now the world scope is the deployable. The signal HANDLER (the
	// orchestration "director script") is content-defined — not yet authored — so the director currently
	// drains + acks signals (the write/broadcast machinery is live via the API; the built-in logic plugs
	// in here when director-script content lands).
	world := director.New("", pool, slog.Default()).
		WithScopeBus(scopeBus, instanceID)
	if claimer != nil {
		world.WithElection(claimer, instanceID)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		world.Run(ctx) // returns on ctx cancel, resigning its lease
	}()

	slog.Info("starting", "env", cfg.Env, "instance", instanceID)
	<-ctx.Done()
	slog.Info("shutting down")
	wg.Wait() // let the director loop resign its lease + exit cleanly
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
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
