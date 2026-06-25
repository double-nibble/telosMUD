// Command telos-gate is the edge service: it terminates telnet connections and
// proxies players to world shards over the gRPC Play stream. TLS/SSH, GMCP, and
// real auth arrive in later phases (docs/ACCOUNT.md, GMCP.md).
//
// Startup wiring:
//
//  1. Load config (path from env) and normalize the service name.
//  2. obs.Init installs the process-wide slog default logger and applies the
//     DEBUG env flag (DEBUG=1 lowers the level to Debug; see internal/obs).
//     From here on every package — gate, telnet, directory — just calls slog
//     and inherits this logger; none of them take a logger argument.
//  3. Resolve the initial shard via the Redis directory (ShardForZone of a home
//     zone), with a graceful fallback to the configured WorldTarget when the
//     directory or world is unreachable.
//  4. Build the gate Server (it owns a per-address Play client pool, dialed on
//     demand as players walk to new shards) and serve until SIGINT/SIGTERM.
//
// Run with DEBUG=1 to watch the edge narrate every connection end to end,
// including cross-shard redirects (re-dial target, replay count, buffer prune).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"

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

	// The directory seam: resolve the initial shard from Redis (the zone->shard map
	// shards register on boot), falling back to the configured WorldTarget so the gate
	// still serves when Redis or the home zone is absent. Re-dials during a handoff go
	// by the address the world hands the gate in a Redirect, not through here.
	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	defer func() { _ = rdb.Close() }()
	homeZone := "midgaard"
	if len(cfg.Zones) > 0 {
		homeZone = cfg.Zones[0]
	}
	dir := loginDirectory{
		redis:    directory.NewRedis(rdb, ""),
		homeZone: homeZone,
		fallback: cfg.WorldTarget,
	}

	srv := gate.New(cfg.GateListen, dir)
	slog.Info("starting", "env", cfg.Env, "listen", cfg.GateListen,
		"home_zone", homeZone, "fallback", cfg.WorldTarget)
	if err := srv.ListenAndServe(ctx); err != nil {
		slog.Error("gate serve failed", "err", err)
	}
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

// loginDirectory adapts the Redis directory to the gate's directory.Directory seam.
// On login it resolves the shard hosting the home zone (the demo's spawn zone); if
// the directory is unreachable or the zone is unregistered it falls back to the
// configured world target, so a single-shard dev stack still works without Redis.
// The gate only consults the directory for the FIRST shard — cross-shard moves carry
// the destination address in the Redirect frame.
type loginDirectory struct {
	redis    *directory.Redis
	homeZone string
	fallback string
}

func (d loginDirectory) ShardForCharacter(characterID string) (string, bool) {
	ctx := context.Background()
	// Two hops: which shard owns the home zone, then where that shard is reachable.
	shardID, err := d.redis.ShardForZone(ctx, d.homeZone)
	if err == nil && shardID != "" {
		if endpoint, eerr := d.redis.EndpointForShard(ctx, shardID); eerr == nil && endpoint != "" {
			slog.Debug("login directory resolved",
				"component", "gate", "character", characterID,
				"home_zone", d.homeZone, "shard_id", shardID, "endpoint", endpoint)
			return endpoint, true
		} else {
			err = eerr
		}
	}
	slog.Debug("login directory fallback",
		"component", "gate", "character", characterID,
		"home_zone", d.homeZone, "shard_id", shardID, "err", err, "fallback", d.fallback)
	if d.fallback == "" {
		return "", false
	}
	return d.fallback, true
}
