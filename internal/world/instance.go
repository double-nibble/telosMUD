package world

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
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
// THIS IS THE EMBEDDER/TEST SEAM AND IT DOES NOT VALIDATE. Operators reach the same fields through
// SetInstanceLimits, which bounds-checks them and refuses a configuration the shard cannot honor. This one is
// deliberately permissive because tests need the extreme values a validator must refuse (a perAccount of 1
// against a mintBurst of 100, say, to exercise one bound without tripping the other).
//
// THE MINT RATE LIMIT IS THE ONLY PER-ACCOUNT BOUND ON MINT CHURN. It is a FIXED window, not a token bucket,
// so its real worst case is 2*mintBurst mints across a window boundary — and perAccount does NOT contain that,
// contrary to what this comment claimed before #436. reserveInstanceSlot excludes ABANDONED records from the
// per-account count (see the comment there), and mint-abandon-mint is precisely the churn loop: each iteration
// is a full zone build plus its boot resets, the expensive part, and none of it is charged to perAccount.
// (perShard still counts abandoned records, so it does bound concurrent churn shard-wide — but that is a
// backstop shared with every other account, not a bound on the account doing the churning.) So
// mintBurst/mintWindow are load-bearing on their own rather than a second line behind the concurrent cap.
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

// Bounds for the operator-facing SetInstanceLimits (#436). Unexported: nothing outside this package needs
// them, and internal/config must not import internal/world — that leaf property is why validation lives at
// the injection point rather than in the config package.
const (
	// maxInstancesPerAccount. Past this the per-account cap has stopped being a fairness bound and is just a
	// second copy of the shard cap. It is also multiplied by the shard count fleet-wide.
	maxInstancesPerAccount = 64

	// maxInstancesPerShard is the tightest ceiling of the four because this is the only cap whose failure mode
	// is an OOM-killed shard rather than a refusal. Three independent arguments land in the same place:
	//
	//  1. Memory. Each live instance is a zone object, an actor goroutine and a Lua VM whose registry grows to
	//     luasandbox.RegistryMaxSize and never shrinks, plus every entity the template's boot resets spawn —
	//     which the engine does not bound at all.
	//  2. Mutex hold time on the ROUTING path. reserveInstanceSlot scans all of s.instances under s.mu, and
	//     s.mu is the same mutex zoneByID and claimTransferTarget take. perShard directly sizes a stall on
	//     every routing read.
	//  3. drainEjectBarrier. BeginDrain's step 0 ejects EVERY instance's occupants under ONE shared 5s
	//     deadline, a budget sized against 256. That is a real ordering relation between this value and a
	//     constant in instance_entry.go — the issue that asked for these knobs asserted there was none — and
	//     overshooting it does not crash: the tail instances miss the barrier and their occupants are dropped
	//     to straggler reclaim on every rolling deploy, warned about and otherwise silent.
	maxInstancesPerShard = 1024

	// warnInstancesPerShard is where (3) above starts being a real risk rather than a theoretical one.
	warnInstancesPerShard = 512

	// maxInstanceMintBurst bounds the instantaneous spike (2*burst across a window boundary); the sustained
	// rate is bounded separately by maxInstanceMintsPerMinute, since a burst ceiling alone means nothing
	// without the window it is measured over.
	maxInstanceMintBurst = 60

	// The mint window's floor and ceiling. FLOOR: a window shorter than a mint's own build latency means
	// essentially every mint opens a fresh bucket, so the limiter is configured but inert. CEILING:
	// pruneMintRateLocked only drops a bucket once its window has FULLY elapsed, so the window is exactly how
	// long s.mintRate retains an entry per minting account — an hours-long window turns a rate limit into a
	// durable per-account quota with an unbounded-in-accounts map behind it. Note that the ceiling guards the
	// direction an operator reaches for when they want to be STRICTER.
	minInstanceMintWindow = time.Second
	maxInstanceMintWindow = time.Hour

	// maxInstanceMintsPerMinute is the sustained mint rate ceiling, per account. A mint is a full buildZone —
	// every room spawned, every boot reset run — plus a seedZone store round trip. This is what bounds that
	// work per unit time, and it is expressed as a RATE because validating burst and window separately lets
	// burst=60/window=1s through, which is 3600 zone builds a minute. Conservative by choice, not derived.
	maxInstanceMintsPerMinute = 120
)

// SetInstanceLimits is the OPERATOR-facing injection point for the four instance caps (#436) — the validated
// twin of WithInstanceLimits, and #368 slice 2. A zero field means "use the compiled-in default", NEVER
// "unlimited": for the counts unlimited is resource exhaustion, and for the window zero is worse still, since
// `now.Sub(windowStart) >= 0` is true on every mint, so the bucket resets every time and the rate limit
// disappears silently while the boot log shows a plausible configuration.
//
// It validates the EFFECTIVE values — after the zero-means-default substitution — which is the difference
// between working and inert. An operator who sets instances_per_account: 300 and leaves instances_per_shard
// unset presents (300, 0): validating the raw pair sees "perShard unset, no cross-field check to do" and ships
// exactly the per-account-above-shard-cap configuration the cross-field rule exists to refuse.
//
// It is also all-or-nothing. The whole limit set is built and checked locally and assigned in one write under
// mu, so a refused configuration changes NOTHING — rather than leaving the shard running two of the operator's
// four values after being told the set was rejected.
//
// A method on the constructed *Shard rather than a builder option, deliberately: cmd/telos-world builds a
// shard on two different paths (with and without Redis), and a builder-option wiring added to one of them is
// silently inert on the other — which would be the local/dev path, where a mistake goes unnoticed longest.
func (s *Shard) SetInstanceLimits(perAccount, perShard, mintBurst, mintWindowSec int) error {
	for _, f := range []struct {
		name string
		v    int
	}{
		{"instances_per_account", perAccount},
		{"instances_per_shard", perShard},
		{"instance_mint_burst", mintBurst},
		{"instance_mint_window_sec", mintWindowSec},
	} {
		if f.v < 0 {
			return fmt.Errorf("%s=%d: there is no unlimited — every instance cap bounds a real resource "+
				"(a zone, a goroutine and a Lua VM each). Use 0 for the engine default", f.name, f.v)
		}
	}

	// The window's ceiling is checked on the RAW SECONDS, before the multiply below, and that ordering is the
	// whole point: time.Duration(mintWindowSec) * time.Second scales by 1e9, so a large int64 wraps — and it
	// can wrap to a value INSIDE the accepted range. 2^55+60 seconds arrives as a perfectly legal 1m0s, so
	// checking only the product turns the ceiling into a silent reinterpretation of the operator's number,
	// which is the "parses, logs plausibly, means something else" failure this whole feature exists to end.
	if maxSec := int(maxInstanceMintWindow.Seconds()); mintWindowSec > maxSec {
		return fmt.Errorf("instance_mint_window_sec=%d exceeds the maximum %d: buckets are only pruned once "+
			"their window has fully elapsed, so a longer window is not stricter — it is a durable per-account "+
			"quota with one retained map entry per account that ever minted", mintWindowSec, maxSec)
	}

	want := defaultInstanceLimits()
	if perAccount > 0 {
		want.perAccount = perAccount
	}
	if perShard > 0 {
		want.perShard = perShard
	}
	if mintBurst > 0 {
		want.mintBurst = mintBurst
	}
	if mintWindowSec > 0 {
		want.mintWindow = time.Duration(mintWindowSec) * time.Second
	}

	switch {
	case want.perAccount > maxInstancesPerAccount:
		return fmt.Errorf("instances_per_account=%d exceeds the maximum %d: past that it is no longer a "+
			"fairness bound, and it is multiplied by the shard count fleet-wide",
			want.perAccount, maxInstancesPerAccount)
	case want.perShard > maxInstancesPerShard:
		return fmt.Errorf("instances_per_shard=%d exceeds the maximum %d: each instance is a zone, an actor "+
			"goroutine and a Lua VM, this is the one cap whose failure is an OOM-killed shard rather than a "+
			"refusal, and the drain's eject barrier is sized against a value in this range",
			want.perShard, maxInstancesPerShard)
	case want.perAccount > want.perShard:
		return fmt.Errorf("instances_per_account=%d exceeds instances_per_shard=%d, so the per-account cap can "+
			"never fire: the only cap left is the global one, which is first-come-first-served, and one account "+
			"filling the shard denies every other player entry", want.perAccount, want.perShard)
	case want.mintBurst > maxInstanceMintBurst:
		return fmt.Errorf("instance_mint_burst=%d exceeds the maximum %d: the fixed window's real worst case "+
			"is twice this across a window boundary, each mint being a full zone build",
			want.mintBurst, maxInstanceMintBurst)
	case want.mintWindow < minInstanceMintWindow:
		return fmt.Errorf("instance_mint_window_sec=%v is below the minimum %v: a window shorter than a mint's "+
			"own build latency opens a fresh bucket for essentially every mint, so the limit is configured and "+
			"inert", want.mintWindow, minInstanceMintWindow)
	}
	// The two are only meaningful together: burst alone permits 60-per-second, and a window alone bounds
	// nothing. Checked in per-minute terms so the number in the message is the one an operator reasons in.
	if rate := float64(want.mintBurst) / want.mintWindow.Minutes(); rate > maxInstanceMintsPerMinute {
		return fmt.Errorf("instance_mint_burst=%d per instance_mint_window_sec=%v is %.0f mints per minute, "+
			"above the maximum %d: a mint is a full zone build (every room spawned, every boot reset run) plus "+
			"a store round trip, and this rate is the only thing bounding that work — the concurrent caps do "+
			"not see mint-abandon-mint churn at all",
			want.mintBurst, want.mintWindow, rate, maxInstanceMintsPerMinute)
	}

	s.mu.Lock()
	s.instanceLimits = want
	s.mu.Unlock()

	if want.perShard > warnInstancesPerShard {
		slog.Warn("instances_per_shard is high: the drain's instance eject runs every instance under one "+
			"shared barrier, so occupants of the tail instances may be dropped to straggler reclaim on a "+
			"rolling deploy", "per_shard", want.perShard, "warn_above", warnInstancesPerShard)
	}
	// Refused only when it can NEVER fire (perAccount > perShard); warned when it barely fires. At equality
	// the per-account cap trips at the exact point the global cap already would, so it reads as a fairness
	// bound and delivers none — but it is a coherent thing for an operator to choose, and refusing it would
	// also refuse the legitimate degenerate case of a one-instance shard, where the two are equal by
	// necessity. Hence the perShard > 1 guard.
	if want.perShard > 1 && want.perAccount*2 > want.perShard {
		slog.Warn("instances_per_account is close to instances_per_shard, so it provides little fairness: one "+
			"account can take most or all of the shard's capacity and deny every other player entry",
			"per_account", want.perAccount, "per_shard", want.perShard)
	}
	// Fires on an explicitly-configured 6 as well as an unset one. The zero sentinel deliberately carries no
	// "was it set" signal, and warning about a deliberate choice is the cheaper error of the two here.
	if want.perAccount > defaultInstancesPerAccount && want.mintBurst == defaultInstanceMintBurst {
		slog.Warn("instances_per_account was raised while the mint rate limit was left at its default: the "+
			"rate limit is the only bound on mint-abandon-mint churn, so consider it alongside the "+
			"concurrent cap", "per_account", want.perAccount, "mint_burst", want.mintBurst)
	}
	// The EFFECTIVE values, logged from the site that installs them, so an operator can confirm their setting
	// took rather than inferring it. The caveat rides along because "3" reads as a fleet-wide guarantee and is
	// not one.
	slog.Info("instance caps", "per_account", want.perAccount, "per_shard", want.perShard,
		"mint_burst", want.mintBurst, "mint_window", want.mintWindow,
		"note", "per shard PROCESS; an account's fleet-wide ceiling is per_account x the shard count")
	return nil
}

// instanceRecord is the shard's bookkeeping for one live instance. Guarded by Shard.mu (the same mutex that
// guards s.zones, so a mint's cap check and its publish are one atomic decision).
type instanceRecord struct {
	id       string    // the instance zone id
	template string    // the content zone it was built from
	account  string    // the account the slot is charged to
	minted   time.Time // when the slot was reserved — the post-mint grace runs from here
	idle     int       // consecutive reaper ticks observed quiescent; reset by any sign of life
	// abandoned marks an instance that was minted successfully but that its entrant never entered — hop 3 of
	// the entry flow decided not to move the player (they quit, walked away, engaged, or the destination stopped
	// being claimable). See abandonInstance for what the flag changes and why it is not a record deletion.
	abandoned bool
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
	// ONE snapshot of the live content for this whole mint (#418). The reloader swaps s.content while the
	// shard runs, so re-reading it per check would let validate and build disagree: the template could pass
	// `instanceable: true` against version N and then be BUILT against N+1, where a reload deleted it —
	// buildZone would boot the instance empty behind a Debug line and hand a player a zone with no rooms.
	// Taking it once makes the whole mint atomic with respect to content, which is also the semantic a
	// builder expects: one run is one version.
	lc := s.liveContent()
	if err := validateMintTemplate(lc, templateRef); err != nil {
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
	// REFUSE an incomplete build (#418). validateMintTemplate proved the SNAPSHOT declares a start room; it
	// cannot prove the prototype cache has one, because the snapshot and the cache converge on independent
	// paths after a reload. Entry lands via transferIn's resolveRoom(""), so an instance whose start room
	// did not spawn DISCONNECTS the entrant mid-entry — after transferOut has already released them. A
	// refusal here is a "the way will not open" line and the player keeps playing.
	if err := z.buildZone(lc); err != nil {
		return nil, fmt.Errorf("mint instance of %q: %w", templateRef, err)
	}

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
	// Advertise the template NOW rather than waiting for the heartbeat's next tick (#416). This matters most
	// for a template's FIRST live copy: with no prior claim in the directory, the prune guard would read
	// "nobody is using this" for a zone a party is standing in. Non-blocking — never make a mint wait on Redis.
	s.kickTemplateUse(templateRef)
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
// THE OPT-IN IS THE THIRD CHECK, and it is the one that bounds the mechanism's blast radius.
//
// A mint runs the template's full boot resets: every item and mob the zone declares, created fresh in a private
// copy. A player alone in that copy can strip it, walk out through any foreign-zone exit (the transfer carries
// their whole inventory subtree), and mint again. That is not a dupe — it is an uncapped GENERATION FAUCET,
// scaling with mint rate times account count. Without an opt-in it reached EVERY zone in loaded content,
// including zones whose in-world access another builder deliberately gated behind a locked door, a quest or a
// level check: a private copy has no doorman, so instancing routes around every in-world gate at once.
//
// WHICH zones may be instanced is content's call, not the engine's — the engine states no policy about it. But
// content cannot MAKE that call unless the engine gives it a way to, and enforces the answer. `instanceable`
// on the zone definition is that way, and this is where it is enforced. Default false: a zone is not an
// instance template unless its author said so, which is the fail-closed direction (a missing opt-in breaks a
// dungeon door, a missing refusal breaks the economy).
//
// THE HOME ZONE AND LOCAL BOOTSTRAP ZONES are consequently no longer allowed by default either, and that is a
// deliberate tightening rather than a side effect: they are the zones a player is most likely to be able to
// reach, so they are the worst faucets. An author who genuinely wants one instanced sets the flag on it, and
// the note that made this safe still holds — UnhostZone's refusals key on the ZONE ID (`id == s.home`,
// `s.isLocalZone(id)`), and an instance's id is `<template>#<serial>`, never equal to either, so an instance of
// the home zone would not shadow, replace or endanger the original.
// It takes the content snapshot as an ARGUMENT rather than reading s.content, so that the caller's single
// snapshot (#418) is provably the one validated — a method reading the live pointer could not be, since the
// reloader swaps it underneath.
func validateMintTemplate(lc *content.LoadedContent, templateRef string) error {
	switch {
	case templateRef == "":
		return fmt.Errorf("mint instance: no template zone named")
	case isInstanceID(templateRef):
		// Includes the obvious "instance of an instance", but the check is on the CHARACTER, not on
		// well-formedness: any '#' at all is refused, because the separator's exclusivity is the invariant.
		return fmt.Errorf("mint instance of %q: a template ref may not contain %q (it is reserved for instance ids)",
			templateRef, instanceSep)
	case lc == nil:
		return fmt.Errorf("mint instance of %q: shard has no retained content to build from", templateRef)
	case lc.Zone(templateRef) == nil:
		// REACHABLE AT RUNTIME since #418, not only on a typo: the snapshot now tracks reloads, so a builder
		// who deletes a dungeon out from under a player lands here. The player-facing surface is
		// instance_entry.go's refusal line, which is deliberately generic for exactly this reason.
		return fmt.Errorf("mint instance of %q: no such zone in loaded content", templateRef)
	case !lc.Zone(templateRef).Instanceable:
		// The content-side opt-in. See the header: every zone being instanceable is an uncapped item faucet
		// (a mint runs the zone's boot resets) that also routes around every in-world access gate.
		return fmt.Errorf("mint instance of %q: the zone is not declared instanceable; a zone must opt in with "+
			"`instanceable: true` before it can be used as an instance template", templateRef)
	}
	// A START ROOM IS MANDATORY FOR AN INSTANCE, and this is where that is discovered — loudly, at mint, in
	// front of the builder who authored the template — rather than at a player's death hours later (#72).
	//
	// An ordinary zone can get away without one: nothing routes a fresh login into a zone that is not somebody's
	// durable location, and resolveRoom's fallback to a nil start room simply leaves the caller's ref standing.
	// An INSTANCE cannot, for two independent reasons:
	//
	//   - Entry lands via transferIn's resolveRoom(""), which IS the start-room fallback. With no start room it
	//     resolves nil, transferIn takes its "destination has no rooms" branch, and the player is DISCONNECTED —
	//     mid-entry, having done nothing wrong.
	//   - respawnPlayer moves the victim to resolveRoom(z.startRoom). With no start room that is nil, so a
	//     player who dies at the boss is revived, at full health, standing in the boss room. The engine has no
	//     cross-zone respawn to fall back on; death.go evicts them to their anchor instead, which is a correct
	//     but degraded outcome nobody authored.
	//
	// The ref must also name a room the template actually declares. A start_room pointing at a room that was
	// renamed or moved to another zone produces exactly the same nil, one indirection later.
	zd := lc.Zone(templateRef)
	if zd.StartRoom == "" {
		return fmt.Errorf("mint instance of %q: the template declares no start_room, which an instance requires "+
			"(entry lands in the start room, and a death inside would have nowhere to respawn)", templateRef)
	}
	hasStart := false
	for _, r := range zd.Rooms {
		if r.Ref == zd.StartRoom {
			hasStart = true
			break
		}
	}
	if !hasStart {
		return fmt.Errorf("mint instance of %q: the template's start_room %q names no room the template declares",
			templateRef, zd.StartRoom)
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
		// ABANDONED instances do not count against their account (#72 M3). The zone still exists until the
		// reaper retires it — so it still counts toward perShard above, which is an honest bound on live zones
		// — but the account should not be billed for a copy nobody is in and nobody can re-enter. Without this
		// exclusion, two commands (ask to enter, then walk away or quit) pin one of the account's slots for the
		// mint grace plus the idle ticks, ~3 minutes, at essentially no cost to the caller: a self-inflicted
		// lockout for an ordinary player who changed their mind, and a cheap self-denial-of-service otherwise.
		if rec.account == accountID && !rec.abandoned {
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

var (
	// templateUseInterval is the cadence of the template-in-use heartbeat (#416).
	//
	// It is the reaper's interval but NOT the reaper's ticker, and that separation is the whole point. The
	// heartbeat first rode the reaper's tick, which made its renewal cadence `interval + sweepDuration` —
	// and the sweep is serial over UnhostZone, each call waiting up to unhostActorGrace (10s) on an actor
	// (#419). Five wedged instances therefore stretched the gap past the TTL and lapsed EVERY claim on the
	// shard, including healthy templates with parties inside them, which is precisely the fail-open #416
	// exists to close. Worse, wedged actors are a correlated condition (a stalled store, GC pressure), and
	// the sweep is longest right after a busy period — exactly when live templates matter most.
	//
	// A TTL sized against a cadence that a colocated operation can stretch without bound is a margin on
	// paper. Its own goroutine makes the cadence real.
	templateUseInterval = 15 * time.Second

	// templateUseTTL is how long a published claim survives without renewal.
	//
	// Three intervals, not one: a TTL equal to the cadence lapses on any tick that runs slightly late, and a
	// lapsed claim reads to the prune guard as "nobody is using this template" — the one answer that lets a
	// pack be stripped out from under live parties. Three means two consecutive missed renewals before that
	// can happen, while still expiring a genuinely crashed shard's claim inside a minute.
	templateUseTTL = 3 * templateUseInterval
)

// runTemplateUsePublisher heartbeats this shard's in-use instance templates to the directory (#416).
// Started by Run, on its OWN goroutine and ticker — see templateUseInterval for why it is not the reaper's.
//
// It also serves the mint KICK. A template whose first copy has just been minted would otherwise be
// invisible to the prune guard until the next tick, and that is the worst case there is: a brand-new
// template has no prior claim to fall back on, so the guard would read "nobody is using this" for a zone a
// party is standing in. The kick advertises on creation, so the claim's lifecycle is
// advertise-on-create → renew-on-tick → expire-on-death, with no cold-start hole.
func (s *Shard) runTemplateUsePublisher(ctx context.Context) {
	if s.tmplUsePublisher == nil {
		return // no directory: nothing to advertise to
	}
	t := time.NewTicker(templateUseInterval)
	defer t.Stop()
	s.publishTemplatesInUse(ctx) // advertise immediately; do not make a restart wait a full interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.publishTemplatesInUse(ctx)
		case tmpl := <-s.tmplUseKick:
			// One template, one write — the freshly minted one. The periodic sweep still covers it from the
			// next tick on, so a dropped kick (full channel) costs at most one interval.
			s.publishTemplateClaims(ctx, []string{tmpl})
		}
	}
}

// kickTemplateUse asks the publisher goroutine to advertise template NOW, without blocking the caller.
//
// Non-blocking on purpose. The caller is a mint worker, and there are only a few of them: letting a slow
// directory occupy one would turn a Redis blip into player-visible mint latency, or into mint refusals once
// the queue backs up. A dropped kick is not a lost claim — the periodic sweep picks the template up on the
// next tick.
func (s *Shard) kickTemplateUse(template string) {
	if s.tmplUsePublisher == nil || s.tmplUseKick == nil || template == "" {
		return
	}
	select {
	case s.tmplUseKick <- template:
	default:
	}
}

// publishTemplatesInUse heartbeats the DISTINCT templates this shard currently has live instances of (#416).
//
// One claim per TEMPLATE, never per instance. A template ref is authored content, so the keyspace is bounded
// by the pack; an instance id is minted per dungeon run from 128 bits of randomness, and putting an
// unbounded player-driven keyspace into the directory is exactly what #411 declined to do when it made
// instances unleased.
//
// It counts RESERVED records too, not only published ones. A reserved record is a mint in flight — positive
// evidence that somebody is running copies of this template right now — and the asymmetry decides it: over-
// advertising delays a legitimate prune by at most the TTL, while under-advertising strips a pack out from
// under a live party. The only cost of a mint that then fails is one TTL of an unprunable template.
//
// Runs off every zone goroutine.
func (s *Shard) publishTemplatesInUse(ctx context.Context) {
	// Snapshot under mu, publish outside it: the publish is network I/O and must not be held across the
	// mutex that every routing read (zoneByID, claimTransferTarget) takes.
	s.mu.Lock()
	seen := make(map[string]struct{}, len(s.instances))
	templates := make([]string, 0, len(s.instances))
	for _, rec := range s.instances {
		if _, dup := seen[rec.template]; dup {
			continue
		}
		seen[rec.template] = struct{}{}
		templates = append(templates, rec.template)
	}
	s.mu.Unlock()
	s.publishTemplateClaims(ctx, templates)
}

// publishTemplateClaims writes one batch of template claims. Best-effort: a failure leaves the claims to
// lapse and the next tick retries, logged at Warn because a lapsed claim is the precondition for the very
// fail-open this signal exists to close — it should be visible to an operator, not buried at Debug.
// templateUsePublishTimeout bounds one batched claim write. One round trip, so this is generous.
const templateUsePublishTimeout = 2 * time.Second

func (s *Shard) publishTemplateClaims(ctx context.Context, templates []string) {
	if s.tmplUsePublisher == nil || len(templates) == 0 {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, templateUsePublishTimeout)
	defer cancel()
	// ONE batched call, not one per template. A serial loop under a single shared deadline degrades in the
	// worst possible shape: once the budget is spent, every REMAINING template silently fails, and Go
	// randomizes map order so the starved subset rotates each tick — a lapse that is both invisible and
	// unreproducible. Batching makes the whole tick one round trip and one outcome.
	if err := s.tmplUsePublisher.SetTemplatesInUse(pctx, templates, s.shardID, templateUseTTL); err != nil {
		slog.Warn("instance template in-use publish failed; the claims may lapse and a content pull could "+
			"prune a pack that has live instances", "templates", templates, "err", err)
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
		if now.Sub(rec.minted) < instanceMintGrace && !rec.abandoned {
			// Nobody has had the chance to enter yet. Entry is a separate mechanism that necessarily runs
			// after the mint returns, so an ungraced instance would be reaped out from under its own party.
			//
			// An ABANDONED instance skips the grace, because the grace's entire premise is false for it: the
			// grace exists to protect a copy whose entrant has not arrived YET, and hop 3 has already decided
			// this one's entrant is never arriving. Nobody else can reach it (an instance id is unguessable and
			// is never handed to content), so waiting out the remaining grace protects nothing and just holds a
			// zone, an actor goroutine and a Lua VM. It still has to pass the quiescence check below and
			// UnhostZone's re-check under mu, so this shortens the wait without weakening the entering-vs-reaping
			// race argument at all.
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
	sort.Strings(due) // deterministic DISPATCH order (map iteration is randomized); completion order is not
	s.reapConcurrently(ctx, due)
}

// instanceReapConcurrency bounds how many instances one sweep retires at a time (#419).
//
// The sweep used to be serial, and UnhostZone waits up to unhostActorGrace (10s) for a zone's actor to
// return. So ONE wedged instance delayed every other reap behind it by 10 seconds, and k wedged instances
// delayed the tail by 10k — while the ticker coalesced behind the whole thing. Nothing about the reaps is
// ordered with respect to each other (each is an independent teardown of an independent zone), so the
// serialization bought nothing and cost the worst case.
//
// Bounded rather than unbounded to control the goroutine burst and the burst of routing-mutex acquisitions
// against live traffic. Note what the bound is NOT doing: each worker takes s.mu and releases it BEFORE it
// blocks on the actor wait, so 256 workers would mostly queue on the mutex rather than hold it — the same
// total mutex work, just burstier. Each hold is a scan of tokenIndex and residentZone, sub-millisecond at
// MUD populations. So this is a smoothing bound, not a correctness one.
const instanceReapConcurrency = 8

// reapConcurrently retires the due instances with bounded parallelism, and waits for the batch.
//
// It WAITS deliberately. Fire-and-forget would let successive ticks pile unbounded goroutines onto the same
// wedged instance — the ticker would keep re-dispatching a teardown that takes 10s to fail, forever. Waiting
// means a slow sweep simply delays the next one, and time.Ticker DROPS ticks its receiver misses (its
// channel holds one) rather than queuing them, so sweeps can never overlap or pile up.
//
// The cost of that is not only latency, and it is worth naming: rec.idle advances once per COMPLETED SWEEP,
// not once per wall-clock tick. So on a shard where sweeps run long, retiring a newly-idle instance takes
// instanceIdleTicks *sweeps* — and while dead instances sit in s.instances, reserveInstanceSlot counts them
// against the per-shard cap and can refuse live mints. Wedged instances do not accumulate across sweeps
// (UnhostZone deletes the record even on its timeout path, so a wedged one is reaped once and never
// re-swept), which is what keeps this bounded rather than progressive.
func (s *Shard) reapConcurrently(ctx context.Context, due []string) {
	if len(due) == 0 {
		return
	}
	sem := make(chan struct{}, instanceReapConcurrency)
	var wg sync.WaitGroup
	for _, id := range due {
		// Checked BEFORE the select, deliberately. With ctx already done and sem free, a select on both
		// would have two ready cases and Go picks between them uniformly at random — so roughly half the
		// remaining ids would be dispatched anyway, and "stop dispatching" would be true only on average.
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			// A DETACHED context per teardown, not the sweep's (#419). UnhostZone passes its ctx to the
			// actor wait, so handing it the shard's ctx meant a shutdown cancelled every in-flight teardown
			// mid-commit: each had already removed the zone and its record and cancelled the actor, then
			// bailed out of the wait. `wg.Wait()` returned almost instantly and looked graceful while
			// leaving up to instanceReapConcurrency zones half torn down.
			//
			// Bounded by the same grace the wait itself uses (plus slack, so the wait's own timeout is what
			// fires and reports the wedged actor, rather than this one racing it to a less specific error).
			// Modeled on unadoptZone, which detaches for the same reason.
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unhostActorGrace+time.Second)
			defer cancel()
			if err := s.UnhostZone(tctx, id); err != nil {
				// Ordinary: someone entered between the sample and the re-check, or the actor is busy. Give
				// the instance a fresh idle budget so it is not retried on every single tick.
				s.resetInstanceIdle(id)
				slog.Debug("instance reap deferred", "zone", id, "err", err)
				return
			}
			slog.Info("reaped idle zone instance", "zone", id)
		}(id)
	}
	wg.Wait()
}

// abandonInstance marks a successfully-minted instance as one its entrant never entered (#72 M3). Called from
// the ENTRANCE ZONE's goroutine at hop 3, on every path that decides not to move the player after the mint
// succeeded. Idempotent; a no-op for an id with no record (already reaped).
//
// IT IS DELIBERATELY NOT A RECORD DELETION, and that distinction is the whole design. The reaper iterates
// s.instances and resolves each record through s.zones; a record with no zone is skipped, but a ZONE WITH NO
// RECORD is never visited at all. So deleting the record here would free the cap slot and permanently orphan
// the zone object, its actor goroutine and its Lua VM — trading a 3-minute slot pin for an unbounded leak, on a
// path an attacker controls. The flag frees the slot NOW and leaves the record in place so the reaper still
// finds, retires and accounts for the zone.
//
// It also does not tear the zone down here. UnhostZone is a BLOCKING teardown (it waits on the zone's actor)
// and this runs on a zone goroutine — calling it would stall the entrance zone, which is the exact failure the
// whole async entry flow exists to avoid (see instance_entry.go's header).
func (s *Shard) abandonInstance(id string) {
	s.mu.Lock()
	if rec := s.instances[id]; rec != nil {
		rec.abandoned = true
	}
	s.mu.Unlock()
}

// resetInstanceIdle clears an instance's consecutive-quiescent counter.
func (s *Shard) resetInstanceIdle(id string) {
	s.mu.Lock()
	if rec := s.instances[id]; rec != nil {
		rec.idle = 0
	}
	s.mu.Unlock()
}
