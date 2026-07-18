package world

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/metrics"
)

// instance.go — the instanced-zone LIFECYCLE (#411, slice 2 of #72): minting a runtime copy of a content
// zone, the caps that bound how many may exist, and the idle reaper that takes them away again.
//
// An instance is a runtime-minted, shard-local, UNLEASED copy of a content-defined zone. Slice 1 (#410)
// built the identity half: Zone.template says which content a zone is built from, Zone.ownsZoneRef decides
// locality, and buildZone resolves by template — so a zone whose id differs from its template is a closed
// copy of that template, sharing the shard's immutable protoCache and hosting the template's AUTHORED room
// refs. This file is what creates one.
//
// # What an instance deliberately is NOT
//
// It takes NO directory lease, is never in the placement pool, and is never a cross-shard handoff
// destination. That is not an optimization, it is the design: releaseZone never deletes a zone hash and a
// lease's generation is immortal (internal/directory/redis.go), because AdoptZone signatures are fenced on
// that generation and must never be replayable. Leasing an EPHEMERAL ref would therefore leak a permanent
// Redis key per dungeon run and reopen the #315 replay window. WithLocalZones is the existing precedent for
// a hosted-but-unleased zone class; an instance is the second, and unlike a local zone it is also
// short-lived, so the whole point is that its identity leaves no durable trace anywhere.
//
// That is why MintInstance has its OWN build+adopt path rather than reusing HostZone. HostZone's tail arms
// lease renewal (startZoneRenewalAdopting), which would write the instance ref into Redis on the very first
// mint and — worse — later fire unadoptZone against a zone with players in it when the adoption it thinks it
// is waiting for never confirms.
//
// # Entry is NOT here
//
// Nothing in this file routes a player INTO an instance. Entry/exit, the exit anchor, and the respawn story
// are slice 3 (#72). Tests place players with the existing white-box transfer helpers.

// instanceSep separates an instance's template from its serial: `<templateRef>#<serial>`.
//
// '#' is the whole trick. It is outside the authored ref charset (internal/content/refcharset.go,
// ^[A-Za-z0-9_:-]+$), so an instance id can never collide with an authored zone ref, and parseRef splits on
// ':' only, so '#' is transparent to every ref-parsing path that already exists. The routing predicate
// (ownsZoneRef) is a PRIVACY boundary between two parties' copies of a dungeon, so this exclusion is enforced
// as a validated invariant at the mint sink below — never as a convention.
const instanceSep = "#"

// instanceSerialBytes is the width of an instance's serial: 128 bits of crypto/rand, hex-encoded.
//
// UNGUESSABLE, not monotonic, and that is a security property rather than a style choice. A counter would
// make live instance ids enumerable, and an enumerable id composes with the loot RNG into a farming oracle:
// the loot stream is seeded from the zone id (luart.go seedFromZoneID), so a predictable id means a
// precomputable drop sequence. See newInstanceZone, which salts the seeds anyway — defense in depth, since
// an id may legitimately become visible to a player (logs, staff tooling, a future GMCP field).
const instanceSerialBytes = 16

// mintInstanceID mints `<template>#<128 random bits, hex>`. The error path is crypto/rand failing, which is
// fatal-adjacent: minting a GUESSABLE id would be worse than refusing to mint, so this never falls back to
// math/rand.
func mintInstanceID(template string) (string, error) {
	b := make([]byte, instanceSerialBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint instance id for %q: %w", template, err)
	}
	return template + instanceSep + hex.EncodeToString(b), nil
}

// isInstanceID reports whether a zone id is INSTANCE-SHAPED. It is the shard-side / ingress-side predicate —
// the one usable where there is no *Zone to ask, which is every off-box ingress (Handoff.Prepare, AdoptZone,
// the durable ZoneRef read). Modeled on Shard.isLocalZone: a pure question about an id, no state.
//
// It tests the SHAPE, not a registry, deliberately. An id arriving from off-box names a zone this shard may
// not host, and the answer must be the same either way: instance-shaped ids are never legitimate there.
func isInstanceID(zoneID string) bool { return strings.Contains(zoneID, instanceSep) }

// splitInstanceID splits an instance id into its template and serial. ok is false for a normal zone id.
func splitInstanceID(zoneID string) (template, serial string, ok bool) {
	i := strings.Index(zoneID, instanceSep)
	if i < 0 {
		return zoneID, "", false
	}
	return zoneID[:i], zoneID[i+len(instanceSep):], true
}

// isInstance reports whether this zone is a runtime-minted instance rather than an authored zone. The
// zone-side twin of isInstanceID, and the predicate every in-zone behavioral exclusion asks (persistent
// resets, timed repop, signal-up, scheduled-spawn delivery).
//
// It asks the ID SHAPE rather than `template != id` so there is exactly ONE definition of "is an instance"
// across the zone side and the ingress side. MintInstance is the only producer of the shape, and it
// guarantees the two agree (an instance always has template != id as well).
func (z *Zone) isInstance() bool { return z != nil && isInstanceID(z.id) }

// newInstanceZone builds the bare Zone object for an instance: newZone, then the three things that differ
// from an authored zone — the template indirection, the logger, and the RNG seeds.
//
// # Logs and metrics deliberately want OPPOSITE answers
//
// The zone LOGGER keeps the instance id (an operator reading a log needs to know WHICH copy misbehaved, and
// the id is the only thing that says so) and gains a `template=` field so lines from every copy of a dungeon
// can still be grepped together. METRICS go the other way and label by template only — see the
// SetOccupancy/RecordTickLag sites in zone.go. A player-minted id as an OTel attribute is unbounded,
// player-driven cardinality: thousands of dead time series, one per dungeon run.
//
// # Why the seeds are salted
//
// z.lua.rng is seeded by a plain FNV-1a over the zone id (luart.go seedFromZoneID), and the loot resolver
// draws from THAT stream (`lootRNG := z.lua.rng`, death.go). So without a salt every mint would restart the
// loot stream at index 0 from a seed computable offline from the id — precompute which serials drop the
// legendary and mint until one comes up. Seeding from the TEMPLATE instead is equally wrong in the other
// direction: every instance would then roll identically.
//
// A random serial already makes the id-derived seed unpredictable, but the salt does not depend on that: an
// instance id may legitimately become visible (a log line, staff tooling, a future GMCP field), and the loot
// stream must not become predictable the moment it does. newZone already documents the same argument for
// combatRand, which is entropy-seeded for exactly this reason; the instance path re-seeds it too so the two
// streams are independently salted rather than sharing the zone's one source of entropy.
func newInstanceZone(id, template string) *Zone {
	z := newZone(id)
	z.template = template
	z.log = slog.With("component", "zone", "zone", id, "template", template)
	z.lua.log = z.log.With("subsystem", "lua") // mirrors newLuaRuntime's own tagging
	// Per-mint salt, mixed into BOTH zone-owned streams. Drawn from crypto/rand; a failure degrades to the
	// unsalted (still id-derived, still random-serial-derived) seed rather than refusing the mint, and says so
	// loudly — a predictable-ish loot stream is bad, a dungeon that cannot be entered is worse.
	var salt int64
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		slog.Warn("could not salt an instance's RNG seeds; falling back to the id-derived seed",
			"zone", id, "template", template, "err", err)
	} else {
		for _, c := range b {
			salt = salt<<8 | int64(c&0xff)
		}
		z.lua.reseed(seedFromZoneID(id) ^ salt)
		z.reseedCombat(salt)
	}
	return z
}

// instanceLimits bounds how many instances may exist and how fast they may be minted.
//
// Every quota here is charged to the requesting ACCOUNT, never to the actor or the zone, and the distinction
// is the whole point. A per-CHARACTER cap is routed around by alts; a per-SCRIPT cap is routed around by one
// script minting on behalf of many players. And quiescence alone is not a bound at all: an idle CONNECTED
// player is never quiescent and pins their instance for as long as they care to stand still, so only the
// concurrent cap bounds that. (A LINK-DEAD session is a much weaker pin than it looks: it is held for
// linkDeadGrace and then reaped by reapMsg — 60s today — after which the zone goes quiescent on its own.)
// Hence: a per-account concurrent cap, a per-account mint RATE limit (the cheap-to-mint, cheap-to-abandon
// churn the concurrent cap alone does not see), and a global per-shard cap as the resource backstop that no
// per-principal accounting can provide.
//
// NOTE that every bound here is PER PROCESS. There is no cross-shard instance accounting, so an account's
// real ceiling across a fleet is perAccount x (number of world shards it can reach). That is deliberate for
// now — a shard-local cap needs no coordination and cannot fail open on a directory outage — but it is what
// perShard, the only bound that maps to a real resource (a zone object, an actor goroutine and a Lua VM
// each), is actually protecting.
type instanceLimits struct {
	perAccount int           // max LIVE instances charged to one account
	perShard   int           // max LIVE instances on this shard, all accounts (the resource backstop)
	mintBurst  int           // max mints per account per mintWindow
	mintWindow time.Duration // the rate-limit window
}

// Default limits. Deliberately conservative: a party-sized dungeon run needs one instance, and the ceiling
// exists to bound abuse, not to express content policy. Tunable via WithInstanceLimits.
const (
	defaultInstancesPerAccount = 3
	defaultInstancesPerShard   = 256
	defaultInstanceMintBurst   = 6
	defaultInstanceMintWindow  = time.Minute
)

func defaultInstanceLimits() instanceLimits {
	return instanceLimits{
		perAccount: defaultInstancesPerAccount,
		perShard:   defaultInstancesPerShard,
		mintBurst:  defaultInstanceMintBurst,
		mintWindow: defaultInstanceMintWindow,
	}
}

// WithInstanceLimits overrides the instance caps (#411). Any non-positive field keeps its default, so a
// caller can raise one bound without restating the others — and so a zero-valued config can never
// accidentally DISABLE a cap, which for the global cap would be a resource-exhaustion hole. Meant to be
// called before Run, like the other With* options.
//
// It still takes mu: instanceLimits is READ under mu on the mint path (reserveInstanceSlot), so writing it
// unlocked is a data race the moment anything calls this after Run — which tests already did before this was
// fixed. Taking the lock costs nothing on a construction-time path and removes the trap entirely.
//
// RAISING perAccount ALONE IS NOT ENOUGH. The mint rate limit is a FIXED window, not a token bucket, so its
// real worst case is 2*mintBurst mints across a window boundary. That is only harmless because perAccount
// (3 by default) bounds how many of those can be LIVE at once, which in turn bounds how fast the churn can
// repeat. Raise perAccount without also reconsidering mintBurst/mintWindow and the coupling that made the
// fixed window acceptable is gone: mint churn — full zone builds and boot resets, the expensive part — stops
// being bounded by anything.
func (s *Shard) WithInstanceLimits(perAccount, perShard, mintBurst int, mintWindow time.Duration) *Shard {
	s.mu.Lock()
	defer s.mu.Unlock()
	if perAccount > 0 {
		s.instanceLimits.perAccount = perAccount
	}
	if perShard > 0 {
		s.instanceLimits.perShard = perShard
	}
	if mintBurst > 0 {
		s.instanceLimits.mintBurst = mintBurst
	}
	if mintWindow > 0 {
		s.instanceLimits.mintWindow = mintWindow
	}
	return s
}

// instanceRecord is the shard's bookkeeping for one live instance. Guarded by Shard.mu (the same mutex that
// guards s.zones, so a mint's cap check and its publish are one atomic decision).
type instanceRecord struct {
	id       string    // the instance zone id
	template string    // the content zone it was built from
	account  string    // the account the slot is charged to
	minted   time.Time // when the slot was reserved — the post-mint grace runs from here
	idle     int       // consecutive reaper ticks observed quiescent; reset by any sign of life
}

// mintBucket is one account's fixed-window mint counter. A fixed window (not a token bucket) because the
// quantity being limited is coarse and human-paced; the worst case is 2*burst across a window boundary,
// which is well inside what perAccount already bounds.
type mintBucket struct {
	windowStart time.Time
	count       int
}

// Reaper tuning. Vars, not consts, so a test can drive the loop without sleeping through a production
// cadence — the same idiom adoptConfirmDeadline and unhostActorGrace use.
var (
	// instanceReapInterval is the shard-level reaper's tick. It is its OWN ticker and NOT the lease-renewal
	// tick: instances take no lease, so no renewal goroutine exists for them to ride on.
	instanceReapInterval = 15 * time.Second

	// instanceIdleTicks is how many CONSECUTIVE quiescent ticks retire an instance. More than one so a
	// single unlucky sample — a party mid-transfer between two rooms is quiescent for the width of a queue
	// hop on the shard as a whole, though not on the destination, which holds an `incoming` claim — cannot
	// reap an occupied dungeon.
	instanceIdleTicks = 4

	// instanceMintGrace protects a freshly-minted instance that nobody has entered YET. Entry is a separate
	// mechanism (slice 3) that necessarily happens after the mint returns, so without this window every
	// instance would be reapable the instant it was born.
	instanceMintGrace = 2 * time.Minute
)

// MintInstance builds, adopts, and starts a private runtime copy of templateRef, charged to accountID, and
// returns the live zone. It is the ONLY producer of an instance-shaped zone id.
//
// It deliberately does NOT reuse HostZone: HostZone's tail arms lease renewal, which for an instance would
// write an ephemeral ref into the directory on the first mint and later fire unadoptZone against a zone that
// may have players in it. See the file header.
//
// accountID is REQUIRED. A cap that cannot be charged to anybody is not a cap, so an empty account is a
// refusal rather than a shared bucket. When slice 3 wires the player-facing entry surface, it must pass the
// account from the VERIFIED session assertion (server.go's assertion.Verify claims) — never a client-supplied
// value, or the per-account cap is trivially evaded by lying about who you are.
//
// NEVER CALL THIS ON A ZONE GOROUTINE. It does a full zone BUILD (every room spawned, every boot reset run)
// and a seedZone store round trip synchronously on the caller's goroutine. From a zone actor that is a
// self-inflicted stall of the entrance zone for the whole build — every player standing there frozen while
// somebody else's dungeon is constructed. Slice 3's entry surface is exactly the caller that would be
// tempted, so it owes an async hand-off (mint off the actor, deliver the result back by message); the
// plumbing for that is slice 3's, and this contract is the marker for it.
func (s *Shard) MintInstance(ctx context.Context, templateRef, accountID string) (*Zone, error) {
	if err := s.validateMintTemplate(templateRef); err != nil {
		return nil, err
	}
	if accountID == "" {
		return nil, fmt.Errorf("mint instance of %q: no account to charge the instance cap to", templateRef)
	}

	id, err := mintInstanceID(templateRef)
	if err != nil {
		return nil, err
	}

	// Phase 1: reserve the slot. Caps + rate limit + the shard-running guards, all under ONE hold of mu, so
	// two concurrent mints cannot both observe "one slot left". The reservation is a record with no zone
	// behind it yet; the reaper skips those (it resolves each record through s.zones).
	if err := s.reserveInstanceSlot(id, templateRef, accountID); err != nil {
		return nil, err
	}
	// Every failure from here on must give the slot back, or an abandoned mint permanently consumes an
	// account's quota. Disarmed once the zone is published.
	//
	// It gives back the SLOT only. A phase-3 refusal (shutdown, or a drain that started mid-build) abandons a
	// fully-built zone whose Lua LState is closed only by Run's defer — and Run never started — so the VM's
	// memory is dropped without close(). Informational, deliberately not plumbed: nothing in gopher-lua needs
	// an explicit close for correctness, the whole zone is unreachable and GC-reclaimable the moment this
	// returns, and the path is bounded by the mint rate limit AND only reachable while the process is on its
	// way out. Closing it here would mean a second teardown path for a VM the actor is otherwise the sole
	// owner of, which is a worse trade than the garbage.
	published := false
	defer func() {
		if !published {
			s.releaseInstanceSlot(id)
		}
	}()

	// Phase 2: BUILD off the lock. buildZone spawns every room and runs the boot resets, which must not block
	// the routing reads (zoneByID, claimTransferTarget) that take this same mutex — the reason HostZone builds
	// outside mu too.
	z := newInstanceZone(id, templateRef)
	z.protos = s.protos
	z.buildZone(s.content)

	// Seed the scope replica BEFORE the zone is reachable, exactly as HostZone does (#280): once it is in
	// s.zones a world-scope delta can be posted to its inbox, and applyScopeSeed is a full-map REPLACE, so a
	// seed arriving after a delta would clobber newer state with the snapshot.
	s.scopes.seedZone(ctx, z)

	// Phase 3: publish + arm, atomically against a concurrent shutdown OR a concurrent drain.
	//
	// `s.draining` is re-checked HERE and not only in reserveInstanceSlot, because everything between the two
	// checks — a full buildZone (every room spawned, every boot reset run) plus the seedZone store round trip
	// above — is unbounded work off the lock: hundreds of milliseconds to seconds. A BeginDrain that starts in
	// that window snapshots zonesList() BEFORE this adoptLocked, so publishing now would put the instance in
	// NO drain set at all: not in `initial`, not flushed by s.Drain, never sent a reclaim notice. Its
	// occupants would be dropped with the process, silently and uncounted — the same hole the reserve-time
	// refusal exists to close, entered from the other side. A drain that begins after this hold instead finds
	// the instance in s.zones and accounts for it normally.
	s.mu.Lock()
	if s.closed || s.runCtx == nil || s.runWG == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("mint instance of %q: shard shutting down", templateRef)
	}
	if s.draining {
		s.mu.Unlock()
		return nil, fmt.Errorf("mint instance of %q: this shard began draining while the instance was building",
			templateRef)
	}
	s.adoptLocked(id, z)
	runCtx := s.runCtx
	s.runWG.Add(1)
	// Arm the actor's cancel+done in the SAME hold that published the zone, so no UnhostZone (the reaper's
	// own path) can ever see a zone in s.zones with no way to stop it. Same invariant HostZone maintains.
	zctx, zcancel, zdone := s.armZoneActorLocked(runCtx, id)
	published = true
	liveForTemplate := s.instanceCountLocked(templateRef)
	liveTotal := len(s.instances)
	s.mu.Unlock()

	// registerZone must precede z.Run: it stamps scopes.regionID, and the seed already sitting in the inbox
	// is silently DROPPED by applyScopeSeed if the stamp has not landed when the loop consumes it.
	s.scopes.registerZone(z)

	go func() {
		defer s.runWG.Done()
		defer s.disarmZoneActor(id, zdone)
		defer zcancel()
		z.Run(zctx)
	}()

	// Metrics label by TEMPLATE (see zone.go's occupancy/tick-lag sites): an instance id is unbounded,
	// player-driven cardinality and must never become an OTel attribute. The VALUE must be per-template too —
	// see instanceCountLocked for why the shard-wide total is not merely imprecise here but wrong.
	metrics.SetInstances(ctx, liveForTemplate, templateRef)
	slog.Info("minted zone instance", "zone", id, "template", templateRef, "account", accountID,
		"live_for_template", liveForTemplate, "live_instances", liveTotal)
	return z, nil
}

// instanceCountLocked returns how many LIVE instances were minted from one template. Caller holds mu.
//
// The gauge is labeled by template, so its VALUE has to be per-template as well. Reporting the shard-wide
// total against a single template label is wrong in two ways at once: with templates A and B live, every
// series reads the same total (so the gauge over-reports each template by the others' load and sums to
// N*total), and — the reason it cannot just be left imprecise — when the LAST instance of A is reaped,
// series{template=A} is set to the remaining total and then never sampled again. It reports a nonzero count
// for a template with zero live instances, forever. This is the one gauge the metrics doc points operators at
// for instanced load, so it has to be the count of the thing its label names.
//
// Linear in the number of live instances, which perShard bounds (256 by default), on a path that is either
// rate-limited (mint) or a teardown. That is cheaper than maintaining a second map that could drift.
func (s *Shard) instanceCountLocked(template string) int64 {
	n := int64(0)
	for _, rec := range s.instances {
		if rec.template == template {
			n++
		}
	}
	return n
}

// instanceTemplateCounts snapshots live instances per template, under mu. Used by the reload readout to warn
// that a reloaded zone has pinned instances running its OLD content (reloadcmd.go); never ranges the live map.
func (s *Shard) instanceTemplateCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.instances))
	for _, rec := range s.instances {
		out[rec.template]++
	}
	return out
}

// validateMintTemplate is the MINT SINK's validation of a template ref, and it is deliberately not delegated
// to the load-time charset lint.
//
// refcharset.go's own scope note says ref-VALUED fields under other names — exit targets, reset protos, Lua
// string literals — are NOT charset-checked. So a content author (or a compromised pack) can get a '#' into a
// string that reaches a new sink like this one. Since the '#' exclusion is what makes an instance id
// unforgeable as an authored ref, it has to be re-checked HERE, where the id is created, not only where refs
// are loaded.
//
// Requiring the template to name an actually-LOADED zone is the second half: it stops a mint from building an
// empty zone (buildZone boots empty for unknown content behind a Debug line — the worst failure shape in this
// package) and stops an arbitrary attacker-chosen string from becoming a live zone id on this shard.
//
// THE HOME ZONE AND LOCAL BOOTSTRAP ZONES ARE DELIBERATELY ALLOWED as templates, rather than accidentally so.
// UnhostZone refuses to tear either of them down, so the asymmetry is worth stating: those refusals key on
// the ZONE ID (`id == s.home`, `s.isLocalZone(id)`), and an instance's id is `<template>#<serial>`, which is
// never equal to either. So an instance of the home zone is an ordinary instance — unleased, private, and
// reapable — and it does not shadow, replace or endanger the original, which stays hosted under its own id
// and remains the login fallback. Refusing them would instead be an engine-side POLICY about which authored
// content may be instanced, which is content's call to make (slice 3 owns the entry gate); the invariants
// this sink defends are the id grammar and the existence of the content.
func (s *Shard) validateMintTemplate(templateRef string) error {
	switch {
	case templateRef == "":
		return fmt.Errorf("mint instance: no template zone named")
	case isInstanceID(templateRef):
		// Includes the obvious "instance of an instance", but the check is on the CHARACTER, not on
		// well-formedness: any '#' at all is refused, because the separator's exclusivity is the invariant.
		return fmt.Errorf("mint instance of %q: a template ref may not contain %q (it is reserved for instance ids)",
			templateRef, instanceSep)
	case s.content == nil:
		return fmt.Errorf("mint instance of %q: shard has no retained content to build from", templateRef)
	case s.content.Zone(templateRef) == nil:
		return fmt.Errorf("mint instance of %q: no such zone in loaded content", templateRef)
	}
	return nil
}

// reserveInstanceSlot takes the caps + rate limit for one mint and records the reservation, under one hold of
// mu. Returns a refusal (never a partial reservation) when any bound is hit.
func (s *Shard) reserveInstanceSlot(id, templateRef, accountID string) error {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.runCtx == nil || s.runWG == nil {
		return fmt.Errorf("mint instance of %q: shard not running", templateRef)
	}
	// A DRAINING shard mints nothing. BeginDrain snapshots the hosted zones ONCE, so an instance minted after
	// that snapshot is in no drain set at all: it is not handed over (correct), but it is also not in the
	// accounting set, not flushed with the others, and not reclaimed — its occupants would be dropped
	// silently, uncounted, with the process. The refusal is the same class as the fresh-login refusal the
	// drain already applies (isDraining), and for the same reason: once the shard has decided to go away,
	// nothing new may take up residence on it.
	if s.draining {
		return fmt.Errorf("mint instance of %q: this shard is draining", templateRef)
	}
	if len(s.instances) >= s.instanceLimits.perShard {
		return fmt.Errorf("mint instance of %q: this shard is at its instance capacity (%d)",
			templateRef, s.instanceLimits.perShard)
	}
	held := 0
	for _, rec := range s.instances {
		if rec.account == accountID {
			held++
		}
	}
	if held >= s.instanceLimits.perAccount {
		return fmt.Errorf("mint instance of %q: account already holds %d instances (limit %d)",
			templateRef, held, s.instanceLimits.perAccount)
	}
	// Fixed-window mint rate. Checked AFTER the concurrent caps so the cheaper, more meaningful refusal wins
	// the message; both are hard refusals either way.
	b := s.mintRate[accountID]
	if b == nil || now.Sub(b.windowStart) >= s.instanceLimits.mintWindow {
		b = &mintBucket{windowStart: now}
		s.mintRate[accountID] = b
	}
	if b.count >= s.instanceLimits.mintBurst {
		return fmt.Errorf("mint instance of %q: account is minting too fast (%d per %s)",
			templateRef, s.instanceLimits.mintBurst, s.instanceLimits.mintWindow)
	}
	b.count++
	s.instances[id] = &instanceRecord{id: id, template: templateRef, account: accountID, minted: now}
	// Prune spent buckets for accounts that hold nothing, so the map cannot grow with one entry per account
	// that ever minted on this process. Cheap: it only runs on the mint path, which is already rate-limited.
	s.pruneMintRateLocked(now)
	return nil
}

// pruneMintRateLocked drops rate-limit buckets whose window has fully elapsed. Caller holds mu.
func (s *Shard) pruneMintRateLocked(now time.Time) {
	for acct, b := range s.mintRate {
		if now.Sub(b.windowStart) >= s.instanceLimits.mintWindow {
			delete(s.mintRate, acct)
		}
	}
}

// releaseInstanceSlot gives an instance's cap slot back. Its ONLY caller is MintInstance's failure defer — a
// mint that reserved a slot and then could not publish the zone. Idempotent.
//
// THE TEARDOWN PATH DOES NOT COME THROUGH HERE. UnhostZone inlines the same bookkeeping instead, because it
// must delete the instance record in the SAME hold of mu that removes the zone from s.zones (see its comment),
// and this function takes the lock itself. So the reaper — the dominant teardown path — never calls this. Do
// not read it as "the place instances are retired"; the gauge update below is duplicated there deliberately,
// and any change to one has to be made to the other.
//
// It deliberately does NOT refund the RATE limit: the mint attempt happened, and refunding it would let a
// caller drive unbounded build work by failing late on purpose.
func (s *Shard) releaseInstanceSlot(id string) {
	s.mu.Lock()
	rec := s.instances[id]
	delete(s.instances, id)
	var live int64
	if rec != nil {
		live = s.instanceCountLocked(rec.template) // per-TEMPLATE, computed in the same hold that removed it
	}
	s.mu.Unlock()
	if rec != nil {
		metrics.SetInstances(context.Background(), live, rec.template)
	}
}

// runInstanceReaper is the shard-level ticker that retires idle instances. Started by Run.
//
// It cannot ride the lease-renewal tick as originally sketched: instances take no lease, so there is no
// renewal goroutine for them to ride. It runs OFF every zone goroutine and only ever reads atomics (via
// quiescent()) and calls UnhostZone, which does its own locking — single-writer is untouched.
func (s *Shard) runInstanceReaper(ctx context.Context) {
	t := time.NewTicker(instanceReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapIdleInstances(ctx)
		}
	}
}

// reapIdleInstances retires every instance that has been quiescent for instanceIdleTicks consecutive ticks
// and is past its post-mint grace. One sweep of the reaper.
//
// # Why the entering-vs-reaping race is safe
//
// Zone.quiescent() folds in `incoming` (#409), and `incoming` is claimed by Shard.claimTransferTarget in the
// SAME hold of s.mu that resolves the destination zone. UnhostZone re-checks quiescent() under that same
// mutex before it removes the zone. So for a player walking into an instance the two orders are both safe:
//
//   - claim first: UnhostZone sees incoming > 0 and refuses; the entrant arrives into a live zone.
//   - unhost first: the zone is already out of s.zones, claimTransferTarget returns nil having claimed
//     nothing, and the walker falls through to the cross-shard branch with nothing mutated.
//
// The idle counting below is only a HEURISTIC for choosing candidates; it is UnhostZone's re-check that is
// load-bearing. That is why a refusal here is an ordinary outcome (reset the counter, try again next tick),
// not an error.
func (s *Shard) reapIdleInstances(ctx context.Context) {
	now := time.Now()
	var due []string
	s.mu.Lock()
	for id, rec := range s.instances {
		z := s.zones[id]
		if z == nil {
			continue // reserved but not published yet (a mint in flight), or already torn down
		}
		if now.Sub(rec.minted) < instanceMintGrace {
			// Nobody has had the chance to enter yet. Entry is a separate mechanism that necessarily runs
			// after the mint returns, so an ungraced instance would be reaped out from under its own party.
			rec.idle = 0
			continue
		}
		if !z.quiescent() {
			rec.idle = 0
			continue
		}
		rec.idle++
		if rec.idle >= instanceIdleTicks {
			due = append(due, id)
		}
	}
	s.mu.Unlock()
	sort.Strings(due) // deterministic sweep order (map iteration is randomized)
	for _, id := range due {
		if err := s.UnhostZone(ctx, id); err != nil {
			// Ordinary: someone entered between the sample and the re-check, or the actor is busy. Give the
			// instance a fresh idle budget so it is not retried on every single tick.
			s.resetInstanceIdle(id)
			slog.Debug("instance reap deferred", "zone", id, "err", err)
			continue
		}
		slog.Info("reaped idle zone instance", "zone", id)
	}
}

// resetInstanceIdle clears an instance's consecutive-quiescent counter.
func (s *Shard) resetInstanceIdle(id string) {
	s.mu.Lock()
	if rec := s.instances[id]; rec != nil {
		rec.idle = 0
	}
	s.mu.Unlock()
}
