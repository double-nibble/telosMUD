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

	// Scheduled spawns (Phase 12.4). schedules are the loaded boss schedules this director owns; the tick
	// drives them (spawn-when-due, broadcast DOWN) and the boss.died signal reschedules. now is the clock
	// seam (time.Now in prod; a test injects a fixed/advancing clock to drive the schedule deterministically).
	schedules    []Schedule
	now          func() time.Time
	scheduleInit bool // the startup on_missed pass has run this session
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
	if len(d.schedules) > 0 && d.leader.Load() {
		d.runSchedules(ctx)
	}
}

func (d *Director) handle(ctx context.Context, m msg) {
	switch v := m.(type) {
	case getMsg:
		v.reply <- d.get(ctx, v.key)
	case setMsg:
		v.reply <- d.set(ctx, v.key, v.value)
	case signalMsg:
		d.handleSignal(ctx, v)
	default:
		d.log.Warn("director: unknown inbox message", "type", v)
	}
}
