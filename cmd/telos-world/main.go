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
	// Fail loud on an unauthenticated multi-shard deployment (#251): a shard that can receive cross-shard
	// handoffs MUST have a handoff verify key, or a forged Prepare could inject carried state. Refuse to boot
	// unless TELOS_ALLOW_INSECURE explicitly opts in (a trusted local multi-node rig, or a Redis-backed single
	// node) — the same fail-closed-by-default posture as the account caller token. A Redis-backed SINGLE-node
	// prod deploy is discoverable, so it correctly requires the keypair; generate one (ops).
	if err := shard.CheckHandoffAuth(); err != nil {
		if !cfg.AllowInsecure {
			slog.Error("refusing to start", "err", err)
			os.Exit(1)
		}
		slog.Warn("insecure handoffs (TELOS_ALLOW_INSECURE): a discoverable shard has no handoff verify key", "err", err)
	}
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

	// Cross-shard handoff keypair (docs/REMAINING.md §1): the shared cluster Ed25519 keys this shard signs
	// outgoing Prepares with and verifies incoming ones against. Invalid keys are fatal (a misconfiguration
	// that would silently disable handoff authentication); absent keys mean handoff signing is NOT enforced.
	var handoffSignKey ed25519.PrivateKey
	var handoffVerifyKey ed25519.PublicKey
	if cfg.HandoffSigningKey != "" {
		sk, err := assertion.ParsePrivateKey(cfg.HandoffSigningKey)
		if err != nil {
			slog.Error("invalid handoff signing key", "err", err)
			os.Exit(1)
		}
		handoffSignKey = sk
	}
	if cfg.HandoffVerifyKey != "" {
		pk, err := assertion.ParsePublicKey(cfg.HandoffVerifyKey)
		if err != nil {
			slog.Error("invalid handoff verify key", "err", err)
			os.Exit(1)
		}
		handoffVerifyKey = pk
	}
	switch {
	case handoffSignKey != nil && handoffVerifyKey != nil:
		slog.Info("cross-shard handoff authentication enabled (ed25519)")
	case handoffSignKey != nil || handoffVerifyKey != nil:
		// Exactly one half configured. This is legal (asymmetric test shards) but in a real cluster it is
		// almost certainly a misconfiguration: a signing-only shard leaves its RECEIVE side accepting
		// unsigned/forged Prepares, and a verify-only shard cannot hand its own players off. Warn loudly.
		slog.Warn("cross-shard handoff auth is HALF-configured — set BOTH handoff_signing_key and handoff_verify_key",
			"have_signing", handoffSignKey != nil, "have_verify", handoffVerifyKey != nil)
	}

	// Load content BEFORE building any zone (docs/PHASE4-PLAN.md §3). This is synchronous boot
	// I/O on the construction goroutine — never on a zone goroutine — so blocking is fine. If
	// Postgres is unreachable the shard boots EMPTY (the bare-engine invariant), exactly as it
	// degrades to single-shard when Redis is down. The demo world lives only in the DB/YAML, so
	// nothing demo is compiled into this path.
	lc, enabledPacks, bootContentVersion := loadContent(ctx, cfg)

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
		// When the configured home has no rooms (bare/unseeded server) host the embedded core bootstrap
		// zone locally and spawn logins there, so a fresh server still accepts logins (#212).
		hostZones, hostHome, _ := resolveHosting(lc, zones, home)
		return world.NewShardFromContent(lc, hostZones, hostHome, "", nil, nil).
			WithLocalZones(content.CoreZone).
			WithPersistence(charStore, nil).
			WithHotReload(defSource, bus, enabledPacks, bootContentVersion).
			WithComms(comms).
			WithVerifyKey(verifyKey).
			WithHandoffKeys(handoffSignKey, handoffVerifyKey).
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
	// The core bootstrap zone is hosted LOCALLY + unleased on every shard (below), so it must never
	// be claimed from the pool — a lease we then never renew (WithLocalZones skips renewal) would
	// leave a stale ShardForZone(core) owner in the directory. Strip it defensively in case an
	// operator lists it in cfg.Zones (#212).
	won, claimErrs := placement.ClaimFromPool(ctx, dir, cfg.ShardID, withoutCoreZone(zones), directory.DefaultZoneLease)
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
	preferredHome := home
	if len(won) > 0 && !contains(won, home) {
		preferredHome = won[0]
	}
	// When the preferred home has no rooms (unseeded/empty content) host the embedded core bootstrap
	// zone LOCALLY (unleased) and spawn logins there (#212) — so even a standby serves the lobby in a
	// fresh, content-less fleet (the fresh-deploy case). When real content exists the preferred home is
	// kept verbatim (even if this shard doesn't host it yet — a standby's later adoption then spawns
	// logins correctly), and the core lobby is NOT hosted at all: no unvisited extra zone, and s.home
	// is never repointed to the lobby.
	hostZones, hostHome, _ := resolveHosting(lc, won, preferredHome)

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

	return world.NewShardFromContent(lc, hostZones, hostHome, cfg.ShardAddr, dir, world.GRPCDialer()).
		WithLocalZones(content.CoreZone). // the bootstrap zone is hosted unleased on every shard (#212)
		WithPersistence(charStore, ckpt).
		WithHotReload(defSource, bus, enabledPacks, bootContentVersion).
		WithComms(comms).
		WithVerifyKey(verifyKey).
		WithHandoffKeys(handoffSignKey, handoffVerifyKey).
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
func loadContent(ctx context.Context, cfg config.Config) (*content.LoadedContent, []string, uint64) {
	if db.AutoMigrateEnabled() {
		if err := db.Migrate(ctx, cfg.Postgres.DSN); err != nil {
			slog.Warn("auto-migrate failed; continuing (boot may be empty)", "err", err)
		} else {
			slog.Info("auto-migrate applied", "guard", db.AutoMigrateEnv)
		}
	}
	pool, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		// Postgres down: still boot the embedded core pack alone (#212) so a fresh/empty server has
		// a bootstrap start room and a builder can connect, rather than an empty, login-rejecting world.
		slog.Warn("postgres unavailable; booting bootstrap-only world (embedded core pack)", "err", err)
		core, _ := content.LoadWithCore(ctx, nil, nil)
		return core, nil, 0
	}
	defer pool.Close()
	// Manifest-driven pack selection (#212 slice 4): serve the packs the currently imported version
	// registered (content_pack_registry), unless the operator pins an explicit set. A read failure
	// (fresh DB before the 00024 migration, or a transient error) degrades to the demo/override default.
	// The version is read HERE — before the packs below — so the boot content version is never AHEAD of
	// the loaded content (reconcile-on-join, PR D: a pull racing boot then fails safe, never a miss).
	var registryPacks []string
	var bootVersion uint64
	if info, verr := pool.CurrentContentVersion(ctx); verr == nil {
		registryPacks = info.Packs
		bootVersion = info.Version
	} else {
		slog.Debug("content version registry unavailable; using configured/default packs", "err", verr)
	}
	enabledPacks := content.ResolveEnabledPacks(cfg.ContentPacks, registryPacks)

	// LoadWithCore layers the minimal embedded core pack UNDER the real packs read from Postgres, so
	// the bootstrap zone is ALWAYS present; real content overrides it by ref (#212).
	lc, err := content.LoadWithCore(ctx, pool, enabledPacks)
	if err != nil {
		slog.Warn("content load failed; booting bootstrap-only world (embedded core pack)", "err", err)
		core, _ := content.LoadWithCore(ctx, nil, nil)
		return core, nil, 0
	}
	// lc always carries the core zone now, so lc.Empty() is never true; report on the REAL content.
	if realZones := len(lc.Zones) - 1; realZones <= 0 {
		slog.Warn("no real content loaded (packs absent in DB?); booting bootstrap-only world (embedded core pack)", "packs", enabledPacks)
	} else {
		slog.Info("content loaded from postgres", "packs", enabledPacks, "zones", realZones, "content_version", bootVersion)
	}
	return lc, enabledPacks, bootVersion
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

// withCoreZone returns the hosted zone set with the embedded core bootstrap zone appended (#212).
// A shard hosts it LOCALLY (unleased); it is a no-op if already present.
func withCoreZone(zoneIDs []string) []string {
	if contains(zoneIDs, content.CoreZone) {
		return zoneIDs
	}
	return append(append([]string(nil), zoneIDs...), content.CoreZone)
}

// resolveHosting decides the hosted zone set + fresh-login home for a shard, given the zones it
// hosts (bare: the configured set; fleet: what it won) and the PREFERRED home. When that home zone
// has rooms (real content present), it is kept verbatim — even if this shard does not host it yet
// (a standby keeps the real home so a later adoption spawns logins correctly). ONLY when the home
// has no rooms (a fresh/empty server) does the shard host the embedded core bootstrap lobby locally
// and spawn logins there. So the core zone is hosted exactly in the bootstrap case, never as an
// unvisited extra zone when real content exists (#212). The bool reports whether core is hosted.
func resolveHosting(lc *content.LoadedContent, hosted []string, preferredHome string) (zones []string, home string, coreHosted bool) {
	if zonePopulated(lc, preferredHome) {
		return hosted, preferredHome, false
	}
	return withCoreZone(hosted), content.CoreZone, true
}

// withoutCoreZone returns zoneIDs with the core bootstrap zone removed — the pool of zones a shard
// may CLAIM a directory lease on. Core is hosted locally + unleased (#212), never leased, so it
// must not enter the claim pool even if an operator mislists it in cfg.Zones.
func withoutCoreZone(zoneIDs []string) []string {
	out := make([]string, 0, len(zoneIDs))
	for _, z := range zoneIDs {
		if z != content.CoreZone {
			out = append(out, z)
		}
	}
	return out
}

// zonePopulated reports whether lc carries the named zone WITH at least one room. A configured home
// zone that is absent/empty (unseeded content) is not a viable spawn — buildShard then falls the
// login home back to the core bootstrap zone (#212).
func zonePopulated(lc *content.LoadedContent, zoneRef string) bool {
	if lc == nil {
		return false
	}
	z := lc.Zone(zoneRef)
	return z != nil && len(z.Rooms) > 0
}
