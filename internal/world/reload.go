package world

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// Bounded-retry delivery for a dropped zone-shape reconcile (#191 PR 3/3, the #194 reliability piece for
// this path). The reconcile fan-out stays NON-BLOCKING (postOrDrop) — a wedged zone inbox must never
// head-of-line-stall every later invalidation shard-wide — but a dropped reconcileZoneMsg is worse than a
// dropped Lua reload: a dropped REMOVE leaves an interactive GHOST ROOM that accumulates players and a
// singleton never self-heals. So on a drop we hand the message to a short-lived retry goroutine that
// re-posts it with bounded backoff. The retry re-posts the SAME immutable message with its ORIGINAL
// version (the reconcileZoneMsg invariant): the version guard advances the cursor only on APPLICATION, so
// a dropped-then-retried reconcile re-applies cleanly, and a retry can never resurrect stale state over a
// newer reload (a superseding reload advanced the cursor past this version, so the stale retry is dropped
// by the guard — safe). These are package vars so a test can shrink the timing.
var (
	// reconcileRetryAttempts is how many times a dropped reconcile is re-posted before giving up (the
	// operator remedy is then to re-run `reload`). The inbox is 256-deep and drains fast, so a few paced
	// attempts cover a transient backlog.
	reconcileRetryAttempts = 5
	// reconcileRetryBackoff is the base inter-attempt delay; attempt N waits N×this (linear backoff).
	reconcileRetryBackoff = 50 * time.Millisecond
	// maxReconcileRetryGoroutines caps concurrent zone-shape reconcile retry goroutines so a reload storm
	// against a saturated shard cannot spawn unbounded goroutines; past the cap a drop is logged and
	// abandoned (re-run remedy).
	maxReconcileRetryGoroutines int64 = 128
	// maxCommsRepublishRetryGoroutines is the SEPARATE cap for comms hear-set republish retries (#345). Comms
	// republish (retryRepublishComms) and zone-shape reconcile (retryReconcile) both re-post dropped hot-reload
	// messages on short-lived goroutines, but they guard DIFFERENT failure classes — a lost republish leaves a
	// stale, too-permissive hear-set (security-relevant, #75); a lost reconcile leaves a ghost room — so they
	// draw from INDEPENDENT counters: a storm of one can no longer starve the other's retry-goroutine budget.
	//
	// Sized at PARITY with the reconcile cap, deliberately (distsys review). Both retries are 1:Z (one per
	// hosted zone) after #269 coalesced a K-channel edit to a single republish per zone, but a channel edit
	// fans to EVERY hosted zone and there is no hard cap on zones-per-shard (s.zones grows at runtime). A
	// SMALLER comms budget would make the security-relevant class — whose "re-run the reload" remedy is the
	// WEAKER one (an identical edit may emit no KindChannel invalidation, so the retry is effectively the
	// primary remedy) — the FIRST to be truncated on a many-zone shard, exactly backwards. The extra ~128
	// short-lived, briefly-sleeping goroutines cost is negligible, so the security-weighted path gets equal
	// headroom rather than less.
	maxCommsRepublishRetryGoroutines int64 = 128

	// contentRefreshDebounce is how long a marked content-snapshot refresh WAITS before re-reading (#418).
	// Package vars so a test can shrink them.
	//
	// It buys two things a bare single-flight cannot. First, ORDERING: the refresh reads Postgres directly
	// while the prototype cache is fed ref-by-ref off the bus, and a pull's rows are all committed before its
	// first invalidation is published — so a refresh that fires on zone #1's invalidation returns content
	// describing zone #40's rooms whose prototypes are still in flight. Waiting for the fan-out to drain
	// keeps the two halves of a zone from coming from different places. (It is not a correctness barrier —
	// buildZone refuses an incomplete build either way — but without it, every deploy would produce a burst
	// of refused mints.) Second, COALESCING: a 40-zone pack marks 40 times and re-reads once.
	contentRefreshDebounce = 2 * time.Second
	// contentRefreshJitter is spread added on top of the debounce. Every shard in the fleet receives the same
	// broadcast within milliseconds, and the re-read is a full materialization of the deployed world: without
	// jitter, N shards hit the content database simultaneously on every deploy, and again — all together —
	// the instant a partition heals. Jitter turns a synchronized spike into a spread.
	contentRefreshJitter = 3 * time.Second
	// rejectedRereadCooldown is the minimum interval between whole-content re-reads of a version the publish
	// gate already REJECTED (#423). See the cooldown note in refreshContentSnapshot: without it, broken rows
	// leave the version gate permanently open and every content event costs every shard a full read. Short
	// enough that an operator's in-place row fix converges on its own; long enough that a broken deploy is
	// not a read multiplier against the content database.
	rejectedRereadCooldown = 60 * time.Second
)

// reload.go is the shard-side hot-reload applier (docs/PHASE4-PLAN.md §5). It subscribes to the
// content bus; on an invalidation it RE-READS just the named (kind, ref) definition from the
// content source, REBUILDS the one *Prototype via the same DTO->component mapper the boot loader
// uses (content_map.go), and SWAPS it into the per-shard prototype cache race-safely
// (protoCache.reload — the atomic table swap, prototype.go). The NEXT spawn of that ref uses the
// new data; live instances spawned earlier keep the old prototype (it stays alive via GC), which
// is the documented MUD semantics: an existing mob keeps its stats, the next repop uses the edit.
//
// # Optional, never fatal
//
// Hot reload is OPTIONAL: a shard with no bus OR no source has a nil reloader and behaves exactly
// as a pre-4.3 shard (boot-load still works, every storeless/busless test stays green). A bus
// subscribe failure is logged, never fatal. Mirrors WithPersistence's disabled fallback.
//
// # Single-writer of the cache swap
//
// The bus delivers invalidations SERIALLY per subscription, so the applier runs reloads one at a
// time on the subscription's own goroutine — it is the sole RUNTIME writer of the cache table.
// spawn never writes the table (only get -> atomic Load), so the swap races neither a reader nor
// another reload. This is the per-shard concurrency contract the distributed-systems-architect
// scrutinizes: the cache is a shared-read structure mutated only by atomic whole-table swap.

// reloadIOTimeout bounds the single-ref re-read so a slow/hung content source cannot wedge the
// subscriber goroutine (which would silently stop every later reload). On timeout the reload is
// abandoned; the last-known prototype stays in the cache and the next invalidation retries.
const reloadIOTimeout = 5 * time.Second

// reloader holds the hot-reload wiring for one shard: the content source to re-read a single
// definition from, the shared prototype cache to swap into, and the live bus subscription. It is
// nil on a shard with no bus/source (hot reload disabled).
type reloader struct {
	src   content.DefinitionSource // single-ref re-read (Postgres in prod, embedded/mem in tests)
	cache *protoCache              // the per-shard cache whose entries this swaps (shared read)
	bus   contentbus.Bus           // the invalidation bus
	sub   contentbus.Subscription  // live subscription; Unsubscribe on stop
	packs map[string]bool          // packs this shard loads; an edit to another pack is ignored
	// enabled is the same pack set as packs, kept as the ORDERED boot list so the `reload` staff command
	// (reloadcmd.go) can iterate + display them deterministically. Set once at construction, read-only.
	enabled []string
	shard   *Shard // the hosting shard — to post a reloadLuaMsg to each zone (7.7); nil on a bare test
	log     *slog.Logger

	// Bounded-retry delivery of a dropped zone-shape reconcile (#191 PR 3/3). retryInFlight caps concurrent
	// reconcile retry goroutines; retryDone is closed once at stop() to cancel any in-flight retry promptly.
	retryInFlight atomic.Int64
	// commsRetryInFlight is the SEPARATE counter for comms hear-set republish retries (#345): it caps
	// retryRepublishComms independently of retryInFlight so a comms-republish storm and a zone-shape
	// reconcile storm can't starve each other's retry budget (they guard different failure classes — a
	// too-permissive hear-set vs a ghost room). Bounded by maxCommsRepublishRetryGoroutines.
	commsRetryInFlight atomic.Int64
	retryDone          chan struct{}
	stopOnce           sync.Once

	// Reconcile-on-join (#212 slice 4 PR D): appliedContentVersion is the monotonic content version this
	// shard's in-memory prototypes reflect (seeded at boot from the version the content was loaded at). On
	// a bus reconnect, reconcileOnJoin compares it against the CURRENT Postgres content version and, if the
	// shard fell behind during the gap (a pull it missed), re-applies locally to catch up. reconcileInFlight
	// single-flights concurrent reconnects.
	appliedContentVersion atomic.Uint64
	reconcileInFlight     atomic.Bool

	// Live content snapshot refresh (#418). contentStale is set by any signal that the shard's boot-time
	// content view may no longer match the deployed rows; contentRefreshInFlight single-flights the
	// re-read the marker kicks off. See markContentStale / refreshContentSnapshot.
	contentStale           atomic.Bool
	contentRefreshInFlight atomic.Bool
	// refreshDebounce/refreshJitter are captured from the package vars at CONSTRUCTION rather than read in
	// the refresh loop. The loop runs on a goroutine that outlives the call that started it, so reading the
	// package vars there races any test that restores them in a t.Cleanup while a previous test's worker is
	// still sleeping — a real, -race-detectable data race between unrelated tests.
	refreshDebounce time.Duration
	refreshJitter   time.Duration
	// snapshotContentVersion is the content version the CURRENT published snapshot was read at. It makes the
	// refresh idempotent: a mark whose version the snapshot already reflects skips the whole-pack read, so a
	// replayed or forged invalidation cannot be used as a read-amplification lever against Postgres.
	snapshotContentVersion atomic.Uint64
	// rejectedContentVersion / rejectedAtNanos / rejectedProblemSet track the last REFUSED publish (#423).
	//
	// rejectedContentVersion + rejectedAtNanos drive the re-read COOLDOWN: a rejection must not advance
	// snapshotContentVersion (that would make the shard treat refused rows as published and stop retrying),
	// but re-reading the whole content database on every content event while rows stay broken is the exact
	// amplification the version gate exists to prevent. The pair bounds it to one re-read per
	// rejectedRereadCooldown while still converging on an in-place row fix.
	//
	// rejectedProblemSet is the LOG memo, keyed on the problem set rather than the version — see
	// rejectionKey for why a version-keyed memo can be used to hide a second, different rejection.
	rejectedContentVersion atomic.Uint64
	rejectedAtNanos        atomic.Int64
	rejectedProblemSet     atomic.Uint64
}

// contentVersioner is the subset of the content source that reports the current Postgres content
// version — satisfied by *store.Pool in production, absent on the embedded/mem sources (which then
// skip reconcile-on-join). Kept structural so the world package needs no store import (store imports
// world, so the reverse would cycle).
type contentVersioner interface {
	ContentVersion(ctx context.Context) (uint64, error)
}

// contentVersionBumper is the subset of the content source that ATOMICALLY bumps + returns the monotonic
// Postgres content version — satisfied by *store.Pool, absent on the embedded/mem sources. When present, a
// shard-local reload mints its bus version by BUMPING the single PG authority (#232) instead of stamping
// the wall clock, so the version is monotonic fleet-wide (no per-shard clock skew) and the bump is visible
// to reconcile-on-join. Structural, to keep the world package free of a store import (store imports world).
//
// SEAM WARNING: a PRODUCTION source must be a *store.Pool (or a wrapper that FORWARDS BumpContentVersion).
// A decorator that dropped it would fail this assertion, silently take the wall-clock fallback path in
// mintReloadVersion, and reintroduce the #222/#232 clock-skew residual — with a green test suite. Same
// accepted risk as the sibling contentVersioner assertion; keep the live source concrete.
type contentVersionBumper interface {
	BumpContentVersion(ctx context.Context) (uint64, error)
}

// localApplyBus is a contentbus.Bus whose Publish applies each invalidation to THIS shard directly
// (r.onInvalidation) instead of broadcasting. reconcileOnJoin feeds a re-read pack through PublishPack
// over it, reusing the exact per-ref swap + zone-shape reconcile the wire path uses — but LOCAL-only,
// so a rejoining shard catches itself up without re-broadcasting to (and re-swapping) the whole fleet.
type localApplyBus struct{ r *reloader }

func (b localApplyBus) Publish(_ context.Context, inv contentbus.Invalidation) error {
	b.r.onInvalidation(inv)
	return nil
}

func (localApplyBus) Subscribe(func(contentbus.Invalidation)) (contentbus.Subscription, error) {
	return nil, nil // unused: a local-apply bus is never subscribed to
}
func (localApplyBus) OnReconnect(func()) {}
func (localApplyBus) Close() error       { return nil }

// newReloader wires a reloader over src/cache/bus for the given enabled packs and SUBSCRIBES. A
// nil bus or nil src yields a nil reloader (hot reload disabled). A subscribe failure logs and
// returns nil — never fatal, so an unreachable/closed bus simply disables hot reload.
func newReloader(src content.DefinitionSource, cache *protoCache, bus contentbus.Bus, enabledPacks []string, bootVersion uint64, shard *Shard) *reloader {
	if bus == nil || src == nil || cache == nil {
		return nil
	}
	r := &reloader{
		src:       src,
		cache:     cache,
		bus:       bus,
		packs:     map[string]bool{},
		enabled:   append([]string(nil), enabledPacks...),
		shard:     shard,
		log:       slog.With("component", "contentreload"),
		retryDone: make(chan struct{}),
		// Snapshot the refresh timing once, here — see the field comments.
		refreshDebounce: contentRefreshDebounce,
		refreshJitter:   contentRefreshJitter,
	}
	// Seed the applied version with the version the boot content was loaded at (read BEFORE the packs
	// in loadContent, so it is never AHEAD of the loaded content — a pull racing boot fails safe to a
	// redundant re-apply on the first reconnect, never a missed catch-up).
	r.appliedContentVersion.Store(bootVersion)
	// The boot snapshot was read at the same version, so the refresh's version gate starts calibrated: an
	// invalidation replayed from before this process started re-reads nothing (#418).
	r.snapshotContentVersion.Store(bootVersion)
	for _, p := range enabledPacks {
		r.packs[p] = true
	}
	sub, err := bus.Subscribe(r.onInvalidation)
	if err != nil {
		r.log.Warn("content invalidation subscribe failed; hot reload disabled", "err", err)
		return nil
	}
	r.sub = sub
	// Reconcile-on-join (#212 slice 4 PR D): after a bus reconnect, catch up on any pull missed during
	// the gap. Off the bus goroutine (reconcileOnJoin does PG I/O), single-flighted.
	bus.OnReconnect(func() { go r.reconcileOnJoin() })
	r.log.Debug("hot reload enabled", "packs", enabledPacks, "boot_content_version", bootVersion)
	return r
}

// reconcileOnJoin catches this shard up to the current Postgres content version after a bus gap
// (#212 slice 4 PR D). Core NATS buffers nothing while disconnected, so a pull that broadcast during
// the gap was missed — the DB rows are current (Model 1) but the in-memory prototypes are stale. It
// reads the current content version; if the shard is behind, it re-reads its enabled packs from
// Postgres and applies them LOCALLY (via localApplyBus — no fleet re-broadcast) stamped with that
// version, exactly as the missed broadcast would have. It NEVER re-pulls (the source is Postgres, not
// git), NEVER refuses logins, and on a read failure just logs and keeps serving the current content.
// Single-flighted against concurrent reconnects; runs off the bus goroutine.
func (r *reloader) reconcileOnJoin() {
	if !r.reconcileInFlight.CompareAndSwap(false, true) {
		return // a catch-up is already running (rapid reconnects) — its result covers this one
	}
	defer r.reconcileInFlight.Store(false)

	vs, ok := r.src.(contentVersioner)
	if !ok {
		return // source can't report a version (embedded/mem dev source): nothing to reconcile
	}
	ctx, cancel := context.WithTimeout(context.Background(), reloadRepublishTimeout)
	defer cancel()
	cur, err := vs.ContentVersion(ctx)
	if err != nil {
		r.log.Warn("reconcile-on-join: could not read the content version; serving current content", "err", err)
		return
	}
	applied := r.appliedContentVersion.Load()
	if cur == 0 || cur <= applied {
		return // not behind (0 = never imported): the reconnect changed nothing to catch up on
	}
	src, ok := r.src.(content.Source)
	if !ok {
		return
	}
	packs, err := src.LoadPacks(ctx, r.enabled)
	if err != nil {
		r.log.Warn("reconcile-on-join: re-read of packs failed; serving current content", "err", err)
		return
	}
	local := localApplyBus{r: r}
	total := 0
	for _, pk := range packs {
		n, _ := contentbus.PublishPack(ctx, local, pk, cur) // applies locally via r.onInvalidation
		total += n
	}
	// advanceApplied (not a raw Store): a wire delivery of a NEWER pull could land on the subscription
	// goroutine during this reconcile and advance the cursor past `cur` — a hard Store would regress it.
	r.advanceApplied(cur)
	// The local re-apply above drove the per-ref swaps and the live-zone reconciles; the snapshot that
	// runtime zone builds and instance mints read from needs the same catch-up (#418). Marked rather than
	// refreshed inline so it coalesces with the marks the local PublishPack fan-out just produced.
	r.markContentStale()
	r.log.Info("reconcile-on-join: caught up after a content-bus gap",
		"from_version", applied, "to_version", cur, "definitions_applied", total)
}

// advanceApplied monotonically raises appliedContentVersion to v (a no-op if v is 0 or not higher),
// via a CAS loop so a concurrent wire delivery and a reconcile-on-join agree on the maximum.
func (r *reloader) advanceApplied(v uint64) {
	for v > 0 {
		prev := r.appliedContentVersion.Load()
		if v <= prev {
			return
		}
		if r.appliedContentVersion.CompareAndSwap(prev, v) {
			return
		}
	}
}

// markContentStale records that the shard's live content snapshot (s.content) may be behind the deployed
// rows and kicks a single-flighted background refresh (#418).
//
// # Why a marker plus a background re-read, rather than a direct patch
//
// The snapshot has to converge on every path that changes content, and the shard-side signals differ per
// path. A coordinated pull ends in the version-complete SENTINEL; a shard-local staff `reload` emits NO
// sentinel (only per-ref invalidations plus a trailing KindZone per zone); a reconcile-on-join has the
// packs in hand already. Marking from all three and refreshing in one place means one mechanism instead of
// three, and — the load-bearing part — it COALESCES: a pull of a 40-zone pack marks 40 times and re-reads
// once, instead of 40 full pack reads against Postgres on the bus goroutine.
//
// Patching the snapshot per-invalidation was the alternative and it cannot work: the KindZone wire payload
// carries only the room set and start room (contentbus.Invalidation), not resets, reset_secs, or
// `instanceable` — and a zone DELETED from a pack emits no invalidation at all, so a patch could never
// learn about the one case (#418's "a deleted template still mints") that most needs learning.
//
// # The refresh runs OFF the bus goroutine
//
// onInvalidation is serial per subscription: a full LoadPacks against Postgres there would head-of-line
// stall every later invalidation behind it. Same reason bus.OnReconnect dispatches reconcileOnJoin to its
// own goroutine.
func (r *reloader) markContentStale() {
	if r == nil || r.shard == nil {
		return // a bare test reloader with no shard has no snapshot to refresh
	}
	// Order matters: publish the mark BEFORE claiming the in-flight slot. The refresher clears the mark
	// before each pass, so a mark published after its clear is guaranteed either to win the CAS here or to
	// be seen by the running refresher's re-check below — never dropped between the two.
	r.contentStale.Store(true)
	if !r.contentRefreshInFlight.CompareAndSwap(false, true) {
		return // a refresh is already running; it will observe the mark we just set
	}
	go func() {
		defer func() {
			r.contentRefreshInFlight.Store(false)
			// Close the lost-wakeup window: a mark that arrived after our last CompareAndSwap below but
			// before the Store above found the slot taken and returned without kicking anything. Re-check
			// once the slot is free. Bounded — the re-kick only happens when there is genuinely new work.
			if r.contentStale.Load() {
				r.markContentStale()
			}
		}()
		for r.contentStale.CompareAndSwap(true, false) {
			// Debounce + jitter BEFORE the read, so the marks still arriving from this same pull are
			// absorbed into this pass rather than each buying their own. Aborts promptly on stop.
			//nolint:gosec // G404: load-spreading jitter, not a secret. The only thing it protects is the
			// content database from a synchronized fleet-wide read, and predicting it does not change that.
			// NOTE (#423): this used to also claim "a forged invalidation re-reads nothing whatever its
			// timing" because the refresh is version-gated. That is no longer unconditionally true — while
			// deployed rows are REJECTED the version gate cannot close (a rejection must not advance
			// snapshotContentVersion), so re-reads do happen. rejectedRereadCooldown is what bounds them now.
			if !r.sleepOrStop(r.refreshDebounce + time.Duration(rand.Int63n(int64(r.refreshJitter)+1))) {
				return // the reloader stopped — refreshing a torn-down shard is pointless
			}
			r.refreshContentSnapshot()
		}
	}()
}

// sleepOrStop waits d, returning false if the reloader stopped first. It is how the refresh loop stays
// promptly cancellable across a debounce that is deliberately longer than a shutdown should take.
func (r *reloader) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.retryDone:
		return false
	case <-t.C:
		return true
	}
}

// refreshContentSnapshot re-reads the shard's enabled packs and publishes the result as the shard's live
// content snapshot (#418). It is the ONLY writer of s.content after construction.
//
// It re-reads through content.LoadPacksWithCore + content.Merge — the two halves of the SAME
// content.LoadWithCore call cmd/telos-world uses at boot — so the refreshed snapshot has the identical
// shape and layering the process booted with: the embedded core bootstrap pack merged UNDER the enabled
// packs, last-write-wins by ref. Assembling it any other way would make a runtime-built zone differ from a
// boot-built one in ways nothing tests. The split exists so the packs can be VALIDATED between the read and
// the merge (#423) without reading twice.
//
// Every failure is non-fatal: the previous snapshot stays published. Serving slightly stale content is the
// bug this fixes, but serving NO content would break HostZone and every mint outright, so a Postgres blip
// must not be allowed to escalate.
//
// # What a rejected/failed refresh actually costs, and the divergence it accepts
//
// Three call sites read this snapshot: HostZone (a zone built fresh after a rebalance/standby adoption),
// MintInstance, and regionForZone. The per-ref applier — what propagates a live content EDIT to a running
// zone — re-reads per ref and never touches the snapshot.
//
// So state the freeze PRECISELY, because "it blocks every pack's deploy" is the wrong reading and it is the
// one a reviewer reaches for. A staff `reload` of an unrelated pack still succeeds and still propagates
// fleet-wide during a freeze: its per-ref invalidations swap the prototype cache, and the zone-shape
// reconcile converges off the WIRE payload with no snapshot read. Room, item, mob and channel edits all go
// live. What a freeze actually holds back is the zone-graph half: a NEW zone cannot be hosted, the
// `instanceable` opt-in cannot take effect, start-room/reset changes do not reach zones built fresh after
// the freeze, and region membership does not update for zones registered after it.
//
// A frozen snapshot can never LOSE a zone (setContent stores a whole new snapshot or nothing, and the
// prototype cache has no delete), so no currently-hosted zone becomes unhostable and nobody playing is
// disconnected. The one real player-facing edge is a stalled drain, and it needs the uptime split below to
// exist at all: if a RESTARTED shard hosts a zone that only exists in the post-freeze rows, a frozen peer
// refuses to adopt it (HostZone -> errNoZoneContent -> FailedPrecondition), so that zone cannot be
// evacuated. Not a dupe and not a disconnect — a drain that cannot complete for one zone, which is one more
// reason the boot-time Error is the alertable signal.
//
// The honest cost is a divergence that does NOT self-heal: boot reads rows raw and unvalidated, so a shard
// restarted while bad rows are deployed serves them, while a long-running shard rejects and stays behind.
// Pre-#423 the divergence was transient (everyone converged on newest); now it persists until a human fixes
// the rows. That is accepted because the alternative is auto-deploying content the operator was told was
// rejected — and it is made survivable by boot logging the same findings at Error (ReportBootContentProblems, in
// bootvalidate.go), which is the alertable signal that says a fleet is now split by uptime. Boot logs and boots
// anyway rather than refusing: it has no previous snapshot to fall back on, so refusing would turn a content
// defect into an outage.
func (r *reloader) refreshContentSnapshot() {
	src, ok := r.src.(content.Source)
	if !ok {
		// An embedded/mem DefinitionSource that cannot re-read whole packs. Logged rather than silent: a
		// production source is meant to be a *store.Pool, and a decorator that dropped LoadPacks would
		// disable the refresh entirely with a green test suite — the same seam the contentVersionBumper
		// SEAM WARNING above documents.
		r.log.Debug("content snapshot refresh skipped: the source cannot re-read whole packs", "packs", r.enabled)
		return
	}
	// The refresh outlives nothing: bound it by BOTH the read timeout and the reloader's stop signal, so a
	// shutdown does not leave a full-content read against Postgres running for another 30 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), reloadRepublishTimeout)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-r.retryDone:
			cancel()
		case <-stop:
		}
	}()

	// VERSION GATE. Read the authority's version FIRST, and skip the whole-pack read when the published
	// snapshot already reflects it. Two things depend on this:
	//
	//   - It makes a replayed or FORGED invalidation inert. The content bus carries unsigned JSON and its
	//     pack filter accepts an empty pack from anybody, so without this gate an attacker who can publish
	//     to the subject could drive every shard in the fleet into back-to-back full-content reads against
	//     the content database, indefinitely, for the cost of one small message.
	//   - It removes the redundant read a coalesced storm would otherwise still pay for.
	//
	// A source with no version authority (embedded/mem in dev) has no gate and always re-reads, which is
	// correct there: no authority means no way to know, and the cost is a small in-memory pack.
	// REJECTED-VERSION COOLDOWN (#423). A rejection deliberately does NOT advance snapshotContentVersion —
	// it must not, or the gate above would treat the refused version as published and the shard would stop
	// re-reading, so an operator's fix would never be picked up. But leaving it at that reopens exactly the
	// amplification lever the gate exists to close: while broken rows are deployed, EVERY content event
	// re-reads the whole content database on every shard, indefinitely, and the attacker can create that
	// precondition with the same one-row write. The cooldown bounds it: a version already rejected is
	// re-read at most once per rejectedRereadCooldown. An in-place row fix (which does not bump the version)
	// still converges, just within the cooldown rather than instantly — the right trade, since the
	// alternative is an unbounded read multiplier on the content database.
	before, versioned := r.currentContentVersion(ctx)
	if versioned && before != 0 && before <= r.snapshotContentVersion.Load() {
		r.log.Debug("content snapshot refresh skipped: already at the current content version",
			"version", before, "packs", r.enabled)
		return
	}
	if versioned && before != 0 && before == r.rejectedContentVersion.Load() && !r.rejectedRereadDue() {
		r.log.Debug("content snapshot refresh skipped: this version was already rejected and is in its "+
			"re-read cooldown", "version", before, "packs", r.enabled)
		return
	}

	// ONE read, then validate-then-merge over that same slice (#423). LoadPacksWithCore is LoadWithCore
	// stopped one step early, so the packs gated below are byte-identically the packs published. Reading
	// twice — once to validate, once to publish — would reintroduce the TOCTOU this gate exists to close.
	packs, err := content.LoadPacksWithCore(ctx, src, r.enabled)
	if err != nil {
		r.log.Warn("content snapshot refresh failed; runtime zone builds keep the previous content",
			"packs", r.enabled, "err", err)
		return
	}
	// THE PUBLISH GATE (#423). Before #423 this function published raw Postgres rows as the shard's live
	// content with nothing checking them, while the staff `reload` command's identical publish WAS gated by
	// validatePacks. That asymmetry meant a reload the operator saw REJECTED ("nothing propagated") still
	// went live on every shard at the next unrelated content event — rows are written by seed/import BEFORE
	// any reload runs, so the refresh simply re-read and deployed them. Gating here makes "rejected" mean
	// rejected on both publish paths.
	//
	// SCOPE = every enabled pack, core EXCLUDED — the same rule republish states for itself ("Core is
	// context-only, never in scope", reloadcmd.go). Core is embedded and compiled in; a finding there is an
	// engine bug, and letting one freeze every shard's snapshot fleet-wide is the opposite of the
	// degraded-but-bootable posture the core pack exists to guarantee.
	//
	// ONE BROKEN PACK REJECTS THE WHOLE SNAPSHOT, and that is deliberate. The alternative — drop the
	// offending pack and merge the rest — is mechanically easy and semantically wrong: it silently PROMOTES
	// whatever definitions the dropped pack was overriding, and those are exactly the ones validatePacks
	// skipped as inert (its provenance model is last-writer-wins). A partial merge would publish content the
	// gate never looked at, and would build a graph no boot would ever produce — which this function's own
	// contract forbids.
	scope := r.snapshotScope()
	reject, warn := validateSnapshotPacks(packs, scope)
	for _, w := range warn {
		// Detection is not reduced by the narrowing — these are still computed and still surfaced, they
		// simply do not hold a veto over the zone graph. Boot reports the same findings at Error.
		r.log.Warn("content snapshot: a shared-def problem in the deployed content (NOT blocking the "+
			"snapshot — these kinds are registered at boot only, or hot-swap via their own path, so the "+
			"snapshot cannot deploy them; a rolling reboot would)", "packs", r.enabled, "problem", w)
	}
	if len(reject) > 0 {
		r.logSnapshotRejection(before, versioned, reject)
		return
	}
	lc := content.Merge(packs)
	prev := r.shard.liveContent()
	// An EMPTY result is not a refresh, it is a wipe. LoadPacks matches on `pack = ANY($1)` and returns no
	// rows and NO ERROR for a pack that was pruned or renamed, so a bad deploy could otherwise replace a
	// good snapshot with nothing — and an empty snapshot makes every runtime zone build refuse. Keeping the
	// previous one is the recoverable direction; the loud log is the operator's signal.
	//
	// Counted OUTSIDE the core namespace, which is the whole reason this is not lc.Empty(). LoadWithCore
	// always layers the embedded bootstrap pack, so a snapshot that lost every real zone still reports one
	// and Empty() is never true here — the obvious guard would have been dead code in production while
	// looking correct in a test that skipped the core layering.
	if realZones(lc) == 0 && realZones(prev) > 0 {
		r.log.Error("content snapshot refresh read ZERO non-core zones for packs that previously had them; "+
			"keeping the previous snapshot (check that the enabled packs still exist in the content database)",
			"packs", r.enabled, "previous_zones", realZones(prev))
		return
	}
	r.shard.setContent(lc)
	r.snapshotContentVersion.Store(before)
	r.log.Info("content snapshot refreshed; new zone builds and instance mints use the current content",
		"packs", r.enabled, "zones", len(lc.Zones), "previous_zones", zoneCountOf(prev), "version", before)

	// TORN-READ DETECTION. LoadPacks issues independent statements per table with no enclosing
	// transaction, so an import committing mid-read yields a snapshot mixing two versions. Re-read the
	// version afterwards: if it moved, the snapshot we just published may be spliced, so mark stale again
	// and let the loop re-read. Publishing first and repairing after is deliberate — a spliced snapshot is
	// still strictly newer than the one it replaced, and buildZone refuses anything incoherent in the gap.
	if after, ok := r.currentContentVersion(ctx); ok && after != before {
		r.log.Warn("the content version moved during the snapshot read; the snapshot may mix two versions, "+
			"re-reading", "read_at", before, "now", after)
		r.contentStale.Store(true)
	}
}

// snapshotScope is the validatePacks provenance scope for a snapshot refresh: every pack this shard
// enables, with the embedded CORE pack deliberately absent.
//
// A `reload <pack>` scopes narrowly so an unrelated broken pack cannot block it (#205). A refresh has no
// such narrowing available — it publishes ONE merged snapshot assembled from every enabled pack, so every
// non-core pack contributes to what goes live and every non-core finding is rejectable. Core is excluded
// for the reason republish excludes it: it is compiled-in content the operator cannot fix by editing rows,
// so a core finding must never be able to wedge the refresh permanently.
func (r *reloader) snapshotScope() map[string]bool {
	scoped := make(map[string]bool, len(r.enabled))
	for _, p := range r.enabled {
		if p != content.CorePack {
			scoped[p] = true
		}
	}
	return scoped
}

// logSnapshotRejection reports a refused snapshot ONCE PER CONTENT VERSION, at Error the first time and
// at Debug for every later refresh that re-reads the same broken rows.
//
// The memo matters because the refresh is driven by content EVENTS, not by the broken pack: once bad rows
// are in Postgres, every unrelated invalidation fleet-wide re-reads and re-rejects them. Without the memo
// a single bad deploy turns into an Error per shard per content event, which is how a real signal gets
// tuned out. Version 0 / an unversioned source has nothing to memo on, so it logs every time — correct
// there, since those are dev sources with no fleet to spam.
func (r *reloader) logSnapshotRejection(version uint64, versioned bool, problems []string) {
	const msg = "content snapshot refresh REJECTED: the deployed content failed validation, so new zone " +
		"builds and instance mints KEEP THE PREVIOUS SNAPSHOT (a shard restarted now would read these rows " +
		"raw and serve different content — fix the rows, then re-deploy)"
	r.rejectedContentVersion.Store(version)
	r.rejectedAtNanos.Store(time.Now().UnixNano())
	// Memoize on the PROBLEM SET, not on the version. A raw row edit does not bump content_version (only an
	// import or a staff reload does), so a version-keyed memo would report the first breakage at Error and
	// then silently downgrade every LATER, DIFFERENT breakage at that same version to Debug — an attacker
	// could land a benign typo to burn the memo and follow it with a real one nobody is paged about.
	if versioned && version != 0 && r.rejectedProblemSet.Swap(rejectionKey(version, problems)) == rejectionKey(version, problems) {
		r.log.Debug(msg, "packs", r.enabled, "version", version, "problems", problems)
		return
	}
	r.log.Error(msg, "packs", r.enabled, "version", version, "problems", problems)
}

// rejectionKey identifies a (version, problem-set) pair for the log memo. Order-independent so a map-
// iteration reshuffle in a validator cannot masquerade as a new problem set and re-page the operator.
func rejectionKey(version uint64, problems []string) uint64 {
	sorted := append([]string(nil), problems...)
	sort.Strings(sorted)
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d\x00", version)
	for _, p := range sorted {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// rejectedRereadDue reports whether a version already rejected may be re-read again. See the cooldown note
// in refreshContentSnapshot: it bounds the read amplification a broken deploy would otherwise hand an
// attacker, while still letting an in-place row fix converge without a restart.
func (r *reloader) rejectedRereadDue() bool {
	last := r.rejectedAtNanos.Load()
	return last == 0 || time.Since(time.Unix(0, last)) >= rejectedRereadCooldown
}

// currentContentVersion reads the content authority's current version. ok is false when the source has no
// authority (the embedded/mem dev sources) or the read failed — callers treat that as "cannot tell" and
// proceed, never as "unchanged", so a version-read blip degrades to a redundant refresh rather than a
// silently skipped one.
func (r *reloader) currentContentVersion(ctx context.Context) (uint64, bool) {
	vs, ok := r.src.(contentVersioner)
	if !ok {
		return 0, false
	}
	v, err := vs.ContentVersion(ctx)
	if err != nil {
		r.log.Debug("content version read failed; refreshing without the version gate", "err", err)
		return 0, false
	}
	return v, true
}

// zoneCountOf is a nil-safe zone count for the refresh log line (a shard can have no previous snapshot).
func zoneCountOf(lc *content.LoadedContent) int {
	if lc == nil {
		return 0
	}
	return len(lc.Zones)
}

// realZones counts zones that are NOT part of the embedded core bootstrap pack.
//
// It is the count that answers "does this snapshot still describe a world", which is what the wipe guard
// needs. The core pack is layered under every load, so a total zone count can never reach zero and would
// make that guard unreachable.
func realZones(lc *content.LoadedContent) int {
	if lc == nil {
		return 0
	}
	n := 0
	for i := range lc.Zones {
		ref := lc.Zones[i].Ref
		if ref == content.CoreZone || strings.HasPrefix(ref, content.CoreRefPrefix) {
			continue
		}
		n++
	}
	return n
}

// onInvalidation is the bus handler: it runs OFF every zone goroutine (the bus's subscription
// goroutine), serially per subscription. It filters by pack, re-reads the single definition,
// rebuilds the prototype, and swaps it into the cache. Every failure is non-fatal (logged):
// hot reload is best-effort and never disturbs the running world beyond the one ref it targets.
func (r *reloader) onInvalidation(inv contentbus.Invalidation) {
	if !r.accepts(inv) {
		r.log.Debug("invalidation ignored", "kind", inv.Kind, "ref", inv.Ref, "pack", inv.Pack)
		return
	}
	r.log.Debug("invalidation received", "kind", inv.Kind, "ref", inv.Ref, "pack", inv.Pack)

	// The version-complete SENTINEL (#212 slice 4 PR D): a versioned pull's LAST invalidation, carrying
	// only the version. Advancing appliedContentVersion ONLY here — not on every per-ref message — is what
	// makes a PARTIALLY-delivered pull safe: a prefix-then-drop never reaches the sentinel, so applied
	// stays behind and reconcile-on-join re-applies the rest on reconnect. It has no ref to swap. (A
	// shard-local `reload` / dev seed does NOT emit it, so a hot-edit's nanos version never poisons the
	// cursor — only a real versioned pull advances it.)
	if inv.Kind == content.KindVersionComplete {
		r.advanceApplied(inv.Version)
		// A completed pull is the ONE signal that covers a zone DELETION (#418): a zone dropped from a pack
		// emits no KindZone invalidation of its own — PublishPack loops over the zones that are PRESENT — so
		// the sentinel is the only edge on which a shard can learn the template is gone.
		r.markContentStale()
		return
	}

	// Phase 8.3: a `channel` invalidation reloads a pack-GLOBAL channel_def into the per-shard channel
	// REGISTRY (the atomic-swap defRegistry), not the prototype cache. It is a different swap target
	// (a channelDef, not a *Prototype) and a different table, so it forks off here before the
	// prototype path. Channel verbs are derived from the registry on each dispatch (channelForVerb), so
	// a verb added/removed by the edit takes effect with no second swap.
	if inv.Kind == content.KindChannel {
		r.reloadChannel(inv)
		return
	}

	// A `zone` invalidation carries no spawnable prototype — it drives the live-room-SHAPE reconcile
	// (#191): it carries the zone's DESIRED room set + start room on the wire, so the hosting zone
	// converges its live room graph (spawn ADDs, resync UPDATEs, tear down DELETIONs) off the
	// already-swapped cache with no source re-read. It forks off before the prototype path, like the
	// channel case.
	if inv.Kind == content.KindZone {
		// The zone-shape edit converges the LIVE zone; the snapshot refresh converges what the NEXT
		// runtime-built zone (a HostZone after a rebalance, every instance mint) is assembled from (#418).
		// Marked here and not only on the sentinel because a shard-local staff `reload` never emits one.
		r.markContentStale()
		r.reconcileZone(inv)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), reloadIOTimeout)
	defer cancel()
	def, err := r.src.LoadDefinition(ctx, inv.Kind, inv.Ref, inv.Pack)
	if err != nil {
		// Infrastructure failure on the re-read: keep the last-known prototype (do NOT drop it on
		// a transient read error — that would empty the ref on a Postgres blip). The next
		// invalidation retries.
		r.log.Warn("hot reload re-read failed; keeping last-known prototype",
			"kind", inv.Kind, "ref", inv.Ref, "err", err)
		return
	}

	ref := ProtoRef(inv.Ref)
	if !def.Found {
		// The row was deleted/renamed: remove the entry so a later spawn of this ref returns nil
		// (logged as unknown) rather than serving a now-orphaned prototype. Live instances already
		// spawned keep their aliased prototype (GC holds it) — they are unaffected, by design.
		r.cache.reload(ref, nil)
		r.log.Debug("hot reload: prototype removed (definition deleted)", "kind", inv.Kind, "ref", inv.Ref)
		r.notifyZones(inv.Kind, inv.Ref) // a deleted scripted proto → its live instances go scriptless
		return
	}

	p := buildPrototype(def)
	if p == nil {
		r.log.Warn("hot reload: unbuildable definition, skipped", "kind", inv.Kind, "ref", inv.Ref)
		return
	}
	// The atomic whole-table swap (prototype.go reload): the new prototype is published in one
	// step; concurrent spawns see the old or the new table, never a half-applied map. The old
	// prototype stays alive while any live instance aliases it.
	r.cache.reload(ref, p)
	r.log.Debug("hot reload: prototype swapped", "kind", inv.Kind, "ref", inv.Ref)
	// Notify each hosted zone to apply the LUA reload on its own goroutine (7.7): recompile the
	// chunk, re-register live instances' handlers (keeping self.state), bump the timer generation,
	// reset the breaker. The shared-cache swap above is the cross-goroutine-safe publish; the
	// per-zone Lua state is updated single-writer via the inbox post.
	r.notifyZones(inv.Kind, inv.Ref)
}

// accepts is the content-bus admission decision: does this shard act on inv at all? It is a PURE
// predicate, split out from onInvalidation so the decision is table-testable over every kind — the
// re-widening regression this guards against is one line, and one line is exactly what slips back in.
//
// # Fail CLOSED on an empty pack (#424)
//
// The rule was `inv.Pack != "" && !r.packs[inv.Pack]` — i.e. an invalidation naming NO pack was accepted
// by EVERY shard regardless of which packs it loads. The bus is unsigned JSON on one NATS subject with no
// publisher identity, so that handed anyone who could publish a KNOWLEDGE-FREE, FLEET-WIDE primitive: no
// need to know a single pack name.
//
// It was also DESTRUCTIVE, which is the part that makes this more than tidying. The applier resolves a
// definition with `WHERE ref = $1 AND pack = $2`, so an empty pack never matches a row — the re-read comes
// back Found:false, and Found:false is the DELETION path: the prototype is evicted from the shard's cache
// (no further spawns, live scripted instances go scriptless) or the channel is dropped from the registry
// (its verb stops resolving, live subscribers lose it). Fleet-wide, per ref, freely repeatable, with no
// version guard anywhere on that path.
//
// It is TEMPTING to add "and naming a real pack does the opposite, since the re-read succeeds and the swap
// is an idempotent no-op". That is FALSE, and the false version was in this comment until a review probed
// it. The re-read dispatches on KIND as well as (ref, pack), and returns Found:false for a kind that does
// not match the target's actual kind — so `{kind:"item", ref:"<a room ref>", pack:"<a loaded pack>"}` still
// evicts that room, fleet-wide, for the cost of one public pack name. The kind whitelist in accepts closes
// the unknown-kind half of that; the kind-MISMATCH half is not closable here (an item invalidation for an
// item ref is exactly what a legitimate publish looks like) and is closed only by authenticating the bus.
//
// # What this does NOT buy
//
// Not authentication. An attacker who names a pack the shard actually loads — and pack names are hardly
// secret; they are in the content repo, the reload readout, and the logs — still reaches the zone-shape
// reconcile, the channel registry swap, and the per-ref re-read. Authenticating the bus is NATS subject
// permissions (deployment-side) and, if that proves insufficient, signing. This is blast-radius reduction:
// it removes the primitive that needs no knowledge at all.
//
// # The one legitimate pack-less publisher
//
// PublishVersionComplete emits a content-LESS sentinel (kind + version only), and every shard must process
// it — it is what advances the applied-version cursor and is the only signal covering a zone DELETION. So
// it is exempted, but STRUCTURALLY: a sentinel carries no ref, no room set and no start room by
// construction, and one that does is not a sentinel. Requiring that here costs nothing and stops the
// exemption from being usable as a carrier for content a future refactor might start reading off it.
//
// RESIDUAL, stated plainly: the sentinel exemption is still forgeable, and advanceApplied is monotone with
// no upper bound. A forged sentinel carrying a max version wedges appliedContentVersion, which makes
// reconcile-on-join's `cur <= applied` gate permanently true — the shard then silently never catches up
// after a bus gap. The snapshot refresh is NOT affected (it gates on the source's own version, #418). That
// residual is bounded by subject permissions, not by anything expressible here.
func (r *reloader) accepts(inv contentbus.Invalidation) bool {
	// A kind outside the closed wire vocabulary is REJECTED rather than passed through. An unknown kind
	// reaches LoadDefinition, whose store dispatch returns Found:false for anything it does not recognise,
	// and Found:false is the DELETION path — so `{kind:"made-up", ref:"<real ref>", pack:"<loaded pack>"}`
	// evicted that prototype from every shard. Whitelisting the vocabulary closes that without needing to
	// know anything about the ref.
	if !content.KnownKind(inv.Kind) {
		return false
	}
	if inv.Kind == content.KindVersionComplete {
		return inv.Pack == "" && inv.Ref == "" && len(inv.Rooms) == 0 && inv.StartRoom == ""
	}
	// Everything else MUST name a pack this shard loads. The explicit `inv.Pack != ""` is NOT redundant
	// with the map lookup, and removing it would silently undo this whole change: r.packs is built by
	// ranging the configured enabled-pack list, and an empty string in that list (reachable — a YAML
	// `content_packs: [reference, '']` survives config load, unlike the env path which drops empties) would
	// put a "" key in the map and make every pack-less invalidation acceptable again, with nothing logged.
	// The property should not depend on the contents of a config-derived map.
	//
	// Two rejections fold together here: an empty pack (fail closed) and a pack another shard loads but
	// this one does not (a real no-op — a shard only caches prototypes from its enabled packs).
	return inv.Pack != "" && r.packs[inv.Pack]
}

// reloadChannel applies a `channel` content hot reload (Phase 8.3): it re-reads the single edited
// channel_def, rebuilds the runtime channelDef via the SAME mapper the boot loader uses
// (buildChannelDef), and SWAPS it into the per-shard channel registry race-safely (defRegistry.reload
// — the atomic whole-table swap). A deleted channel (Found=false) is REMOVED from the registry, so its
// verb stops resolving and a speak attempt falls through to "Huh?". Every failure is non-fatal
// (logged): hot reload is best-effort and never disturbs the running world beyond the one channel.
//
// It runs OFF every zone goroutine (the content-bus subscription goroutine), serially per
// subscription, and the registry swap is the cross-goroutine-safe publish (a zone goroutine reading
// channelForVerb sees the old or the new table whole). Channel verbs are DERIVED from the registry on
// each dispatch, so a verb the edit added/removed/renamed takes effect with no further notification —
// nothing per-zone to re-register (unlike a scripted prototype's Lua handlers).
func (r *reloader) reloadChannel(inv contentbus.Invalidation) {
	if r.shard == nil || r.shard.defs == nil {
		// A bare reloader (a test without a shard) has no channel registry to swap; nothing to do.
		return
	}
	reg := r.shard.defs.channel
	if reg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reloadIOTimeout)
	defer cancel()
	def, err := r.src.LoadDefinition(ctx, inv.Kind, inv.Ref, inv.Pack)
	if err != nil {
		// Infrastructure failure on the re-read: keep the last-known channel_def (do NOT drop it on a
		// transient read error). The next invalidation retries.
		r.log.Warn("hot reload re-read failed; keeping last-known channel", "ref", inv.Ref, "err", err)
		return
	}
	if !def.Found {
		// The channel was deleted/renamed: remove it so its verb stops resolving.
		reg.reload(inv.Ref, nil, true)
		r.log.Debug("hot reload: channel removed (definition deleted)", "ref", inv.Ref)
		r.republishCommsToZones(inv.Ref) // a removed channel must drop live subscribers' subscriptions (#75)
		return
	}
	reg.reload(inv.Ref, buildChannelDef(def.Channel), false)
	r.log.Debug("hot reload: channel swapped", "ref", inv.Ref)
	// #75: the registry now holds the new access/hear_access, but live sessions' hear-sets are PUSHED — a
	// retightened channel leaves an already-subscribed player over-permissive until their next
	// toggle/handoff/relog. Fan a republish out to every hosted zone so the gate re-filters now.
	r.republishCommsToZones(inv.Ref)
}

// republishCommsToZones fans a republishCommsMsg out to every hosted zone after a channel_def reload, so each
// zone re-publishes its players' comms config on its own goroutine (#75). Runs OFF every zone goroutine (the
// content-bus subscriber). The fan-out is NON-BLOCKING (postOrDrop) so one saturated zone can't head-of-line-
// stall the shard-wide reload — but unlike the Lua reload (notifyZones), a DROPPED comms republish leaves the
// hear-set PERMANENTLY stale (it is pushed, not pulled) and RE-OPENS the security gap, so a drop is handed to
// bounded-retry (retryRepublishComms), the same posture the zone-shape reconcile uses (#191). Idempotent +
// level-triggered, so the retry (or a re-run) always converges to the current state.
//
// COALESCED (#269): republishAllComms recomputes every player's FULL hear-set and is ref-INDEPENDENT, so a
// pack edit touching K channel_defs emits K serial KindChannel invalidations → K identical republishes per
// zone, where one suffices. commsRepublishArmed collapses them: a zone with a republish already queued (or in
// retry) is skipped, so K invalidations cost ONE republish per zone.
//
// Convergence is safe because the SINGLE surviving message is processed LATER, on the zone goroutine, and
// reads the CURRENT channel registry — which by then holds every coalesced edit's swap. The flag CAS orders
// it: for a later edit to be skipped, its reg.reload must precede its CAS(fail), which precedes the zone's
// disarm, which precedes the zone's registry read. So the republish reflects all coalesced edits, not just the
// first. The zone handler disarms the flag before recomputing, so an edit landing mid-republish re-arms and
// gets its own pass.
// This also collapses the retry fan-out from K:Z down to 1:Z, bounding how much of the comms republish retry
// budget (its OWN commsRetryInFlight since #345, no longer shared with reconcile) a KindChannel drop-storm can
// consume — at most one retry goroutine per hosted zone (the caveat retryRepublishComms records).
func (r *reloader) republishCommsToZones(ref string) {
	if r == nil || r.shard == nil {
		return
	}
	for _, z := range r.shard.zonesList() { // mu-guarded: safe against a runtime HostZone (16.4a)
		if !z.commsRepublishArmed.CompareAndSwap(false, true) {
			continue // a republish is already queued for this zone; ref-independent, so it covers this edit too
		}
		if !z.postOrDrop(republishCommsMsg{ref: ref}) {
			r.retryRepublishComms(z, ref)
		}
	}
}

// reconcileZone drives a `zone` content hot reload (#191): the KindZone invalidation carries the zone's
// DESIRED room-ref set + start room + a monotonic version, so this simply posts that desired state to the
// hosting zone as a reconcileZoneMsg — the diff-and-converge (spawn ADDs, resync UPDATEs, tear down
// DELETIONs, apply the start_room change) runs single-writer ON THE ZONE GOROUTINE. It runs OFF every zone
// goroutine (the content-bus subscription goroutine) and does NO source re-read: the payload travels on
// the wire, and because the contentbus is a single ordered subject, this shard already applied the pack's
// per-ref prototype cache swaps (delivered before this trailing KindZone invalidation) — so the zone
// reconciles ADDs/UPDATEs off the already-swapped cache.
//
// Only a shard HOSTING this zone reconciles it (the reconcile is a no-op elsewhere). Delivery is the same
// non-blocking postOrDrop the rest of hot reload uses — a wedged zone inbox must not head-of-line-stall
// every later invalidation shard-wide — but a dropped reconcile is worse than a dropped Lua reload (a lost
// REMOVE leaves a ghost room), so a drop is handed to bounded-retry delivery (retryReconcile) rather than
// merely warned. The reconcile is level-triggered + idempotent, so a retry (or a re-run) converges.
func (r *reloader) reconcileZone(inv contentbus.Invalidation) {
	if r == nil || r.shard == nil {
		return
	}
	// A local, unleased bootstrap zone (#212 core pack) is embedded-only and never reloaded — never
	// let a KindZone invalidation naming it drive a shape-reconcile (which tears down rooms not in the
	// desired set) against the live lobby on every shard. Defense-in-depth on top of the reserved-
	// namespace lint: a core-namespace ref should never reach the broadcast, but fail safe if one does.
	if r.shard.isLocalZone(inv.Ref) {
		return
	}
	msg := reconcileZoneMsg{
		zoneRef:   inv.Ref,
		version:   inv.Version,
		rooms:     inv.Rooms,
		startRoom: ProtoRef(inv.StartRoom),
	}
	for _, z := range r.shard.zonesList() { // mu-guarded against a runtime HostZone (16.4a)
		if z.isInstance() {
			// FROZEN (#411): a live instance is PINNED to the content it was minted from and never reconciles.
			// Note this is an EXPLICIT refusal, not the `z.id != inv.Ref` mismatch below doing the work by
			// accident — an instance's id is `<template>#<serial>`, so it never equals a ref and the loop
			// happens to skip it. That accident is exactly the kind of thing a later "match by template"
			// refactor removes without noticing.
			//
			// WHY FREEZE. A reconcile is a diff-and-CONVERGE that tears down rooms absent from the desired
			// set. Applied to a zone with a party inside it, a builder's mid-run edit deletes the room they
			// are standing in. And "the run you started is the run you finish" is the instanced semantic
			// anyway: an instance is one bounded pass over a fixed room graph, and it is short-lived, so the
			// edit lands on the next mint — seconds to minutes away — with nobody's run corrupted.
			if z.template == inv.Ref {
				r.log.Debug("zone-shape reconcile withheld from a pinned zone instance; it keeps the content it "+
					"was minted from until it is reaped", "zone", z.id, "template", z.template, "version", inv.Version)
			}
			continue
		}
		if z.id != inv.Ref {
			continue
		}
		if !z.postOrDrop(msg) {
			r.retryReconcile(z, msg) // bounded-retry the dropped reconcile off this goroutine (#191 PR 3/3)
		}
		return // a zone is hosted by at most one shard-local Zone; done once matched
	}
}

// retryReconcile re-posts a DROPPED reconcileZoneMsg to z with bounded linear backoff, on a short-lived
// goroutine so the subscriber goroutine is never blocked (#191 PR 3/3 / #194). It re-posts the SAME
// immutable msg (original version) — the version guard makes that safe: a dropped reconcile never advanced
// z.lastReconciledPackVer, so the retry re-applies, and a retry superseded by a newer reload is harmlessly
// dropped by the guard. Concurrent retries are capped (maxReconcileRetryGoroutines) so a reload storm on a
// saturated shard can't spawn unbounded goroutines; past the cap the drop is abandoned (re-run remedy). The
// goroutine self-terminates on success, on attempt exhaustion, or when the reloader stops (retryDone).
func (r *reloader) retryReconcile(z *Zone, msg reconcileZoneMsg) {
	if r.retryInFlight.Add(1) > maxReconcileRetryGoroutines {
		r.retryInFlight.Add(-1)
		r.log.Warn("hot-reload zone-shape reconcile dropped (retry budget exhausted); a room add/remove is lost until re-run",
			"zone", msg.zoneRef)
		return
	}
	go func() {
		defer r.retryInFlight.Add(-1)
		for attempt := 1; attempt <= reconcileRetryAttempts; attempt++ {
			select {
			case <-r.retryDone:
				return // reloader stopped — abandon the retry (posting to a torn-down shard is pointless)
			case <-time.After(reconcileRetryBackoff * time.Duration(attempt)):
			}
			if z.postOrDrop(msg) {
				r.log.Debug("zone-shape reconcile retry delivered", "zone", msg.zoneRef, "attempt", attempt)
				return
			}
		}
		r.log.Warn("hot-reload zone-shape reconcile retries exhausted; a room add/remove is lost until re-run",
			"zone", msg.zoneRef, "attempts", reconcileRetryAttempts)
	}()
}

// retryRepublishComms re-posts a DROPPED republishCommsMsg to z with bounded linear backoff on a short-lived
// goroutine, so the content-bus subscriber goroutine is never blocked (#75). It mirrors retryReconcile's
// posture because the failure mode is the same class — a dropped push that re-opens a gap (here: a stale,
// too-permissive hear-set) — but the message is level-triggered + carries no version, so a re-post always
// republishes the CURRENT config and a retry superseded by a newer channel reload is harmlessly redundant
// (no version guard needed, unlike reconcile).
//
// Retry EXHAUSTION degrades to exactly the pre-fix behavior (the hear-set is re-pushed on the player's next
// toggle/handoff/relog) — a strict Pareto improvement, never worse than before. Two caveats the distsys
// review recorded: (a) the operator "re-run the reload" remedy is weaker than reconcile's — re-applying an
// IDENTICAL edit may emit no KindChannel invalidation, so it can be a no-op; a genuinely stuck hear-set clears
// only on the player's next toggle/handoff/relog. (b) A channel reload fans to every hosted zone, but the
// coalescing flag (#269) collapses a K-channel edit to ONE republish (and so at most ONE retry) per zone.
//
// Retry budget (#345): comms republish draws from its OWN commsRetryInFlight counter, SEPARATE from the
// reconcile budget (retryInFlight), each capped independently (both 128). #269 already collapsed a K-channel
// burst to 1:Z republishes; the independent counter means neither a comms-republish storm nor a zone-shape
// reconcile storm can exhaust the other's retry-GOROUTINE capacity (they guard different failure classes — a
// too-permissive hear-set vs a ghost room). NOTE the independence is at the goroutine-budget layer only: both
// classes still post into the SAME per-zone inbox, so within one saturated hot zone a reconcile storm can
// still make a comms postOrDrop to that zone fail (and burn its attempts) — separate budgets decouple the
// shard-wide goroutine pools, not per-zone inbox delivery. Past the cap the drop is abandoned with a loud
// warn. Self-terminates on success, exhaustion, or reloader stop (retryDone).
func (r *reloader) retryRepublishComms(z *Zone, ref string) {
	// The coalescing flag (#269) stays ARMED across the retry: it means "a republish is queued OR being
	// retried for this zone", so a concurrent channel edit correctly coalesces onto this in-flight one rather
	// than racing a second delivery. Whoever stops carrying the republish must disarm it — the zone handler on
	// a successful delivery, or this goroutine if it abandons the drop — or the flag would latch and suppress
	// every future republish to this zone.
	if r.commsRetryInFlight.Add(1) > maxCommsRepublishRetryGoroutines {
		r.commsRetryInFlight.Add(-1)
		z.commsRepublishArmed.Store(false)
		r.log.Warn("hot-reload comms republish dropped (retry budget exhausted); this zone's comms hear-sets "+
			"stay stale until re-run (a coalesced republish covers every channel edited in the burst, not just this one)",
			"zone", z.id, "channel", ref)
		return
	}
	go func() {
		defer r.commsRetryInFlight.Add(-1)
		for attempt := 1; attempt <= reconcileRetryAttempts; attempt++ {
			select {
			case <-r.retryDone:
				z.commsRepublishArmed.Store(false) // reloader stopped mid-retry — release the flag
				return
			case <-time.After(reconcileRetryBackoff * time.Duration(attempt)):
			}
			if z.postOrDrop(republishCommsMsg{ref: ref}) {
				// Delivered: the zone handler disarms the flag when it processes the message. Do NOT disarm
				// here, or a coalesced edge could re-arm and double-post before the handler runs.
				r.log.Debug("comms republish retry delivered", "zone", z.id, "channel", ref, "attempt", attempt)
				return
			}
		}
		z.commsRepublishArmed.Store(false) // exhausted: no message will land, so release the flag for a future reload
		r.log.Warn("hot-reload comms republish retries exhausted; this zone's comms hear-sets stay stale until "+
			"re-run (the coalesced republish covers every channel edited in the burst)",
			"zone", z.id, "channel", ref, "attempts", reconcileRetryAttempts)
	}()
}

// reloadLua applies a content Lua hot reload for (kind, ref) ON THE ZONE GOROUTINE (slice 7.7,
// P7-D7 / §1.1). The shard reloader already swapped the new prototype/def into the shared cache;
// this re-runs on each hosted zone so the per-zone Lua state (the chunk cache, the LState, the
// per-instance entityScripts) — all zone-owned — is updated single-writer. It:
//
//  1. BUMPS the chunk generation so pending old-gen mud.after timers DROP at fire (don't run old
//     code against new state; a durable=true finalizer still completes).
//  2. INVALIDATES the per-(kind,ref) shared chunk cache entry (an ability on_resolve / affect hook /
//     formula / pvp policy) so the NEXT invocation recompiles from the new source. The source-aware
//     chunkFor would recompile anyway on a changed source, but a SHARED def's source lives on the
//     swapped shared registry, not passed here — so we drop the cache entry and let the next entry-
//     point invocation re-read + recompile (the security-critical pvp_allowed case takes effect on
//     its next consult, never the stale permissive policy).
//  3. RE-REGISTERS the handlers of every LIVE instance of a scripted prototype (a mob/room with a
//     `lua` block) from the NEW source, PRESERVING each instance's self.state (the DATA survives).
//  4. RESETS the circuit breaker for that def so a script a bug had quarantined is re-enabled by the
//     fix reload.
func (z *Zone) reloadLua(kind, ref string) {
	if z.lua == nil {
		return
	}
	rt := z.lua
	rt.chunkGen++ // (1) old-gen mud.after timers drop at fire (P7-D7)

	// (2) Invalidate the shared chunk cache entries that key off this ref, so the next invocation
	// recompiles from the swapped registry source. Keys are "<kind>:<ref>:<hook>" / "<kind>:<ref>" /
	// "formula:<name>" / "pvp_allowed". We drop every cache entry that BELONGS to this ref (matched as a
	// whole colon-delimited segment, not a raw substring — #57, so reloading "orc" never drops "sorcerer")
	// plus the global policy/formula keys, and reset their breakers.
	for key := range rt.chunks {
		if keyMatchesRef(key, ref) || key == "pvp_allowed" || strings.HasPrefix(key, "formula:") {
			delete(rt.chunks, key)
			rt.breakerReset(breakerKeyShared(key)) // (4) re-enable a quarantined shared def
		}
	}

	// (3) Re-register live instances of the reloaded scripted prototype from the new source, keeping
	// their self.state. A non-prototype (ability/affect/formula) reload has no instances to walk.
	rt.reloadEntityScriptsForProto(ProtoRef(ref))

	z.log.Debug("lua hot reload applied", "kind", kind, "ref", ref, "gen", rt.chunkGen)
}

// keyMatchesRef reports whether a chunk-cache key belongs to content ref — i.e. ref appears in the key as a
// whole colon-delimited segment run, not merely as a raw substring (#57). Keys are "<kind>:<ref>[:<hook>]"
// and a ref may ITSELF contain colons (e.g. "midgaard:mob:orc"), so ref is matched bounded by ':' (or a key
// boundary) on both sides. This stops reloading ref "orc" from over-invalidating "sorcerer"/"orchard" chunk
// entries (whose keys merely contain the substring "orc"). Keys always carry a "<kind>:" prefix, so in
// practice ref matches as the interior ":<ref>:" or trailing ":<ref>" run; the bare-equal / leading forms
// are kept for completeness.
func keyMatchesRef(key, ref string) bool {
	if ref == "" {
		return false
	}
	return key == ref ||
		strings.HasPrefix(key, ref+":") ||
		strings.HasSuffix(key, ":"+ref) ||
		strings.Contains(key, ":"+ref+":")
}

// notifyZones posts a reloadLuaMsg to every hosted zone's inbox so each applies the Lua reload on
// its own goroutine (the per-zone Lua state is zone-owned). Called by the subscriber goroutine
// AFTER the shared prototype cache swap; the post is the cross-goroutine-safe hand-off (the only
// sanctioned way to reach zone state). A reloader with no shard (a bare test) skips it.
func (r *reloader) notifyZones(kind, ref string) {
	if r == nil || r.shard == nil {
		return
	}
	for _, z := range r.shard.zonesList() { // mu-guarded: safe against a runtime HostZone (16.4a)
		if z.isInstance() {
			// FROZEN (#411), the other half of the reconcile freeze above — and unlike that one this is not a
			// no-op today, it is a real behavior change. notifyZones fans out to EVERY hosted zone by id-free
			// broadcast, so without this an instance DID recompile its Lua and re-register its live handlers
			// mid-run. Combined with the shared prototype cache (an instance sets z.protos = s.protos), a
			// reload left a running dungeon with new scripts and new prototypes over the OLD room graph the
			// reconcile above correctly refused to touch — the least coherent of the three possible states.
			//
			// RESIDUAL, stated plainly, because "an instance is frozen" is NOT true — what is frozen is the
			// zone's ROOM GRAPH and its ACTIVE handler re-registration. Three shared surfaces still move
			// under a running instance, and skipping the message here is what leaves them uneven:
			//
			//  1. The protoCache is shared (an instance sets z.protos = s.protos) and is swapped by the
			//     reload. An entity SPAWNED in an instance after a reload gets the new prototype; entities
			//     already alive keep the one they alias.
			//  2. The compiled-Lua cache is only HALF frozen, and NOT in the direction the word suggests.
			//     Skipping reloadLua means rt.chunks is not flushed, no chunkGen bump (so old mud.after
			//     timers keep firing), no breaker reset, and no reloadEntityScriptsForProto walk — live
			//     entities keep their registered handlers. But chunkFor is SOURCE-AWARE: its `src` argument
			//     comes from these same shared registries, so the next call for any def whose source changed
			//     RECOMPILES from the new body. New Lua therefore does reach a running instance, lazily and
			//     one def at a time, mixed with old-generation timers.
			//  3. z.defs is the shared per-shard registry (adoptLocked points every zone at s.defs), so
			//     reloaded item / resource / ability / loot defs are visible to an instance IMMEDIATELY —
			//     nothing here touches that at all.
			//
			// Freezing content properly means per-instance pinning (its own cache generation and def
			// snapshot), which is a bigger change than this slice. What is fixed here is the destructive
			// part: the room graph a party is standing in no longer converges under them mid-run.
			continue
		}
		// NON-BLOCKING fan-out: a blocking post here would let ONE saturated (or wedged) zone inbox
		// head-of-line-stall every LATER zone's invalidation shard-wide, and a wedged zone would halt hot
		// reload entirely (distsys review) — so a full inbox DROPS the message. This carries only the LUA
		// recompile now (chunk re-read + per-instance handler re-register): a shared def (ability/formula)
		// self-heals on next invocation (chunkFor re-reads the swapped source); a scripted-prototype's live
		// instances keep their old handler until a reload of that ref lands, so a dropped one loses that
		// Lua EDIT until re-run — hence the loud warn. Room SHAPE (add/update/remove + start_room) does NOT
		// ride this message at all — it rides the reconcileZoneMsg (reconcileZone), whose own dropped-delivery
		// gap is tracked as #194 (PR 3/3, bounded-retry).
		if !z.postOrDrop(reloadLuaMsg{kind: kind, ref: ref}) {
			slog.Warn("hot-reload Lua invalidation dropped (zone inbox full); a shared def self-heals on next "+
				"access, but a scripted prototype's live instances keep the old handler until re-run",
				"zone", z.id, "kind", kind, "ref", ref)
		}
	}
}

// stop unsubscribes the reloader from the bus and cancels any in-flight reconcile-retry goroutines.
// Idempotent (the retryDone close is once-guarded); safe on a nil reloader.
func (r *reloader) stop() {
	if r == nil {
		return
	}
	// Close retryDone FIRST and unconditionally. It used to sit behind an `r.sub == nil` early return, so a
	// reloader with no live subscription never signalled stop at all — and the content-snapshot refresh
	// (#418) made that load-bearing: its debounce and its Postgres read are both cancelled by this channel,
	// so a missed close means a full-content read outliving the shard by up to the read timeout.
	r.stopOnce.Do(func() {
		if r.retryDone != nil {
			close(r.retryDone)
		}
	})
	if r.sub == nil {
		return
	}
	_ = r.sub.Unsubscribe()
}

// buildPrototype turns a single re-read Definition into a fresh *Prototype using the SAME
// DTO->component mapper the boot loader uses (content_map.go), so a hot-reloaded prototype is
// byte-identical to one built at boot. It does NOT touch the cache — the caller swaps it in. A
// nil result means the kind carries no spawnable prototype (e.g. a zone definition). Note: a room
// prototype's display name lives in RoomDTO.Name (short) and its long in RoomDTO.Long, matching
// defineContent's room define call.
func buildPrototype(def content.Definition) *Prototype {
	switch def.Kind {
	case content.KindRoom:
		r := def.Room
		return newPrototype(ProtoRef(r.Ref), nil, r.Name, r.Long, roomComponents(r))
	case content.KindItem, content.KindMob:
		d := def.Proto
		return newPrototype(ProtoRef(d.Ref), d.Keywords, d.Short, d.Long, protoComponents(d))
	default:
		return nil
	}
}
