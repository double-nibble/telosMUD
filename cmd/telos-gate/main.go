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

	"github.com/double-nibble/telosmud/internal/commbus"
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

	// The Phase-8 comms bus (P8-D1-B: the gate is the comms SINK). It is opened via
	// commbus.OpenGate ONLY — NEVER OpenWorld and NEVER the test-only MemBus.WorldHandle():
	// a gate is structurally subscribe-only on chan/tell subjects (the publish ACL / the
	// impersonation gate, P8-A2). A gate handed a world handle would let a forged client author
	// a channel/tell line, defeating the whole boundary. Optional/never-fatal, mirroring
	// cmd/telos-world's openContentBus: an unreachable broker (or an empty URL) yields a Disabled
	// RoleGate no-op so comms degrade to unavailable, never a boot failure.
	comms := commbus.OpenGate(cfg.NATS.URL, func(err error) {
		if err != nil {
			slog.Warn("nats unavailable; comms disabled (gate)", "url", cfg.NATS.URL, "err", err)
			return
		}
		slog.Info("comms bus ready (gate sink)", "url", cfg.NATS.URL)
	})
	defer func() { _ = comms.Close() }()

	srv := gate.New(cfg.GateListen, dir, comms)
	// Phase 14.6 transports: TLS is the encrypted default; plain telnet is opt-in. The SSH transport (14.6b)
	// is configured here too once it lands.
	srv.WithTransports(cfg.GateAllowPlaintext, cfg.GateTLSListen, cfg.GateTLSCert, cfg.GateTLSKey)
	srv.WithSSH(cfg.GateSSHListen, cfg.GateSSHHostKey)
	// Phase 14: wire the real telos-account client when an account service is configured; otherwise the gate
	// keeps the stub "type a name" login. The login flow that USES it lands in 14.2 (link codes).
	if cfg.AccountTarget != "" {
		ac, err := gate.DialAccount(cfg.AccountTarget)
		if err != nil {
			slog.Error("account dial failed", "target", cfg.AccountTarget, "err", err)
			os.Exit(1)
		}
		defer func() { _ = ac.Close() }()
		srv.WithAccountClient(ac)
		slog.Info("account service wired", "target", cfg.AccountTarget)
	}
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
	// FIRST consult the per-character placement: the directory is the authoritative ownership
	// record (the handoff CAS writes it, and it PERSISTS across logout — ClearPlayer is deferred).
	// A character who was handed off to another shard and logged out there must reconnect to THAT
	// shard, not to the fixed home zone — otherwise a fresh login routed back to the home shard
	// rehydrates the player at a room the home shard cannot place (the cross-shard reconnect bug).
	// Falls through to the home-zone resolution when the player has no placement yet (brand-new, or
	// never handed off), so a first-ever login is unchanged. Resolving the placement's shard id to
	// an endpoint reuses the same id->endpoint hop as the home-zone path.
	if place, perr := d.redis.PlayerPlacement(ctx, characterID); perr == nil && place.ShardID != "" {
		if endpoint, eerr := d.redis.EndpointForShard(ctx, place.ShardID); eerr == nil && endpoint != "" {
			slog.Debug("login directory resolved by placement",
				"component", "gate", "character", characterID,
				"shard_id", place.ShardID, "epoch", place.Epoch, "endpoint", endpoint)
			return endpoint, true
		}
	}
	// Fallback: route to the home zone's shard. This is correct ONLY while ClearPlayer is deferred
	// (placement persists across logout, so a returning handed-off player resolves via the branch
	// above and never reaches here). COUPLED INVARIANT — do not decouple: if a future slice wires
	// ClearPlayer-on-quit, a returning player will have no placement and fall through HERE, and this
	// fallback MUST then resolve the player's DURABLE zone_ref (load the row, route to whoever owns
	// that zone), not the fixed home zone — otherwise it reintroduces the cross-shard reconnect bug
	// for anyone who quit outside their home zone. (distsys review, 6.3a handoff fix.)
	// Two hops: which shard owns the home zone, then where that shard is reachable.
	shardID, err := d.redis.ShardForZone(ctx, d.homeZone)
	if err == nil && shardID != "" {
		endpoint, eerr := d.redis.EndpointForShard(ctx, shardID)
		if eerr == nil && endpoint != "" {
			slog.Debug("login directory resolved",
				"component", "gate", "character", characterID,
				"home_zone", d.homeZone, "shard_id", shardID, "endpoint", endpoint)
			return endpoint, true
		}
		err = eerr
	}
	slog.Debug("login directory fallback",
		"component", "gate", "character", characterID,
		"home_zone", d.homeZone, "shard_id", shardID, "err", err, "fallback", d.fallback)
	if d.fallback == "" {
		return "", false
	}
	return d.fallback, true
}
