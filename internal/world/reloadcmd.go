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
// `reload <pack>` just that one. The per-ref applier swaps EXISTING prototypes in place, so this
// propagates edits to rooms/mobs/items/channels; it does NOT yet add/remove rooms (a zone-SHAPE change)
// nor hot-swap shared defs (attributes/abilities — a rolling reboot, by design, per the settled design
// note). Central DIRECTOR-side validate-before-broadcast and finer zone/region scope are the tracked
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
		z.postOrDrop(reloadDoneMsg{player: pid, summary: summary})
	}()
	return nil
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
	if problems := validatePacks(loaded); len(problems) > 0 {
		r.log.Warn("reload: content failed validation; nothing propagated", "packs", packs, "problems", problems)
		return reloadOutcome{rejected: problems}
	}
	if checkOnly {
		// Pre-flight: the content validated, but a dry run deliberately publishes nothing.
		r.log.Info("reload --check: content validated (dry run, nothing published)", "packs", packs)
		return reloadOutcome{checkOnly: true}
	}
	out := reloadOutcome{}
	// A shard-local `reload` is a staff hot-edit of already-imported rows: it stamps its invalidations with
	// a DIRECTOR-INDEPENDENT, nanos-scale version (never-fatal) so it interoperates with the director-
	// coordinated pull's PG-minted version at the reconcile guard. mintReloadVersion floors that stamp at
	// pgVersion+1 (#222) so a reload right after a pull always advances past the current content version even
	// when this shard's clock lags the host that minted the pull — otherwise the reload's zone-SHAPE
	// reconcile would be silently dropped by the version guard as stale.
	version := r.mintReloadVersion(ctx)
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

// mintReloadVersion stamps a shard-local reload's invalidations as max(now_nanos, pgVersion+1): the
// wall-clock nanos keep it director-independent and nanos-scale, while the pgVersion+1 FLOOR guarantees the
// stamp always advances past the current Postgres content version — closing the cross-host clock-skew race
// (#222) where a reload issued right after a director pull, on a shard whose clock lags the puller's, would
// carry a version below the pull's and have its zone-SHAPE reconcile dropped by the guard as stale. When
// the PG version can't be read (a source without ContentVersion, or a transient read error), it falls back
// to bare nanos — the guard degrades to the pre-#222 behavior, never worse.
func (r *reloader) mintReloadVersion(ctx context.Context) uint64 {
	now := uint64(time.Now().UnixNano())
	vs, ok := r.src.(contentVersioner)
	if !ok {
		return now
	}
	pg, err := vs.ContentVersion(ctx)
	if err != nil {
		r.log.Warn("reload: could not read the content version for the mint floor; using bare nanos", "err", err)
		return now
	}
	if floor := pg + 1; floor > now {
		return floor
	}
	return now
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
