package world

import (
	"fmt"

	"github.com/double-nibble/telosmud/internal/content"
)

// bundle.go — the Phase-11.4b TEMPLATE/BUNDLE machinery (docs/PHASE11-PLAN.md §11.4, gap [G6c]): a class/
// race/background/feat/talent is content (bundle_defs) — a kind discriminator + a grant op-list applied
// as a unit. apply_bundle(bundle) runs the bundle's grants on an entity (chargen picks a class+race; a
// track step or a "join guild" action applies one mid-life; a prestige class gates the apply behind a
// `check`). The engine knows only "apply this bundle's grants"; the bundles are content. Single-writer:
// the op runs on the zone goroutine.

// bundleDef is the runtime form of a content BundleDTO: the kind + the parsed grant op-list.
type bundleDef struct {
	ref      string
	kind     string
	uncapped bool // a profession bundle that does NOT count against the learned-profession cap (gathering/utility)
	grants   []effectOp
}

// buildBundleDef maps a content.BundleDTO onto the runtime bundleDef, parsing its grant op-list. A parse
// error returns the partial def + the error (registered with whatever parsed — content-lint is the gate).
func buildBundleDef(b content.BundleDTO) (*bundleDef, error) {
	def := &bundleDef{ref: b.Ref, kind: b.Kind, uncapped: b.Uncapped}
	ops, err := parseOpList(b.Grants)
	if err != nil {
		return def, fmt.Errorf("bundle %s grants: %w", b.Ref, err)
	}
	def.grants = ops
	return def, nil
}

// opApplyBundle: apply_bundle(target, bundle) — apply a class/race/feat bundle's grants to the target. It
// runs the bundle's grant op-list (modify_attribute_base / grant_ability / grant_track / set_flag / …) on
// the same ctx, so every grant op composes. Multiclass / join-a-guild / a chargen bundle all funnel here;
// a prestige class's entry prerequisite is a `check` in the CALLING content, gating the apply.
func opApplyBundle(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("apply_bundle: no target")
	}
	if op.bundle == "" {
		return fmt.Errorf("apply_bundle: no bundle")
	}
	def := c.z.bundleDefs().get(op.bundle)
	if def == nil {
		return fmt.Errorf("apply_bundle: unknown bundle %q", op.bundle)
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	if len(def.grants) > 0 {
		runOps(c, def.grants) // the bundle's grants run on the same ctx (c.target = the entity being built)
	}
	return nil
}
