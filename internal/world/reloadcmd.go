package world

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
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
	// "Core"); the bare/"all" keyword is matched case-insensitively in scopePacks.
	arg := strings.TrimSpace(c.Arg(0))
	packs, msg := r.scopePacks(arg)
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
	c.Send(fmt.Sprintf("reload: propagating a content reload of %s to all shards… (the fan-out completes in the background)", packLabel(packs)))
	// Off the zone goroutine: each publish flushes to NATS (a per-ref round-trip), so a synchronous loop
	// here would stall the zone. The applier on every shard (this one included) re-reads and swaps; a
	// re-read/publish failure is logged by republish, never surfaced to the builder mid-command.
	go r.republish(context.Background(), packs)
	return nil
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

// republish re-reads each pack in packs from the shard's content source and publishes a per-ref
// invalidation for each (contentbus.PublishPack) so every subscribed shard hot-swaps. It runs OFF the
// zone goroutine (cmdReload spawns it). Every failure is logged and non-fatal — content reload is
// best-effort, exactly like the seed/OLC publish path. The content source must implement the full
// content.Source (LoadPacks); both production sources (the pgx store, the embedded pack) do — a source
// that cannot re-read whole packs disables the command rather than crashing.
func (r *reloader) republish(ctx context.Context, packs []string) {
	src, ok := r.src.(content.Source)
	if !ok {
		r.log.Warn("reload: content source cannot re-read whole packs; reload skipped", "packs", packs)
		return
	}
	// Bound the background work so a wedged Postgres re-read or a hung NATS flush cannot leak this
	// goroutine indefinitely (mirrors the applier's reloadIOTimeout discipline). The whole re-read +
	// per-ref publish fan-out for a pack runs under one deadline.
	ctx, cancel := context.WithTimeout(ctx, reloadRepublishTimeout)
	defer cancel()
	loaded, err := src.LoadPacks(ctx, packs)
	if err != nil {
		r.log.Warn("reload: re-read of content packs failed; nothing propagated", "packs", packs, "err", err)
		return
	}
	total := 0
	for _, pk := range loaded {
		n, perr := contentbus.PublishPack(ctx, r.bus, pk)
		total += n
		if perr != nil {
			r.log.Warn("reload: publishing invalidations failed (partial)", "pack", pk.Pack, "published", n, "err", perr)
		}
	}
	r.log.Info("reload: content reload propagated across the fleet", "packs", packs, "invalidations", total)
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
