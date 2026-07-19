// Command telos-gate is the edge service: it terminates telnet connections (plain
// and TLS), runs the browser OAuth device login via telos-account (docs/ACCOUNT.md),
// speaks GMCP (docs/GMCP.md), and proxies players to world shards over the gRPC
// Play stream.
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
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/gate"
	"github.com/double-nibble/telosmud/internal/obs"
)

// accountAuthGate is the FAIL-CLOSED boot decision for OAuth enforcement (#96, security-review finding 1): a
// gate with NO account service configured (empty TELOS_ACCOUNT_TARGET) falls back to the bare-name login with
// no OAuth — a passwordless bypass just as complete as the (now compiled-out) TELOS_DEV_AUTOAUTH path. In a
// release build that must not happen by an accidental missing config, so refuse to boot unless
// TELOS_ALLOW_INSECURE explicitly opts in (a no-account dev/test gate). It mirrors the handoff/caller-token/
// pack-set gates' posture. Factored out so the decision is unit-testable. Returns fatal when account-less and
// not opted in; a warn when running open under the explicit opt-in; both empty when an account is wired.
func accountAuthGate(accountTarget string, allowInsecure bool) (warn string, fatal error) {
	if accountTarget != "" {
		return "", nil
	}
	if !allowInsecure {
		return "", errors.New("no account service configured (TELOS_ACCOUNT_TARGET) — the gate would accept the " +
			"UNAUTHENTICATED bare-name login (no OAuth), a passwordless bypass; set an account target, or " +
			"TELOS_ALLOW_INSECURE=1 on a trusted dev rig")
	}
	return "no account service (TELOS_ALLOW_INSECURE): the gate accepts the bare-name login with NO OAuth — " +
		"anyone who can reach it may log in as any character name", nil
}

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
	// #340: the gate READS the directory to route logins, so an evicting policy degrades it to mis-routing
	// (an evicted shard registration sends a returning player to the home zone instead of where they were)
	// rather than to corruption. Report it; never refuse — the gate is the sole player-facing entry point and
	// a boot refusal here is a total outage over state it only reads.
	gateDir := directory.NewRedis(rdb, "")
	evCtx, evCancel := context.WithTimeout(ctx, 3*time.Second)
	gateDir.CheckEvictionPolicy(evCtx)
	evCancel()
	dir := loginDirectory{
		redis:    gateDir,
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
	// Phase 14.6/15 transports: TLS telnet is the encrypted default; plain telnet is opt-in. (SSH was removed
	// in Phase 15 — auth is OAuth-only.)
	srv.WithTransports(cfg.GateAllowPlaintext, cfg.GateTLSListen, cfg.GateTLSCert, cfg.GateTLSKey)
	srv.WithDevAutoAuth(cfg.DevAutoAuth)
	srv.WithDevAutoAuthAllowRemoteBind(cfg.DevAutoAuthAllowRemoteBind) // permit a non-loopback bind ONLY in sandboxed orchestration

	srv.WithWriteTimeout(cfg.GateWriteTimeout) // Phase 16.3: bound writes so a wedged client is reclaimed
	srv.WithCommsExpected(cfg.NATS.URL != "")  // #61: warn players when a CONFIGURED comms bus is down
	// #96 (security review): a gate with no account service accepts the bare-name login with NO OAuth. Refuse
	// to boot in that state unless TELOS_ALLOW_INSECURE explicitly opts in, so a production release can't be
	// silently downgraded to passwordless login by a missing TELOS_ACCOUNT_TARGET — the same fail-closed
	// posture as the handoff / caller-token / pack-set gates.
	if warn, fatal := accountAuthGate(cfg.AccountTarget, cfg.AllowInsecure); fatal != nil {
		slog.Error("refusing to start", "err", fatal)
		os.Exit(1)
	} else if warn != "" {
		slog.Warn(warn)
	}
	// Wire the real telos-account client when an account service is configured (it drives the browser OAuth
	// device login); otherwise the gate keeps the bare-name dev stub.
	if cfg.AccountTarget != "" {
		// #247: authenticate to telos-account with the shared caller token so the gate's privileged calls are
		// accepted (and an untrusted caller's are not). Empty in dev (the account service then runs tokenless).
		ac, err := gate.DialAccount(cfg.AccountTarget, cfg.AccountCallerToken)
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

// loginDirectory adapts the Redis directory to the gate's directory.Directory seam. It is a thin
// binding: the routing POLICY (route by the placement's zone, then a legacy shard id, then the home
// zone, then the configured target) lives in internal/gate.ResolveLoginShard, where the gate's own
// integration tests can call it directly. The gate consults the directory only for the FIRST shard —
// cross-shard moves after login carry the destination address in the Redirect frame.
type loginDirectory struct {
	redis    *directory.Redis
	homeZone string
	fallback string
}

func (d loginDirectory) ShardForCharacter(characterID string) (string, bool) {
	// The policy itself lives in internal/gate (loginroute.go) so the gate's integration tests exercise
	// the SHIPPING resolver rather than a hand-synced copy of it. It bounds its own Redis I/O.
	return gate.ResolveLoginShard(context.Background(), d.redis, characterID, d.homeZone, d.fallback)
}
