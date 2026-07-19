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
	"errors"
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
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentpull"
	"github.com/double-nibble/telosmud/internal/director"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/obs"
	"github.com/double-nibble/telosmud/internal/placement"
	"github.com/double-nibble/telosmud/internal/presence"
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

	// #368: same fail-closed treatment as telos-world. The director runs the world-director script (#47) in
	// its own luasandbox Runtime, so the same caps apply to it.
	if err := cfg.Tunables.Err(); err != nil {
		slog.Error("refusing to start", "err", err)
		os.Exit(1)
	}
	if err := director.SetLuaCaps(cfg.Tunables.LuaInstrBudget, cfg.Tunables.LuaCallDeadlineMS); err != nil {
		slog.Error("refusing to start", "err", err)
		os.Exit(1)
	}

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
	var dir *directory.Redis                   // the fleet view the placement coordinator watches (nil without Redis)
	var rosterSrc director.ChannelRosterSource // the cross-shard presence roster the #90 aggregator reads (nil without Redis)
	instanceID := directorInstanceID(cfg)
	if cfg.Redis.Addr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
		defer func() { _ = rdb.Close() }()
		dir = directory.NewRedis(rdb, "telos")
		// #340: report an evicting directory Redis. The director has the most to lose here — its own
		// LEADER-ELECTION lease is a TTL'd key, and evicting it lets two directors both believe they lead —
		// so it is the tier most worth telling. Warn-only; see CheckEvictionPolicy for why not fatal.
		evCtx, evCancel := context.WithTimeout(ctx, 3*time.Second)
		dir.CheckEvictionPolicy(evCtx)
		evCancel()
		rosterSrc = presence.NewRedis(rdb, "telos") // #90: same Redis + namespace as the who roster
		claimer = dir
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

	// Load the scheduled-spawn definitions (Phase 12.4): the director owns the long-timer boss schedules
	// (a weekly world boss). It loads them from content (Postgres) at boot and drives them on its tick,
	// persisting each schedule's next-spawn time in scope state (restart-safe). An empty/unreachable load
	// yields no schedules — the director simply has no scheduled bosses.
	var schedules []director.Schedule
	// zoneRegion maps each pool zone to its region (#42 locality): the placement coordinator prefers to
	// colocate a region's zones on one shard. Empty when content has no regions (locality then no-ops).
	zoneRegion := map[string]string{}
	// worldScript is the content-defined world-director Lua signal handler (#47), read from pack_meta. Empty
	// => the director drains+acks signals with no orchestration reaction.
	var worldScript string
	if lc, err := content.Load(ctx, pool, []string{content.DemoPack}); err == nil {
		for _, sc := range lc.SpawnSchedules {
			schedules = append(schedules, director.BuildSchedule(sc))
		}
		if len(schedules) > 0 {
			slog.Info("loaded spawn schedules", "count", len(schedules))
		}
		for _, r := range lc.Regions {
			for _, z := range r.Zones {
				zoneRegion[z] = r.Ref
			}
		}
		worldScript = lc.WorldScript
	} else {
		slog.Warn("could not load director content (no schedules / world script)", "err", err)
	}

	// Build + run the WORLD director. Region directors (one per region_defs) join here once region
	// content lands (10.3+); for now the world scope is the deployable. The signal HANDLER (the
	// orchestration "director script") is content-defined — not yet authored — so the director currently
	// drains + acks signals (the write/broadcast machinery is live via the API; the built-in logic plugs
	// in here when director-script content lands). WithSchedules wires the Phase-12.4 boss scheduler.
	world := director.New("", pool, slog.Default()).
		WithScopeBus(scopeBus, instanceID).
		// #47: the content-defined world-director script reacts to signal-up events. Wired BEFORE
		// WithSchedules so the scheduler's reserved-event (boss.died) handling composes OUTERMOST and the
		// Lua handler (its `prev`) only sees non-reserved events. Empty/failed script => no orchestration.
		WithWorldScript(worldScript).
		WithSchedules(schedules).
		// #45: the world director periodically reaps dead-letter mail — orphaned mail (to a name that never
		// logs in) older than 30 days, and any mail older than 180 days (the backstop TTL). Leader-gated, so
		// only one director in the fleet runs it. Reap once a day.
		WithMailReaper(pool, 24*time.Hour, 30*24*time.Hour, 180*24*time.Hour).
		// #90: the LEADER world director aggregates the cross-shard presence roster into per-channel listener
		// sets and publishes changed channels' rosters (GMCP Comm.Channel.Players). Disabled without Redis.
		WithChannelRosterAggregator(rosterSrc, scopeComms, channelRosterInterval)
	if claimer != nil {
		world.WithElection(claimer, instanceID)
	}
	// Coordinated content pull (#212 slice 4 PR E): wire the puller only when a content store is
	// configured, so an in-game `pull <version>` installs a published version fleet-wide. Without a
	// content store the world director simply doesn't run coordinated pulls (the request is logged + dropped).
	if cfg.Content.URL != "" {
		// dir (the fleet view) is nil without Redis (single-process dev): the prune guard has no directory
		// to consult and is skipped. NOTE this is broader than "nothing is hosted" — a single-process
		// director with configured zones IS a live host of them, just not one recorded in a directory, so a
		// dev pull CAN hot-strip a pack whose zones this process is serving (surfaced loudly by the reload
		// path). The guard is a multi-shard-deployment safety, not a dev one.
		world.WithContentPuller(contentPuller{cfg: cfg, dir: dir})
		slog.Info("coordinated content pull enabled (director)", "content_url", cfg.Content.URL, "prune_guard", dir != nil)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		world.Run(ctx) // returns on ctx cancel, resigning its lease
	}()

	// Zone-placement COORDINATOR (docs/PLACEMENT.md §2/§4, Phase 10.6b): while this director is the leader,
	// observe the live fleet + the zone assignment and compute the desired rebalancing PLAN. The coordinator
	// is an OPTIMIZER, not a dependency — liveness (claim-from-pool, failover) is decentralized in the world
	// servers, so this only nudges toward balance. It currently OBSERVES + PLANS + LOGS the recommended
	// moves; executing a move drains a zone's live players via the cross-shard handoff (Shard.Drain) — the
	// documented next integration. Runs only with Redis (the fleet view) + a configured pool.
	if dir != nil && len(cfg.Zones) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runPlacementCoordinator(ctx, dir, world, cfg.Zones, zoneRegion)
		}()
	}

	slog.Info("starting", "env", cfg.Env, "instance", instanceID)
	<-ctx.Done()
	slog.Info("shutting down")
	wg.Wait() // let the director loop resign its lease + exit cleanly
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

// placementCoordinatorTick is how often the leader recomputes the placement plan. A moderate cadence: the
// directory leases (15s) already give liveness; balance is a slow-changing optimization, not a hot path.
const placementCoordinatorTick = 10 * time.Second

// channelRosterInterval is how often the leader re-aggregates + republishes changed channels' rosters (#90).
// A roster panel tolerates a few seconds; this matches the who-cache/heartbeat cadence order.
const channelRosterInterval = 3 * time.Second

// runPlacementCoordinator is the leader-only observe→plan→report loop. It honors leadership (a standby
// director does nothing) and surfaces the desired moves; the drain executor is the documented next step.
func runPlacementCoordinator(ctx context.Context, dir *directory.Redis, d *director.Director, pool []string, zoneRegion map[string]string) {
	ticker := time.NewTicker(placementCoordinatorTick)
	defer ticker.Stop()
	fleet := fleetView{dir: dir}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !d.IsLeader() {
				continue // only the leader coordinates placement
			}
			live, assignment, err := placement.Observe(ctx, fleet, pool)
			if err != nil {
				slog.Warn("placement: fleet observe failed", "err", err)
				continue
			}
			// #42 slice 3b: fold any pending rebalance directive into the assignment so the plan sees an
			// in-flight move as ALREADY applied — it won't re-issue that move, and won't over-target the
			// destination during the migration window. The stateless plan otherwise re-derives the same move
			// every tick until the occupancy signal shifts. Only fold when the target is still LIVE: a
			// directive to a since-dead shard must NOT overwrite the real observed owner (that would understate
			// the live owner's load for the tick) — the plan then treats the zone as still owned + the expired
			// directive / claim-from-pool reconciles it.
			liveSet := make(map[string]bool, len(live))
			for _, s := range live {
				liveSet[s] = true
			}
			for _, zone := range pool {
				if to, found, rerr := dir.ReadRebalance(ctx, zone); rerr == nil && found && liveSet[to] {
					assignment[zone] = to
				}
			}
			// #42: balance by real per-zone player WEIGHT (the occupancy signal each shard heartbeats), so a
			// busy town counts more than an empty wilderness. A read failure — or a zone with no live signal —
			// degrades to weight 1 (the zone-count plan), never a crash.
			zoneWeight, werr := dir.ZoneOccupancies(ctx)
			if werr != nil {
				slog.Warn("placement: zone occupancy read failed; balancing by zone count this tick", "err", werr)
				zoneWeight = nil
			}
			moves := placement.PlanColocated(live, assignment, pool, zoneWeight, zoneRegion)
			if len(moves) == 0 {
				continue // balanced + fully claimed: nothing to do
			}
			// #42 slice 3b: DRIVE the plan. An unclaimed-zone assignment (From=="") is left to decentralized
			// claim-from-pool (the world self-claims it); a rebalance drain (From!="") is issued as a directive
			// the owning shard executes, gated so it doesn't thrash.
			draining, _ := dir.ListDraining(ctx) // best-effort; exclude a target that is itself draining
			issued := 0
			for _, m := range moves {
				if m.From == "" {
					slog.Info("placement: zone needs (re)assignment (world self-claims)", "zone", m.Zone, "to", m.To)
					continue
				}
				if issueRebalance(ctx, dir, m, draining) {
					issued++
				}
			}
			if issued > 0 {
				slog.Info("placement: rebalance directives issued", "count", issued)
			}
		}
	}
}

const (
	// rebalanceDirectiveTTL bounds a published rebalance directive (matches the world executor's TTL): long
	// enough to outlive the drain, short enough that a stuck/abandoned directive self-expires so the plan
	// re-derives it.
	rebalanceDirectiveTTL = 90 * time.Second
	// rebalanceCooldown suppresses re-moving a zone right after it was rebalanced (weights shift — including
	// from the move itself — so a boundary zone could ping-pong tick to tick). Several minutes, >> the tick +
	// the drain deadline. Keyed by ZONE in the directory, so it survives the ownership change; the failover
	// claim path never reads it, so a crashed shard's zone is always reassignable.
	rebalanceCooldown = 5 * time.Minute
)

// issueRebalance gates and publishes ONE rebalance-drain directive; it returns whether the directive was
// issued. It skips a zone on cooldown (recently moved), one already being rebalanced (an in-flight
// directive), and a target shard that is itself draining (a fleet rollout — don't hand it more load). The
// fenced lease CAS in the executor is the ultimate no-double-own backstop; these gates only prevent thrash.
func issueRebalance(ctx context.Context, dir *directory.Redis, m placement.Move, draining map[string]bool) bool {
	if on, err := dir.OnCooldown(ctx, m.Zone); err != nil || on {
		return false
	}
	if _, inFlight, err := dir.ReadRebalance(ctx, m.Zone); err != nil || inFlight {
		return false
	}
	if draining[m.To] {
		return false
	}
	if err := dir.IssueRebalance(ctx, m.Zone, m.To, rebalanceDirectiveTTL); err != nil {
		slog.Warn("placement: issue rebalance failed", "zone", m.Zone, "to", m.To, "err", err)
		return false
	}
	if err := dir.SetCooldown(ctx, m.Zone, rebalanceCooldown); err != nil {
		slog.Warn("placement: set cooldown failed", "zone", m.Zone, "err", err)
	}
	slog.Info("placement: rebalance issued", "zone", m.Zone, "from", m.From, "to", m.To)
	return true
}

// fleetView adapts *directory.Redis to placement.Fleet: ListShards is direct; ShardForZone maps the
// directory's ErrNotFound (an unclaimed zone) to found=false so the coordinator reassigns it.
type fleetView struct{ dir *directory.Redis }

func (f fleetView) ListShards(ctx context.Context) ([]string, error) { return f.dir.ListShards(ctx) }

func (f fleetView) ShardForZone(ctx context.Context, zone string) (string, bool, error) {
	owner, err := f.dir.ShardForZone(ctx, zone)
	if errors.Is(err, directory.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return owner, true, nil
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

// contentPuller adapts the shared install pipeline (internal/contentpull) to the director's
// ContentPuller interface (#212 slice 4 PR E): a coordinated `pull <version>` resolves that version from
// the configured content store and imports it. The requested version overrides the config's pinned one.
// dir is the fleet view for the live-hosted-pack prune guard (PR E2); nil (single-process dev, no Redis)
// disables the guard.
type contentPuller struct {
	cfg config.Config
	dir *directory.Redis
}

func (p contentPuller) Pull(ctx context.Context, spec director.PullSpec) (director.PullOutcome, error) {
	opts := contentpull.Options{
		ContentURL:  p.cfg.Content.URL,
		Version:     spec.Version,
		Token:       p.cfg.Content.Token,
		CacheDir:    p.cfg.Content.CacheDir,
		PostgresDSN: p.cfg.Postgres.DSN,
		NATSURL:     p.cfg.NATS.URL,
		// The guard is still WIRED under force (below) — ForcePrune only downgrades its veto to a report,
		// so the operator learns what they overrode instead of it silently not being checked.
		ForcePrune: spec.Force,
	}
	// The prune guard needs the fleet directory to know which zones are hosted; without Redis there is no
	// fleet, so a single-process director prunes freely.
	if p.dir != nil {
		opts.PruneGuard = contentpull.FleetPruneGuard(zoneLocator{dir: p.dir})
	}
	res, err := contentpull.Pull(ctx, opts)
	if err != nil {
		return director.PullOutcome{}, err
	}
	return director.PullOutcome{ForcedPacks: res.PruneForced}, nil
}

// shardForZoner is the fleet lookup the prune-guard locator needs — satisfied by *directory.Redis. It is
// an interface so the ErrNotFound→not-hosted / other-error→propagate mapping is unit-testable without Redis.
//
// TemplateInUse is the second half (#416): a zone can be in use as an INSTANCE TEMPLATE without any shard
// holding its lease, so the lease lookup alone cannot answer "is this zone live".
type shardForZoner interface {
	ShardForZone(ctx context.Context, zone string) (string, error)
	TemplateInUse(ctx context.Context, template string) (bool, error)
}

// zoneLocator adapts the fleet directory to contentpull.ZoneLocator: a zone is "hosted" when the directory
// has a live shard owning it; the unclaimed sentinel (ErrNotFound) maps to not-hosted, any other error
// propagates (the guard fails closed on incomplete fleet info).
//
// # Why the lease lookup is not the whole answer (#416)
//
// A lease means "a shard owns this zone". Instances (#411) take no lease, and the natural authoring shape
// for a dungeon template is a zone that is never in cfg.Zones at all — the raw template is not meant to be
// walkable, so nothing ever claims it. A template with forty live copies and parties inside them therefore
// resolved to ErrNotFound and read as NOT HOSTED, so the guard passed and the pull stripped the pack out
// from under them.
//
// That is worse than the deferred harm the guard's doc reasons about. Stripping a leased zone's rows does
// not yank the running zone, because shard memory is authoritative. But instances are minted CONTINUOUSLY,
// and the next mint after the prune fails validateMintTemplate with "no such zone" — a runtime failure with
// no operator action in between.
type zoneLocator struct{ dir shardForZoner }

func (z zoneLocator) ZoneHosted(ctx context.Context, zone string) (bool, error) {
	_, err := z.dir.ShardForZone(ctx, zone)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, directory.ErrNotFound) {
		return false, err
	}
	// No lease. Before concluding "not hosted", ask whether the zone is live as an INSTANCE TEMPLATE.
	// An error here PROPAGATES rather than degrading to the unleased answer we already have: an
	// instance-aware lookup that fails must not be read as "nothing is using this, go ahead and prune",
	// which is precisely the fail-open this whole check exists to close.
	inUse, terr := z.dir.TemplateInUse(ctx, zone)
	if terr != nil {
		return false, terr
	}
	if inUse {
		// Say WHY, because the two blocking reasons have different operator remedies and the refusal alone
		// cannot be told apart. A leased zone means "drain it and retry". This means "a party is inside a
		// live copy right now" — wait for them to finish, or eject them. Without this line an operator sees
		// only "pack blocked" for a zone that appears in no lease anywhere.
		slog.Info("prune guard: zone has no lease but is LIVE as an instance template (parties are inside "+
			"copies of it right now); the pull is refused until they leave", "zone", zone)
	}
	return inUse, nil
}
