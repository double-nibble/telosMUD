package world

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// reloadRepublishTimeout bounds the whole background re-read + per-ref publish fan-out one `reload`
// triggers, so a hung content source or NATS flush cannot leak the detached goroutine. Generous vs the
// applier's single-ref reloadIOTimeout because it covers a full pack's worth of publishes.
const reloadRepublishTimeout = 30 * time.Second

// reloadcmd.go — the trust-gated staff `reload [<pack>]` command (#53, the content install/reload
// pipeline). It is the IN-GAME trigger the settled design names ("in-game builder `reload <scope>`
// command → the same publish API"): a builder edits content in the external store, syncs it into this
// shard's content SOURCE (Postgres, via `make seed` / a deploy), then runs `reload` to PROPAGATE the
// change to every live shard with no restart — instead of shelling out.
//
// It re-reads the named pack(s) from the shard's content source and PUBLISHES a per-ref invalidation for
// each, exactly as telos-seed does after an import; every shard subscribed to the content bus (this one
// included) re-reads and hot-swaps the changed prototypes via the existing applier (reload.go). Because
// all shards re-read the SAME shared source, they converge — no split-brain — so the bus fan-out gives
// multi-shard consistency for this per-ref propagation without central coordination.
//
// SCOPE + LIMITS (v1). Scope is PACK-granular: bare `reload` republishes every pack this shard loads;
// `reload <pack>` just that one. The per-ref applier swaps EXISTING prototypes in place, and a trailing
// zone-SHAPE reconcile (#191) adds/removes rooms; so this propagates edits AND structural add/remove for
// rooms/mobs/items/channels. It does NOT hot-swap shared defs (attributes/abilities — a rolling reboot, by
// design, per the settled design note). Central DIRECTOR-side validate-before-broadcast and finer zone/region scope are the tracked
// follow-ups on #53. The publish loop runs OFF the zone goroutine (each NATS publish flushes — a per-ref
// round-trip), so the command acks immediately and the fan-out completes in the background.

// cmdReload propagates a content hot-reload for the scoped pack(s) across the fleet. Two trust checks: the
// dispatch USE gate (MinRank=rankStaff) makes the verb invisible to non-staff, and here a CAPABILITY gate
// requires the builder flag — fleet-wide content propagation is a builder power, not something a bare
// moderator/staff tier should wield (the flag is granted by the builder + admin tiers, content-defined via
// the ladder, so this stays policy-agnostic). A shard with no content bus (a bare/dev shard) or a source
// that cannot re-read whole packs reports the feature unavailable rather than falsely claiming success.
func cmdReload(c *Context) error {
	sh := c.z.shard
	if sh == nil || sh.reloader == nil {
		c.Send("reload: content hot-reload is not available here (no content bus configured).")
		return nil
	}
	// CAPABILITY gate: require the builder flag (granted by builder/admin tiers). A staff tier without it
	// (e.g. a moderator) can see nothing here — the dispatch MinRank already hid the verb below staff rank.
	if !hasFlag(c.Actor, flagBuilder) {
		c.Send("reload: you lack the builder capability to reload content.")
		return nil
	}
	r := sh.reloader
	// Pack names are content-defined identifiers, NOT verbs — do not case-fold them (a pack "Core" must stay
	// "Core"); the bare/"all" keyword is matched case-insensitively in scopePacks. `--check` is a dry-run
	// flag (validate, report, publish NOTHING) — the builder's pre-flight (#192).
	scope, checkOnly := parseReloadArgs(c.Rest())
	packs, msg := r.scopePacks(scope)
	if msg != "" {
		c.Send(msg)
		return nil
	}
	// Fail fast (synchronously) if this shard's source cannot re-read whole packs, so the ack never claims a
	// propagation that can't happen. Transient re-read/publish failures inside republish are still best-
	// effort (logged), matching the seed/OLC posture.
	if !r.canRepublish() {
		c.Send("reload: this shard's content source cannot re-read packs; reload unavailable.")
		return nil
	}
	if checkOnly {
		c.Send(fmt.Sprintf("reload: validating %s (check only — nothing will propagate)…", packLabel(packs)))
	} else {
		c.Send(fmt.Sprintf("reload: validating and propagating %s… (the result follows in the background)", packLabel(packs)))
	}
	// Off the zone goroutine: each publish flushes to NATS (a per-ref round-trip), so a synchronous loop
	// here would stall the zone. Capture the triggering zone + builder id as locals (immutable) so the
	// goroutine can post the OUTCOME back to be shown on the zone goroutine (reloadDoneMsg) — the applier
	// on every shard (this one included) re-reads and swaps in the meantime.
	z, pid, label := c.z, c.s.character, packLabel(packs)
	go func() {
		out := r.republish(context.Background(), packs, checkOnly)
		auditReload(sh, pid, packs, out) // #192 S3: record who/what/when to the world director (fire-and-forget)
		z.postOrDrop(reloadDoneMsg{player: pid, summary: reloadSummary(label, out)})
	}()
	return nil
}

// reloadSummary renders the builder-facing readout for a republish outcome. Pure (no I/O), so the readout
// wording — including the #56 shared-def rolling-reboot reminder — is unit-testable without driving the whole
// command. Cases: a validation REJECTION (nothing propagated), a --check dry run, an infra failure (full or
// partial), and a clean propagation. On any successful/partial reload whose content also defines shared defs
// (abilities/formulas/pvp policy/…), a reminder is appended — those have no hot-reload loop, so a CHANGE to
// one needs a rolling reboot; without the reminder a live-edited shared def silently keeps its boot value.
func reloadSummary(label string, out reloadOutcome) string {
	var summary string
	switch {
	case len(out.rejected) > 0:
		// #192: the content did not validate — nothing went to the fleet. Name the problems so the
		// builder can fix the pack before re-running.
		summary = fmt.Sprintf("reload: %s REJECTED — content failed validation, nothing propagated:\r\n  - %s",
			label, strings.Join(out.rejected, "\r\n  - "))
	case out.checkOnly:
		summary = fmt.Sprintf("reload --check: %s validated OK — no problems found (nothing propagated).", label)
	case out.failed && out.published == 0:
		summary = fmt.Sprintf("reload: %s — propagation FAILED, 0 definitions published; see the server log.", label)
	case out.failed:
		summary = fmt.Sprintf("reload: %s partially propagated — %d definitions published; see the server log for errors.", label, out.published)
	default:
		summary = fmt.Sprintf("reload: %s propagated — %d definitions pushed to all shards.", label, out.published)
	}
	// #56: rooms/items/mobs/channels hot-swap live, but the pack-global SHARED DEFS do not. Remind the
	// operator when the reloaded content defines any (also on a --check, so a pre-flight flags it too). Not
	// appended on a hard rejection (nothing was applied at all) or an infra failure (the result is unreliable).
	if len(out.sharedDefs) > 0 && len(out.rejected) == 0 && !out.failed {
		summary += fmt.Sprintf("\r\n  Note: this content also defines shared defs (%s). These are NOT hot-applied — "+
			"if you changed one, a rolling reboot of the world shards is required for it to take effect.",
			strings.Join(out.sharedDefs, ", "))
	}
	// #309: advisory heads-ups — a not-reloaded pack references a room/proto this reload removes. Non-blocking
	// (the reload still propagated), but the operator should know a dependency now dead-ends. Not shown on a
	// hard rejection (nothing was applied) — the advisories are moot there.
	if len(out.advisories) > 0 && len(out.rejected) == 0 {
		summary += fmt.Sprintf("\r\n  Heads-up: this reload removes content another (not-reloaded) pack still "+
			"references — those references will dead-end:\r\n  - %s", strings.Join(out.advisories, "\r\n  - "))
	}
	return summary
}

// parseReloadArgs splits the `reload` command tail into the pack-scope arg and the check-only (dry-run)
// flag. Recognized: `reload`, `reload <pack>`, `reload --check`, `reload <pack> --check` (flag in either
// position). The scope is the first non-flag token (empty => all loaded packs); a pack name is NOT
// case-folded (it is a content identifier, not a verb).
func parseReloadArgs(rest string) (scope string, checkOnly bool) {
	for _, tok := range strings.Fields(rest) {
		switch tok {
		case "--check", "-n":
			checkOnly = true
		default:
			if scope == "" {
				scope = tok
			}
		}
	}
	return scope, checkOnly
}

// scopePacks resolves the command argument to the pack list to republish: empty/"all" => every pack this
// shard loads; a name => just that pack (rejected if the shard does not load it, so a typo fails loudly
// rather than silently reloading nothing). The second result is a user-facing message to echo back when
// the scope is invalid ("" on success) — a display string, not a Go error.
func (r *reloader) scopePacks(arg string) (packs []string, msg string) {
	all := r.enabledSorted()
	if arg == "" || strings.EqualFold(arg, "all") {
		if len(all) == 0 {
			return nil, "reload: this shard loads no content packs."
		}
		return all, ""
	}
	if !r.packs[arg] {
		return nil, fmt.Sprintf("reload: this shard does not load a pack named %q (loaded: %s).", arg, strings.Join(all, ", "))
	}
	return []string{arg}, ""
}

// canRepublish reports whether this shard's content source can re-read whole packs (implements the full
// content.Source, not only DefinitionSource) — the precondition for republish. All production sources do;
// the check lets cmdReload refuse up front rather than claim a propagation that would silently no-op.
func (r *reloader) canRepublish() bool {
	_, ok := r.src.(content.Source)
	return ok
}

// enabledSorted returns this shard's enabled pack names in a stable sorted order (for display + a
// deterministic republish order). A copy — never hand the caller the reloader's backing slice.
func (r *reloader) enabledSorted() []string {
	out := append([]string(nil), r.enabled...)
	sort.Strings(out)
	return out
}

// republish re-reads each pack from the shard's content source, VALIDATES it (validatePacks — the #192
// pre-publish gate), and only then publishes a per-ref invalidation for each (contentbus.PublishPack) so
// every subscribed shard hot-swaps. It runs OFF the zone goroutine (cmdReload spawns it). The returned
// reloadOutcome distinguishes a clean propagation, a validation REJECTION (nothing published — the content
// is broken), and a best-effort INFRA failure (a re-read/publish blip). The content source must implement
// the full content.Source (LoadPacks); both production sources (the pgx store, the embedded pack) do.
//
// Validation gates ONLY the publish, never the live cache — a broken pack is simply not propagated, and the
// per-ref applier's fail-safe (keep last-known on a re-read error) remains the runtime backstop. A re-read
// failure stays best-effort (failed, logged), NOT a hard content rejection, since it may be transient.
func (r *reloader) republish(ctx context.Context, packs []string, checkOnly bool) reloadOutcome {
	src, ok := r.src.(content.Source)
	if !ok {
		r.log.Warn("reload: content source cannot re-read whole packs; reload skipped", "packs", packs)
		return reloadOutcome{failed: true}
	}
	// Bound the background work so a wedged Postgres re-read or a hung NATS flush cannot leak this
	// goroutine indefinitely (mirrors the applier's reloadIOTimeout discipline). The whole re-read +
	// validate + per-ref publish fan-out for a pack runs under one deadline.
	ctx, cancel := context.WithTimeout(ctx, reloadRepublishTimeout)
	defer cancel()
	loaded, err := src.LoadPacks(ctx, packs)
	if err != nil {
		// Best-effort infra failure (may be a transient re-read blip), NOT a hard content rejection.
		r.log.Warn("reload: re-read of content packs failed; nothing propagated", "packs", packs, "err", err)
		return reloadOutcome{failed: true}
	}
	// #192 GATE: dry-run validate against the boot builders. Definitively-broken content blocks the publish
	// fleet-wide (shared-source convergence), so a bad edit never propagates. This runs for BOTH a real
	// reload and a `--check` dry run.
	// #205 full merged-graph validation: resolve refs against the WHOLE live content graph (so a cross-pack
	// attribute cycle or a cross-zone dangling exit/reset resolves), but attribute rejections to the reloaded
	// (scoped) packs — an unrelated broken pack must not block this reload. `full` is loaded fresh from ALL
	// enabled packs in ENABLED/BOOT ORDER (r.enabled — NOT the sorted scope list), so last-writer provenance
	// matches what content.Load makes live; the embedded CORE pack is layered UNDER it (as boot's LoadWithCore
	// does) so a reloaded exit/reset into a core room/proto resolves. Core is context-only (never in scope). A
	// context re-read failure is a best-effort INFRA failure — NOT a fall-through to the now-strict cross-zone
	// checks against a PARTIAL graph, which would falsely reject a legitimate cross-zone reference.
	full, ferr := src.LoadPacks(ctx, r.enabled)
	if ferr != nil {
		r.log.Warn("reload: full-graph context re-read failed; nothing propagated", "packs", packs, "err", ferr)
		return reloadOutcome{failed: true}
	}
	if corePacks, cerr := (content.EmbeddedSource{}).LoadPacks(ctx, []string{content.CorePack}); cerr == nil {
		full = append(corePacks, full...) // core UNDERNEATH the enabled set: an enabled pack overriding a core ref still wins
	}
	scoped := make(map[string]bool, len(packs))
	for _, p := range packs {
		scoped[p] = true
	}
	if problems := validatePacks(full, scoped); len(problems) > 0 {
		r.log.Warn("reload: content failed validation; nothing propagated", "packs", packs, "problems", problems)
		return reloadOutcome{rejected: problems}
	}
	// Content-lint (#60): WARN (non-blocking) on a present-but-empty/partial channel access condition so a
	// staff hot-edit that typos a channel restriction is surfaced in the reload readout, not only at next
	// boot. The struct-level checks (a whitespace-only flag, a min on an unnamed attribute) fire on the PG
	// re-read here; the present-null require_flag/min_attr catch needs the YAML ingress (import/seed), where
	// it is also wired. WARN not reject — a too-open channel is authoring noise, not fleet-state corruption.
	for _, v := range content.LintChannelAccess(loaded) {
		r.log.Warn("reload: channel access-condition lint (present-but-empty/partial condition; likely a typo)",
			"pack", v.Pack, "channel", v.Channel, "field", v.Field, "detail", v.Detail)
	}
	// #56: the pack-global shared defs (abilities/formulas/pvp policy/…) have no hot-reload loop; they take
	// effect only after a rolling reboot. Surface which are present so the readout reminds the operator (a
	// shared-def edit is otherwise silently un-applied). Computed for both a dry run and a real reload.
	shared := sharedDefKinds(loaded)
	// #309: ADVISORY (non-blocking) — a NOT-reloaded pack references a room/proto THIS reload removes. #205
	// correctly does not hard-block the reload for an out-of-scope pack's dependency, but the operator should
	// still hear about it. `wasLive` is the pre-reload snapshot: r.cache holds the CURRENT live prototypes, and
	// a removal lands only when the publish below drives the zone-shape reconcile — AFTER this line — so at
	// validate time the cache is the "before" graph. A target absent from the re-read content but still live
	// here is one this reload removes. Computed for both a dry run and a real reload.
	scope := newReloadScope(full, scoped)
	advisories := advisoryReloadRemovals(scope, func(ref string) bool {
		return r.cache != nil && r.cache.get(ProtoRef(ref)) != nil
	})
	for _, a := range advisories {
		r.log.Warn("reload: advisory — a not-reloaded pack depends on content this reload removes", "detail", a)
	}
	// #411: ADVISORY (non-blocking) — a zone being reloaded has live INSTANCES, which are PINNED to the
	// content they were minted from (reload.go freezes both the shape reconcile and the Lua fan-out for them).
	// Without this line the freeze is invisible: a builder edits a dungeon, the readout says the reload
	// succeeded, and the parties currently inside keep playing the old version with nothing to explain why.
	// Same shape as the #309 advisories above and computed for a dry run too, so `reload --check` answers
	// "who is mid-run right now" before the edit lands.
	for _, a := range pinnedInstanceAdvisories(r.shard, scope) {
		r.log.Warn("reload: advisory — a reloaded zone has live instances, which stay on their minted content", "detail", a)
		advisories = append(advisories, a)
	}
	if checkOnly {
		// Pre-flight: the content validated, but a dry run deliberately publishes nothing.
		r.log.Info("reload --check: content validated (dry run, nothing published)", "packs", packs)
		return reloadOutcome{checkOnly: true, sharedDefs: shared, advisories: advisories}
	}
	out := reloadOutcome{sharedDefs: shared, advisories: advisories}
	// A shard-local `reload` is a staff hot-edit of already-imported rows: it stamps its invalidations with
	// a version minted from the single PG content-version authority (#232) — an atomic bump, monotonic
	// fleet-wide with no wall clock — so it interoperates with the director-coordinated pull's PG-minted
	// version at the reconcile guard, and a shard that missed the broadcast reconciles on rejoin. Falls back
	// to the pre-#232 wall-clock stamp only when the source has no PG authority (embedded/mem) — see
	// mintReloadVersion.
	version, ok := r.mintReloadVersion(ctx)
	if !ok {
		// A PG-authority bump failed (#232): fail the reload rather than stamp a wall-clock version that
		// could poison later durable reloads' shape reconciles. Nothing was published; the operator re-runs.
		r.log.Warn("reload: aborting — could not mint a durable content version (PG bump failed); nothing propagated")
		return reloadOutcome{failed: true}
	}
	for _, pk := range loaded {
		n, perr := contentbus.PublishPack(ctx, r.bus, pk, version)
		out.published += n
		if perr != nil {
			out.failed = true
			r.log.Warn("reload: publishing invalidations failed (partial)", "pack", pk.Pack, "published", n, "err", perr)
		}
	}
	r.log.Info("reload: content reload propagated across the fleet", "packs", packs, "invalidations", out.published, "failed", out.failed)
	return out
}

// mintReloadVersion mints the version a shard-local reload stamps on its invalidations, returning ok=false
// to tell the caller to FAIL the reload rather than stamp a poisonous version.
//
// DURABLE path (#232), taken whenever the source has a PG authority (a contentVersionBumper — *store.Pool
// in production): bump the single content-version counter atomically, so the stamp is monotonic FLEET-WIDE
// with no wall clock. This closes the two residuals #222 left — a clock-AHEAD shard can no longer mint a
// far-future version that silently drops a later pull's zone-shape reconcile, and the bump is visible to
// reconcile-on-join so a shard that missed the reload's bus message catches up on rejoin. If the bump
// ERRORS (a transient PG failure), we return ok=false so the reload FAILS: stamping a current-wall-clock
// version here would sit ABOVE the (un-bumped) PG counter, so a SUBSEQUENT durable reload minting
// pgVersion+1 would be below it and have its zone-shape reconcile dropped fleet-wide as stale — the very
// residual this issue closes. Failing loud (the operator re-runs) is safe; poisoning the cursor is not.
//
// FALLBACK path (a source with NO PG authority — the embedded/mem source used in dev/tests): there is no
// cross-shard clock skew and no reconcile-on-join, so the pre-#222 wall-clock stamp (floored at pgVersion+1
// when the source reports a version) is fine and ok is always true.
func (r *reloader) mintReloadVersion(ctx context.Context) (version uint64, ok bool) {
	// Durable: mint through the PG-monotonic authority — no clock, so no skew poisoning.
	if b, isBumper := r.src.(contentVersionBumper); isBumper {
		v, err := b.BumpContentVersion(ctx)
		if err != nil {
			r.log.Warn("reload: could not bump the PG content version; FAILING the reload rather than stamping a wall-clock version that could drop later reloads' shape reconciles", "err", err)
			return 0, false
		}
		return v, true
	}
	// No PG authority (embedded/mem): the wall-clock stamp is safe (single process, no fleet reconcile).
	now := uint64(time.Now().UnixNano())
	vs, isVersioner := r.src.(contentVersioner)
	if !isVersioner {
		return now, true
	}
	pg, err := vs.ContentVersion(ctx)
	if err != nil {
		r.log.Warn("reload: could not read the content version for the mint floor; using bare nanos", "err", err)
		return now, true
	}
	if floor := pg + 1; floor > now {
		return floor, true
	}
	return now, true
}

// auditReload fires a fire-and-forget DURABLE signal UP to the world director recording who ran `reload`,
// which packs, and the outcome (#192 S3, the director-side advisory audit). Called from the background
// republish goroutine (off the zone goroutine — enqueueSignal is safe there and never blocks). It is
// best-effort accountability, not a correctness path: a shard with no scoped bus (sh.scopes nil) no-ops,
// and a dropped/failed signal never affects the reload. A `--check` dry run is NOT audited — it changed
// nothing on the fleet, so it is not an audit-worthy content event.
func auditReload(sh *Shard, actor string, packs []string, out reloadOutcome) {
	if out.checkOnly {
		return
	}
	outcome := "propagated"
	switch {
	case len(out.rejected) > 0:
		outcome = "rejected"
	case out.failed && out.published == 0:
		outcome = "failed"
	case out.failed:
		outcome = "partial"
	}
	payload, err := json.Marshal(contentbus.ReloadAudit{
		Actor:     actor,
		Packs:     packs,
		Published: out.published,
		Outcome:   outcome,
		AtUnixMs:  time.Now().UnixMilli(),
	})
	if err != nil {
		sh.reloader.log.Warn("reload: marshal audit payload failed; audit skipped", "err", err)
		return
	}
	// World-scoped: a content reload is a fleet event, so the WORLD director records it. enqueueSignal
	// tolerates a nil scopeReplication (no scoped bus) and a full queue — both a clean no-op.
	sh.scopes.enqueueSignal(scopeSignalJob{
		scope:   scopebus.World(),
		event:   contentbus.ReloadAuditEvent,
		payload: payload,
	})
}

// packLabel renders a pack-name list for the builder's ack line: `pack "demo"` for one, `packs "a", "b"`
// for several. Purely cosmetic.
func packLabel(packs []string) string {
	quoted := make([]string, len(packs))
	for i, p := range packs {
		quoted[i] = fmt.Sprintf("%q", p)
	}
	if len(packs) == 1 {
		return "pack " + quoted[0]
	}
	return "packs " + strings.Join(quoted, ", ")
}
