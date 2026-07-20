// Package director is the orchestration tier (docs/WORLD-EVENTS.md §3, Phase 10): the supra-zone
// actors that own region/world state and run cross-zone orchestration. A director is an ACTOR
// internally — an inbox + a tick + (later) a sandboxed Lua VM, the SAME model as a zone — but hosted
// out-of-band from the simulation shards in the telos-director service, so orchestration never competes
// with zone ticks for CPU. Region and world state have a SINGLE owning writer (the director), so even
// global state never has two writers: the actor model, one level up.
//
// This slice (10.1b) is the actor + its authoritative, persistence-backed STATE bag. Leader election
// (one live owner per scope) is 10.1c; the scoped event bus (signal/broadcast) is 10.2+. The golden
// rule (WORLD-EVENTS §intro) is enforced structurally: a director never reaches into a zone — it will
// SIGNAL over the bus, and each zone applies the consequence locally on its own goroutine.
package director

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// ScopeStore is the persistence seam for director scope state (the world_state / region_state tables).
// *store.Pool satisfies it; tests inject an in-memory implementation. A Save is an optimistic CAS on
// expectedVersion: ok=false means a concurrent writer (a stale leader racing the promoted standby during
// failover) moved the version — the director reloads + reconciles rather than clobbering.
type ScopeStore interface {
	LoadWorldState(ctx context.Context, key string) (value []byte, version uint64, found bool, err error)
	SaveWorldState(ctx context.Context, key string, value []byte, expectedVersion uint64) (newVersion uint64, ok bool, err error)
	LoadRegionState(ctx context.Context, regionID, key string) (value []byte, version uint64, found bool, err error)
	SaveRegionState(ctx context.Context, regionID, key string, value []byte, expectedVersion uint64) (newVersion uint64, ok bool, err error)
}

// DefaultTick is the director heartbeat. Director scripts schedule waves/phases against it (10.4+).
const DefaultTick = time.Second

// Director is one scope's owning actor: the WORLD director (regionID == "") or a REGION director
// (regionID == the region_defs ref). It owns an in-memory, authoritative copy of its scope state plus
// the per-key versions for the optimistic-concurrency CAS, and is the single serialization point for
// every read and write of that scope — exactly like a zone is for its entities.
type Director struct {
	regionID string // "" => the world director; otherwise the region this director owns
	store    ScopeStore
	log      *slog.Logger
	tick     time.Duration

	inbox chan msg

	// state / versions are the authoritative in-memory scope state and each key's persisted version.
	// Touched ONLY by the actor goroutine (Run), so no lock is needed — the single-writer invariant.
	state    map[string]json.RawMessage
	versions map[string]uint64

	// Leader election (10.1c). When a claimer is wired, the director campaigns for an exclusive lease on
	// its scope so exactly ONE live instance owns it (the others are warm standbys). leader is read from
	// any goroutine (IsLeader) and written by the Run goroutine's campaign, so it is atomic.
	claimer    LeaseClaimer
	instanceID string
	leaseID    string
	leaseTTL   time.Duration
	leader     atomic.Bool

	// Scoped event bus (10.4). The director CONSUMES signal-up events from its zones (durable) and
	// BROADCASTS state + remote effects DOWN (transient). nil disables orchestration I/O (a state-only
	// director / the 10.1 tests). source seeds the down-broadcast author; handler is the orchestration
	// logic (the "director script") invoked per signal-up. applied is the per-source dedup high-water for
	// the at-least-once durable stream; consumer is the live durable subscription (only while leader).
	bus      *scopebus.Bus
	source   string
	handler  SignalHandler
	applied  map[string]uint64
	consumer commbus.Consumer

	// writeFailed records that a d.set did NOT land during the CURRENT handleSignal apply window (#354) —
	// a lost CAS, a store error, or a write refused because this director no longer leads the scope.
	// handleSignal arms it before dispatch and inspects it after, NAKing the signal rather than acking a
	// consequence that never happened. Actor-goroutine only (like state/versions), so no lock.
	//
	// It is a DIRECTOR field rather than a SignalHandler return value or an *API flag, for three reasons
	// no return-value design can match:
	//   - a script's `pcall(function() director.set(k,v) end)` SWALLOWS the Lua error luaSet raises, so
	//     any outcome derived from error propagation is invisible through content;
	//   - WithSchedules composes handlers as closures that discard returns, so every present and future
	//     wrapper would have to remember to propagate it;
	//   - the only production Go writer, saveScheduleState, calls d.set DIRECTLY and never holds an *API
	//     at all — an API-scoped flag would miss the one path that wedges a boss permanently.
	writeFailed bool

	// Scheduled spawns (Phase 12.4). schedules are the loaded boss schedules this director owns; the tick
	// drives them (spawn-when-due, broadcast DOWN) and the boss.died signal reschedules. now is the clock
	// seam (time.Now in prod; a test injects a fixed/advancing clock to drive the schedule deterministically).
	schedules    []Schedule
	now          func() time.Time
	scheduleInit bool // the startup on_missed pass has run this session

	// Coordinated content pull (#212 slice 4 PR E). puller runs the resolve→import→broadcast pipeline for
	// a `pull <version>` request; nil disables coordinated pulls (a state-only director). pulling
	// single-flights it so a second request while one is in flight is dropped rather than double-importing.
	puller  ContentPuller
	pulling atomic.Bool
	// workers tracks in-flight off-actor pull workers so Run's shutdown WAITS for them (#230). The worker
	// derives its ctx from Run's ctx, so ctx-cancel aborts an in-flight git clone/import promptly and the
	// Wait bounds shutdown rather than orphaning the goroutine (previously it used context.Background()).
	workers sync.WaitGroup

	// Mail dead-letter reaper (#45): a leader-gated, tick-driven periodic reap of undeliverable/orphaned
	// mail — the director-owned scheduler tick this track exercises, the same shape as the boss scheduler.
	// nil reaper disables it. reapInterval is the cooldown between reaps; reapOrphanGrace / reapHardTTL are
	// the two retention windows (see ReapDeadLetterMail). lastReapAt (actor-goroutine only) tracks the last
	// reap; reapInFlight single-flights the off-actor DELETE so a slow reap can't stall the director loop or
	// overlap itself.
	mailReaper      MailReaper
	reapInterval    time.Duration
	reapOrphanGrace time.Duration
	reapHardTTL     time.Duration
	lastReapAt      time.Time
	reapInFlight    atomic.Bool

	// Channel-roster aggregator (#90): the LEADER periodically inverts the cross-shard presence roster to
	// each channel's listener set and publishes CHANGED channels' rosters to their per-channel roster subject
	// (Comm.Channel.Players). nil source/bus disables it. rosterInterval is the poll cooldown; lastRosterAt
	// (actor-goroutine only) tracks the last poll; rosterInFlight single-flights the off-actor List+publish;
	// lastRosters (worker-goroutine only, single-flighted) is the per-channel snapshot the poll diffs against;
	// rosterPolls (worker-only) counts aggregations to drive the periodic FULL resync (a roster is convergent
	// state, so a transient publish dropped for some subscriber must eventually be re-sent even absent a change).
	rosterSrc      ChannelRosterSource
	rosterBus      commbus.Bus
	rosterInterval time.Duration
	lastRosterAt   time.Time
	rosterInFlight atomic.Bool
	lastRosters    map[string][]string
	rosterPolls    uint64
}

// MailReaper deletes undeliverable/orphaned mail past the given retention cutoffs (satisfied by *store.Pool).
// It lives here as an interface so the director package stays free of the store dependency, mirroring
// ContentPuller. A nil reaper disables the periodic reap.
type MailReaper interface {
	ReapDeadLetterMail(ctx context.Context, orphanCutoff, hardCutoff time.Time) (int64, error)
}

// ContentPuller installs a published content version (resolve from the external store → atomic Postgres
// import → hot-reload broadcast). The concrete implementation lives at the cmd tier (cmd/telos-director,
// over internal/contentpull) so the director package stays free of the git/store/content dependencies.
// A nil puller means the director does not run coordinated pulls.
type ContentPuller interface {
	Pull(ctx context.Context, spec PullSpec) (PullOutcome, error)
}

// PullOutcome is what a completed pull reports BACK. It exists because the request side was made a struct
// while the response side stayed a bare error, which left the forced-prune record with nowhere to go: the
// packs an operator overrode were computed, logged in the director's process, and then dropped on the floor
// before reaching the person who typed the command. A break-glass action whose report does not reach the
// operator is not audited, it is just logged somewhere else.
type PullOutcome struct {
	// ForcedPacks are packs the live-hosted-pack guard BLOCKED that a forced pull stripped anyway (#427).
	// Empty on every ordinary pull, including a forced one that turned out to block nothing.
	ForcedPacks []string
}

// PullSpec is one coordinated-pull request as the director hands it to the puller. It is a STRUCT rather
// than positional arguments deliberately: Force is a player-affecting override, and a positional bool at an
// interface boundary is how one gets passed by accident. (It also retires the `actor` parameter the old
// signature declared and the implementation ignored.)
type PullSpec struct {
	Version string // the published content version (git tag/SHA) to install
	Actor   string // the character id who requested it, for logging/audit
	// Force overrides the live-hosted-pack prune guard. The guard STILL RUNS when it is set — the blocked
	// list is computed, logged and reported back — it simply no longer aborts the pull. See #427.
	Force bool
}

// New builds a director for a scope. regionID "" makes the WORLD director; a non-empty ref makes that
// region's director. The actor does not run until Run is called.
func New(regionID string, store ScopeStore, log *slog.Logger) *Director {
	return &Director{
		regionID: regionID,
		store:    store,
		log:      log.With("component", "director", "scope", scopeLabel(regionID)),
		tick:     DefaultTick,
		inbox:    make(chan msg, 256),
		state:    map[string]json.RawMessage{},
		versions: map[string]uint64{},
		applied:  map[string]uint64{},
		now:      time.Now,
	}
}

// WithTick overrides the heartbeat (tests use a fast tick; production the default). Call before Run.
func (d *Director) WithTick(t time.Duration) *Director {
	if t > 0 {
		d.tick = t
	}
	return d
}

// WithNow overrides the scheduler clock (tests inject a fixed/advancing clock to drive scheduled spawns
// deterministically). Call before Run.
func (d *Director) WithNow(now func() time.Time) *Director {
	if now != nil {
		d.now = now
	}
	return d
}

// WithContentPuller wires the coordinated-pull executor (#212 slice 4 PR E): the world director runs it
// on a `content.pull.request` signal to install a published content version. Call before Run. A nil
// puller (a state-only / region director) leaves coordinated pulls disabled.
func (d *Director) WithContentPuller(p ContentPuller) *Director {
	d.puller = p
	return d
}

// WithMailReaper wires the periodic dead-letter mail reap (#45): the LEADER director reaps undeliverable /
// orphaned mail every `interval`, deleting orphaned mail older than `orphanGrace` and any mail older than
// `hardTTL`. Call before Run. A nil reaper (or a non-positive interval) leaves the reap disabled — the
// standalone/dev default, and every region director (only the world director is wired with it in prod).
func (d *Director) WithMailReaper(r MailReaper, interval, orphanGrace, hardTTL time.Duration) *Director {
	if r == nil || interval <= 0 {
		return d
	}
	// The hard TTL is a backstop ABOVE the orphan grace — it must never be SHORTER, or the hard arm would
	// reap a live player's unread mail younger than the orphan window intended (deleting deliverable mail
	// early). Clamp a misconfig up to the grace and warn rather than silently over-reaping.
	if hardTTL < orphanGrace {
		d.log.Warn("mail reaper hard TTL is shorter than the orphan grace; clamping up to the grace",
			"hard_ttl", hardTTL, "orphan_grace", orphanGrace)
		hardTTL = orphanGrace
	}
	d.mailReaper = r
	d.reapInterval = interval
	d.reapOrphanGrace = orphanGrace
	d.reapHardTTL = hardTTL
	return d
}

// maybeReapMail runs the dead-letter reap when due (#45), on the director tick. It is LEADER-gated by the
// caller (onTick), so exactly one director reaps fleet-wide. The cooldown (reapInterval) is checked on the
// actor goroutine against the director clock, and the actual DELETE runs OFF the actor goroutine (a slow
// reap must not stall the director loop), single-flighted so it never overlaps itself.
//
// The reap is NOT lease-fenced: unlike director SCOPE STATE (single-writer, leader-fenced), mail is a global
// table, so during a failover a resigning leader's in-flight worker and the newly-promoted leader (whose
// lastReapAt is zero, so it reaps immediately) can briefly BOTH issue a reap. That is harmless because
// ReapDeadLetterMail is an idempotent `DELETE ... WHERE sent_at < cutoff` — the overlap deletes the same
// already-eligible rows, never live data. So the reap rests on DELETE idempotency, not the leader lock.
func (d *Director) maybeReapMail(ctx context.Context) {
	if d.mailReaper == nil {
		return
	}
	now := d.now()
	if !d.lastReapAt.IsZero() && now.Sub(d.lastReapAt) < d.reapInterval {
		return
	}
	if !d.reapInFlight.CompareAndSwap(false, true) {
		return // a prior reap is still running; try again next interval
	}
	d.lastReapAt = now
	orphanCutoff := now.Add(-d.reapOrphanGrace)
	hardCutoff := now.Add(-d.reapHardTTL)
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		defer d.reapInFlight.Store(false)
		n, err := d.mailReaper.ReapDeadLetterMail(ctx, orphanCutoff, hardCutoff)
		if err != nil {
			d.log.Warn("mail dead-letter reap failed", "err", err)
			return
		}
		if n > 0 {
			d.log.Info("reaped dead-letter mail", "count", n)
		}
	}()
}

func scopeLabel(regionID string) string {
	if regionID == "" {
		return "world"
	}
	return "region:" + regionID
}

// msg is the director inbox message (mirrors the zone's msg interface). The marker method keeps the
// inbox typed to director messages only.
type msg interface{ directorMsg() }

// post enqueues a message onto the director's inbox — the ONLY ingress to its state, from any goroutine.
func (d *Director) post(m msg) { d.inbox <- m }

// Run is the director actor loop: it processes inbox messages and ticks on its heartbeat, all on this
// one goroutine, so every scope-state read/write is single-writer. Returns when ctx is cancelled.
func (d *Director) Run(ctx context.Context) {
	d.log.Debug("director loop start")
	ticker := time.NewTicker(d.tick)
	defer ticker.Stop()

	// Leader election (10.1c): with no claimer wired, this director is always the leader (single-process
	// / hermetic tests). With a claimer, campaign for the scope lease immediately (so leadership is known
	// at startup) and then renew/contest it on a sub-ticker faster than the TTL; a crash lets the TTL
	// expire and a standby take over.
	var campaignC <-chan time.Time
	if d.claimer == nil {
		d.leader.Store(true)
	} else {
		d.campaign(ctx)
		ct := time.NewTicker(d.leaseTTL / 3)
		defer ct.Stop()
		campaignC = ct.C
	}
	// Bind the durable signal-up consumer to LEADERSHIP: only the live leader consumes + applies its
	// scope's events (a standby must not). syncScopeSubscription subscribes on becoming leader and tears
	// down on losing it; called now (initial state) and after every campaign. Torn down at loop exit.
	d.syncScopeSubscription(ctx)
	defer d.unsubscribeSignals()

	for {
		select {
		case <-ctx.Done():
			d.log.Debug("director loop stop")
			d.resign()
			d.workers.Wait() // #230: bound shutdown on any in-flight pull worker (ctx-cancelled above)
			return
		case m := <-d.inbox:
			d.handle(ctx, m)
		case <-ticker.C:
			d.onTick(ctx)
		case <-campaignC:
			d.campaign(ctx)
			d.syncScopeSubscription(ctx)
		}
	}
}

// onTick is the heartbeat hook: it drives the scheduled-spawn scheduler (Phase 12.4) when this director is
// the leader (a standby must not spawn). Runs on the actor goroutine, so it reads/writes scope state
// single-writer.
func (d *Director) onTick(ctx context.Context) {
	if !d.leader.Load() {
		return // only the leader drives scheduled work — one owner fleet-wide
	}
	if len(d.schedules) > 0 {
		d.runSchedules(ctx)
	}
	d.maybeReapMail(ctx)                // #45: periodic dead-letter mail reap (no-op unless a reaper is wired)
	d.maybeAggregateChannelRosters(ctx) // #90: periodic per-channel roster publish (no-op unless wired)
}

func (d *Director) handle(ctx context.Context, m msg) {
	switch v := m.(type) {
	case getMsg:
		v.reply <- d.get(ctx, v.key)
	case setMsg:
		_, err := d.set(ctx, v.key, v.value)
		v.reply <- err
	case signalMsg:
		d.handleSignal(ctx, v)
	default:
		d.log.Warn("director: unknown inbox message", "type", v)
	}
}
