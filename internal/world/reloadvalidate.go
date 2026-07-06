package world

import (
	"fmt"

	"github.com/double-nibble/telosmud/internal/content"
)

// reloadvalidate.go — pre-publish content PACK-HEALTH gate for `reload` (#192, Option B: shard-local
// validate-before-broadcast). Before a shard broadcasts a content reload fleet-wide, it dry-run-checks the
// re-read pack(s) and REJECTS the publish (naming the problems) when the content is DEFINITIVELY BROKEN —
// so a bad edit can't be propagated. Because every shard re-reads the same source, gating the publish at the
// triggering shard gates the whole fleet (shared-source convergence), which is why this needs no central
// director tier (see the #192 design note).
//
// WHAT "definitively broken" MEANS HERE — an honest scope note (mudlib review). Boot's builders are
// deliberately FAIL-SAFE: a bad attribute formula is logged and registered with a zero base, an attribute
// cycle is logged, a room/item/mob/channel always builds SOMETHING (buildChannelDef/buildPrototype
// log-and-continue). So "boot would build it" is nearly always true — boot never aborts. This gate is
// therefore STRICTER than boot on purpose: it rejects content boot would tolerate-but-degrade, on the
// theory that a builder should FIX a broken pack rather than propagate a silently-degraded one. It reuses
// boot's two ERROR-RETURNING functions (parseAttributeBase + lintAttributeCycles) as the faithful signal.
//
// COVERAGE CAVEAT: those two functions cover ATTRIBUTES, which are a BOOT concern — PublishPack propagates
// rooms/items/mobs/channels, NOT attributes. So v1 is a pack-HEALTH proxy ("is this pack sound to push?"),
// not payload validation ("is the room I edited valid?"): a broken attribute blocks pushing the pack's
// other content, but the propagated kinds are not yet independently error-checked (they have no clean
// error surface at boot). Growing coverage to the published kinds (channel access/format first) is tracked
// as #197. The framework here — collect problems -> gate the publish -> report to the builder — is what new
// checks slot into.
//
// Validation is PURE over the parsed DTOs (it reuses parseAttributeBase + lintAttributeCycles and builds a
// THROWAWAY attributeDef map — never the shard's live registries), so it is safe on the off-zone-goroutine
// republish path and never touches live state. Only definitively-broken content blocks; a transient re-read
// blip stays best-effort (handled in republish), preserving reload's optional posture.

// reloadOutcome is the result of a republish attempt, shaped so the builder readout can distinguish the
// three cases: a clean propagation, a validation REJECTION (nothing published — the content is broken), and
// a best-effort INFRA failure (a re-read/publish blip; the applier's per-ref fail-safe is the backstop).
type reloadOutcome struct {
	published int      // invalidations put on the wire (0 when rejected)
	rejected  []string // validation problems; NON-EMPTY => nothing was published (a hard content gate)
	failed    bool     // an infrastructure failure (re-read/publish error) — logged, best-effort
}

// validatePacks returns one human-readable problem per definitively-broken thing in the re-read packs, or
// nil when they validate. A non-empty result MUST block the publish. Attributes merge across packs
// last-write-wins by ref (mirroring content.Load), so a cycle spanning packs is caught WHEN both packs are
// in scope. NOTE: a scoped `reload <onepack>` only re-reads that pack, so a cycle spanning it and a
// NOT-reloaded pack is invisible here (lintAttributeCycles treats an out-of-scope ref as a non-edge — safe,
// never a false positive, but asymmetric with boot which lints the full merged graph). Acceptable since
// attributes don't hot-swap on this path anyway; full-graph validation is part of #197.
func validatePacks(loaded []content.Pack) []string {
	var problems []string
	// Build the merged attribute-def graph exactly as boot does (build.go defineGlobals): parse each base
	// formula with the SAME parser, last-write-wins by ref. A parse failure is a problem; a successfully
	// parsed set is then cycle-checked with the SAME lint boot runs.
	attrDefs := map[string]*attributeDef{}
	for i := range loaded {
		for _, a := range loaded[i].Attributes {
			base, err := parseAttributeBase(a)
			if err != nil {
				problems = append(problems, fmt.Sprintf("attribute %q: bad base formula: %v", a.Ref, err))
				continue
			}
			attrDefs[a.Ref] = &attributeDef{ref: a.Ref, base: base}
		}
	}
	for _, err := range lintAttributeCycles(attrDefs) {
		problems = append(problems, err.Error())
	}
	return problems
}
