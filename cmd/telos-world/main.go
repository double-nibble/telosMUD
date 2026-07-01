// Command telos-world runs a world-simulation shard: the zone actor loop plus the
// gRPC Play server. Phase 1 serves one hardcoded two-room zone (docs/ROADMAP.md).
//
// Startup order: load config -> obs.Init (installs the slog default logger; honors
// DEBUG=1 to enable Debug-level world tracing) -> start the zone actor goroutine ->
// serve gRPC. SIGINT/SIGTERM triggers a graceful DRAIN (Phase 16.4c): every zone +
// its live players are handed off to a peer shard (sockets stay open) BEFORE the zone
// loops stop, then the gRPC server is drained.
package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"github.com/double-nibble/telosmud/db"
	"github.com/double-nibble/telosmud/internal/assertion"
	"github.com/double-nibble/telosmud/internal/checkpoint"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/obs"
	"github.com/double-nibble/telosmud/internal/placement"
	"github.com/double-nibble/telosmud/internal/presence"
	"github.com/double-nibble/telosmud/internal/scopebus"
	"github.com/double-nibble/telosmud/internal/sessionlock"
	"github.com/double-nibble/telosmud/internal/store"
	"github.com/double-nibble/telosmud/internal/world"
)

// enabledPacks is the content packs a world shard loads. v1 ships only the demo pack;
// strip-the-stdlib / per-deploy pack selection (a config field) is a later concern.
var enabledPacks = []string{content.DemoPack}

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

	// SIGINT/SIGTERM triggers a graceful DRAIN; the zone/lease lifetime (worldCtx) is deliberately SEPARATE
	// so the drain runs while the zones are still LIVE (the flush + handoff must precede the zone loops
	// stopping — PERSISTENCE.md §6), then worldCtx is cancelled to stop them. A lease-loss FENCE (onFence =
	// stopWorld, passed to buildShard) cancels worldCtx too — that path stops immediately WITHOUT a drain
	// (we lost the lease, so we can't hand our zones off).
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	worldCtx, stopWorld := context.WithCancel(context.Background())
	defer stopWorld()

	// Build the shard. With Redis reachable, register every zone this shard hosts in
	// the directory and wire cross-shard handoff; otherwise fall back to a single-shard
	// world whose cross-shard exits are sealed (so a bare run still works).
	zones := cfg.Zones
	if len(zones) == 0 {
		zones = []string{"midgaard"}
	}
	shard, chooseTarget := buildShard(worldCtx, stopWorld, cfg, zones)
	go shard.Run(worldCtx) // each zone actor loop owns its world state from here on

	lis, err := net.Listen("tcp", cfg.WorldListen)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.WorldListen, "err", err)
		os.Exit(1)
	}
	gs := grpc.NewServer()
	shard.Register(gs)

	go func() {
		select {
		case <-sigCtx.Done():
			// Graceful drain (Phase 16.4c): hand every zone + its live players off to a peer over the
			// cross-shard handoff (sockets stay open — zero dropped connections) BEFORE the zones stop.
			// Bounded by the drain timeout so a stuck handoff can't hang shutdown forever.
			slog.Info("signal received: draining before shutdown")
			if chooseTarget != nil {
				dctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				res, derr := shard.BeginDrain(dctx, chooseTarget, 30*time.Second)
				cancel()
				if derr != nil {
					slog.Warn("graceful drain incomplete; stopping anyway", "err", derr)
				} else {
					slog.Info("drain complete", "redirected", res.Redirected, "reclaimed", res.Reclaimed)
				}
			} else {
				shard.Drain() // single-shard: no peer to hand off to; best-effort durable flush
			}
		case <-worldCtx.Done():
			slog.Warn("lease fence: stopping without drain")
		}
		stopWorld()       // stop the zone + saver goroutines (state already flushed / handed off)
		gs.GracefulStop() // then drain the gRPC server and let Serve return
	}()

	slog.Info("starting", "env", cfg.Env, "listen", cfg.WorldListen)
	if err := gs.Serve(lis); err != nil {
		slog.Error("serve failed", "err", err)
	}
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

// buildShard wires the world shard. With Redis reachable it registers EVERY zone this
// shard hosts in the directory, claims a lease on each, and enables cross-shard
// handoff; otherwise it logs a warning and returns a single-shard world whose
// cross-shard exits are sealed (so a bare run without backing services still works).
// home (the spawn zone for fresh logins) is the first configured zone.
func buildShard(ctx context.Context, stop func(), cfg config.Config, zones []string) (*world.Shard, world.TargetChooser) {
	home := zones[0]

	// Session-assertion verify key (Phase 14.3): account's Ed25519 PUBLIC key, if configured. The shard then
	// verifies the gate's assertion offline on a fresh-login Attach. An invalid key is fatal (a
	// misconfiguration that would silently disable auth); an absent key means assertions are NOT enforced.
	var verifyKey ed25519.PublicKey
	if cfg.AccountVerifyKey != "" {
		pk, err := assertion.ParsePublicKey(cfg.AccountVerifyKey)
		if err != nil {
			slog.Error("invalid account verify key", "err", err)
			os.Exit(1)
		}
		verifyKey = pk
		slog.Info("session-assertion verification enabled (ed25519)")
	}

	// Load content BEFORE building any zone (docs/PHASE4-PLAN.md §3). This is synchronous boot
	// I/O on the construction goroutine — never on a zone goroutine — so blocking is fine. If
	// Postgres is unreachable the shard boots EMPTY (the bare-engine invariant), exactly as it
	// degrades to single-shard when Redis is down. The demo world lives only in the DB/YAML, so
	// nothing demo is compiled into this path.
	lc := loadContent(ctx, cfg)

	// Open the long-lived Postgres pool (slice 4.2 character ladder + slice 4.3 hot-reload re-read).
	// It is OPTIONAL: if Postgres is unreachable it stays nil and characters are EPHEMERAL and hot
	// reload is DISABLED (today's behavior) — the bare-engine boot degrades, never crashes. Separate
	// from the content-load pool above (which is closed after the boot load); this one lives for the
	// shard's lifetime. The same *store.Pool serves both world.CharacterStore and
	// content.DefinitionSource, so the hot-reload single-ref re-read reuses it.
	livePool := openLivePool(ctx, cfg)
	var charStore world.CharacterStore
	var defSource content.DefinitionSource
	var mailStore world.MailStore
	if livePool != nil {
		charStore = livePool
		defSource = livePool
		mailStore = livePool // Phase 8.7: the same pool backs the durable mail inbox (nil => mail disabled)
	}

	// Optional content bus for hot reload (slice 4.3). NATS unreachable => hot reload DISABLED
	// (boot-load still works); never fatal, exactly like Redis/Postgres being down.
	bus := openContentBus(cfg)

	// Optional comms bus (Phase 8.3): the world is the comms SOURCE — it publishes channel lines through
	// a RoleWorld commbus handle (commbus.OpenWorld ONLY — never OpenGate). NATS unreachable => a Disabled
	// no-op bus => channels degrade to "temporarily offline" for the speaker, never a boot failure
	// (the never-fatal rule, mirroring openContentBus + the gate's commbus.OpenGate).
	comms := commbus.OpenWorld(cfg.NATS.URL, func(err error) {
		if err != nil {
			slog.Warn("nats unavailable; comms disabled (world source)", "url", cfg.NATS.URL, "err", err)
			return
		}
		slog.Info("comms bus ready (world source)", "url", cfg.NATS.URL)
	})

	// Optional scoped event bus (Phase 10.3b/c): the world SUBSCRIBES to the region/world scopes so a
	// director's state broadcast updates each hosted zone's read-replica (world.flag/region:get), and it
	// SIGNALS UP (signal_region/signal_world) on the durable tier so a state-changing report survives a
	// blip. The transient half rides the comms transport; the durable half is the WORLD_EVENTS JetStream
	// stream (OpenScopeJetStream — Disabled if NATS is down, so signal-up degrades, never a boot failure).
	// source is run-unique (shard id + a random suffix) so the per-process idempotency keys never collide
	// with a prior run's. lc.Regions is the region_defs membership the shard derives zone→region from.
	scopeJS := commbus.OpenScopeJetStream(cfg.NATS.URL, func(err error) {
		if err != nil {
			slog.Warn("scope jetstream unavailable; durable signal-up disabled", "url", cfg.NATS.URL, "err", err)
			return
		}
		slog.Info("scope event stream ready", "url", cfg.NATS.URL)
	})
	scopeSource := "world-" + cfg.ShardID + "-" + uuid.NewString()[:8]
	scopeBus := scopebus.New(comms).WithDurable(scopeJS, scopeSource)

	// Optional DURABLE-tell transport (Phase 8.5, OQ-1 durable-always): the world PublishDurable's every
	// tell here and runs a per-resident durable consumer. JetStream unreachable => DisabledJetStream =>
	// tells degrade to "temporarily offline", never a boot failure (the never-fatal rule, mirroring the
	// comms bus). Same TELOS_NATS_URL — JetStream rides the same broker.
	tellJS := commbus.OpenJetStream(cfg.NATS.URL, func(err error) {
		if err != nil {
			slog.Warn("jetstream unavailable; durable tells disabled", "url", cfg.NATS.URL, "err", err)
			return
		}
		slog.Info("durable tell stream ready", "url", cfg.NATS.URL)
	})

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		slog.Warn("redis unavailable; single-shard mode (cross-shard exits sealed)",
			"addr", cfg.Redis.Addr, "err", err)
		// Redis down: no checkpoint tier, but the Postgres tier (if up) still gives save-on-logout
		// durability — a character survives a restart, just with a wider crash window.
		// No directory (single-shard) => no peer to drain onto => nil chooser (BeginDrain degrades to a flush).
		return world.NewShardFromContent(lc, zones, home, "", nil, nil).
			WithPersistence(charStore, nil).
			WithHotReload(defSource, bus, enabledPacks).
			WithComms(comms).
			WithVerifyKey(verifyKey).
			WithScopeBus(scopeBus, lc.Regions).
			WithMail(mailStore).
			WithTells(tellJS), nil
	}
	dir := directory.NewRedis(rdb, "telos")
	ckpt := checkpoint.NewRedis(rdb, "telos") // ~10s Redis checkpoint tier of the ladder
	// Cross-shard `who` roster (Phase 8.4): the same Redis the directory uses, namespaced "<ns>:presence:*"
	// so it never collides with the directory's "<ns>:dir:*". Each shard writes ONLY its own residents
	// (write authority keyed by cfg.ShardID — P8-A4) and `who` reads the whole roster. Operational/ephemeral
	// (PERSISTENCE.md Redis tier); a crashed shard's players age out via the TTL.
	roster := presence.NewRedis(rdb, "telos")

	// Publish where THIS shard is reachable (shard-id -> endpoint) BEFORE claiming any
	// zone, so the moment a zone names us as owner, peers can resolve us to a live
	// address. Then heartbeat it and drop it on shutdown.
	if err := dir.RegisterShard(ctx, cfg.ShardID, cfg.ShardAddr, directory.DefaultShardLease); err != nil {
		slog.Error("shard registration failed", "shard_id", cfg.ShardID, "err", err)
		os.Exit(1)
	}
	slog.Info("registered shard", "shard_id", cfg.ShardID, "endpoint", cfg.ShardAddr, "lease", directory.DefaultShardLease)
	go renewShardRegistration(ctx, dir, cfg.ShardID, cfg.ShardAddr) //nolint:gosec // G118: ctx is the shard's main lifetime ctx (cancelled on shutdown) — exactly what this renewal goroutine should follow.

	// CLAIM-FROM-POOL (docs/PLACEMENT.md §4, Phase 10.6a): this server no longer DECLARES a fixed zone
	// set — it CLAIMS what is free from the pool (the configured `zones`) via the directory's time-fenced
	// CAS. It hosts exactly what it WINS; a zone already owned by another live shard is simply skipped
	// (normal in a fleet, not a misconfiguration — the two-writer guard is the shard-id RegisterShard
	// check above, not the zone claim). A server that wins NOTHING (a saturated fleet) runs as a STANDBY:
	// registered + heartbeating, hosting no zone, ready to take an orphan on the next failure. Decentralized
	// LIVENESS: this works with no director running. (Live re-claim of an orphan by a standby needs runtime
	// zone-add — the documented next step; boot-time claim is the slice here.)
	won, claimErrs := placement.ClaimFromPool(ctx, dir, cfg.ShardID, zones, directory.DefaultZoneLease)
	for zoneID, cerr := range claimErrs {
		slog.Warn("zone claim error; skipped (left for another server / a retry)", "zone", zoneID, "err", cerr)
	}
	for _, zoneID := range won {
		slog.Info("claimed zone", "zone", zoneID, "shard_id", cfg.ShardID, "lease", directory.DefaultZoneLease)
	}
	// Zone-lease RENEWAL now lives in the shard (WithZoneLeasing below), not per-zone goroutines here — so a
	// graceful drain can hand a zone's lease to a peer without the source's renewal fencing the whole shard
	// (Phase 16.4b). The shard renews every hosted zone (boot + runtime-adopted) and fences via stop on an
	// UNEXPECTED lease loss, releasing on clean shutdown — the same contract the old renewZoneLease had.
	if len(won) == 0 {
		slog.Warn("won no zones from the pool: running as a STANDBY (registered, hosting nothing, ready to take over)", "pool", zones)
	}
	// The spawn home must be a zone this server actually hosts: keep the configured home if won, else the
	// first won zone. A standby (won nothing) keeps the configured home unhosted — no login lands here.
	hostHome := home
	if len(won) > 0 && !contains(won, home) {
		hostHome = won[0]
	}

	// chooseDrainTarget selects a live PEER shard to hand this shard's zones + players to on a graceful drain
	// (Phase 16.4c). Decentralized self-select: the first live shard in the directory that isn't us. Good for
	// a single rolling redeploy (the replacement/standby is the peer); director-owned load-aware selection +
	// serialization of simultaneous drains is the documented follow-up.
	chooseDrainTarget := func(string) (string, string, error) {
		lctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shards, err := dir.ListShards(lctx)
		if err != nil {
			return "", "", err
		}
		for _, id := range shards {
			if id == cfg.ShardID {
				continue
			}
			addr, err := dir.EndpointForShard(lctx, id)
			if err != nil {
				continue // registration lapsed between the list and the resolve
			}
			return id, addr, nil
		}
		return "", "", fmt.Errorf("no live peer shard to drain onto")
	}

	return world.NewShardFromContent(lc, won, hostHome, cfg.ShardAddr, dir, world.GRPCDialer()).
		WithPersistence(charStore, ckpt).
		WithHotReload(defSource, bus, enabledPacks).
		WithComms(comms).
		WithVerifyKey(verifyKey).
		WithSessionLock(sessionlock.NewRedis(rdb), 0, 0). // Phase 14.4: cross-shard single-session lock (Redis)
		WithScopeBus(scopeBus, lc.Regions).
		WithZoneLeasing(dir, cfg.ShardID, directory.DefaultZoneLease, directory.DefaultZoneLease/3, stop).
		WithPresence(roster, cfg.ShardID).
		WithMail(mailStore).
		WithTells(tellJS), chooseDrainTarget
}

// openLivePool opens the long-lived Postgres pool the shard keeps for its lifetime: it backs both
// the durable character store (the async saver + login read) and the hot-reload single-ref re-read
// (content.DefinitionSource). It is OPTIONAL and never fatal: an unreachable database returns nil,
// so characters stay ephemeral AND hot reload is disabled (the bare-engine degradation) rather than
// the world failing to boot. Returns the concrete *store.Pool (nil on failure) so the caller can
// keep the CharacterStore and DefinitionSource interfaces truly nil when there is no pool — a typed
// nil in an interface would be non-nil and defeat the disabled-fallback guards.
func openLivePool(ctx context.Context, cfg config.Config) *store.Pool {
	pool, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		slog.Warn("postgres unavailable; characters ephemeral and hot reload disabled", "err", err)
		return nil
	}
	slog.Info("postgres ready (durable characters + hot reload)")
	return pool
}

// openContentBus connects the content hot-reload bus (NATS). OPTIONAL and never fatal: an
// unreachable broker returns a nil Bus, so hot reload is simply disabled (boot-load still works).
// Returns the contentbus.Bus interface so the disabled path is a true nil interface (a typed nil
// *NATSBus would be non-nil and slip past WithHotReload's nil guard).
func openContentBus(cfg config.Config) contentbus.Bus {
	bus, err := contentbus.Connect(cfg.NATS.URL)
	if err != nil {
		slog.Warn("nats unavailable; content hot reload disabled", "url", cfg.NATS.URL, "err", err)
		return nil
	}
	slog.Info("content bus ready (hot reload enabled)", "url", cfg.NATS.URL)
	return bus
}

// loadContent reads the enabled packs from Postgres, optionally running migrations first
// (opt-in via TELOS_DB_AUTOMIGRATE, advisory-locked so multi-shard boots serialize). If the
// database is unreachable it logs a warning and returns EMPTY content — the engine boots with
// zero rooms (bare-engine invariant), and a login is rejected cleanly rather than panicking.
// Postgres is the production source; the embedded YAML pack is the unit-test/dev source.
func loadContent(ctx context.Context, cfg config.Config) *content.LoadedContent {
	if db.AutoMigrateEnabled() {
		if err := db.Migrate(ctx, cfg.Postgres.DSN); err != nil {
			slog.Warn("auto-migrate failed; continuing (boot may be empty)", "err", err)
		} else {
			slog.Info("auto-migrate applied", "guard", db.AutoMigrateEnv)
		}
	}
	pool, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		slog.Warn("postgres unavailable; booting empty world (bare-engine)", "err", err)
		empty, _ := content.Load(ctx, nil, nil)
		return empty
	}
	defer pool.Close()
	lc, err := content.Load(ctx, pool, enabledPacks)
	if err != nil {
		slog.Warn("content load failed; booting empty world", "err", err)
		empty, _ := content.Load(ctx, nil, nil)
		return empty
	}
	if lc.Empty() {
		slog.Warn("no content loaded (packs absent in DB?); booting empty world", "packs", enabledPacks)
	} else {
		slog.Info("content loaded from postgres", "packs", enabledPacks, "zones", len(lc.Zones))
	}
	return lc
}

// renewShardRegistration heartbeats this shard's id->endpoint registration until ctx is
// cancelled, then deregisters it so peers stop resolving a dead address immediately.
// Losing the registration is not fatal (unlike a zone lease): the next tick re-publishes
// it; it only needs to stay live so zone owners remain dialable.
func renewShardRegistration(ctx context.Context, dir *directory.Redis, shardID, endpoint string) {
	t := time.NewTicker(directory.DefaultShardLease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = dir.DeregisterShard(rctx, shardID, endpoint)
			cancel()
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := dir.RegisterShard(rctx, shardID, endpoint, directory.DefaultShardLease)
			cancel()
			if err != nil {
				slog.Warn("shard registration renewal error", "shard_id", shardID, "err", err)
			}
		}
	}
}

// contains reports whether s is in xs.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
