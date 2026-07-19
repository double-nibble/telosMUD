package content

import (
	"context"
	"strings"
)

// coreLayered serves the embedded CorePack UNDERNEATH a delegate Source (production: Postgres).
// It is how the minimal bootstrap pack (#212) is guaranteed present on every boot: LoadPacks
// returns the embedded core pack FIRST, then the delegate's packs for the real enabled names, so
// content.Load's last-write-wins-by-ref merge lets real content OVERRIDE core. The delegate is
// never asked for CorePack — core is embedded-only and is never seeded into the delegate.
type coreLayered struct{ delegate Source }

// LoadPacks emits the embedded core pack first, then delegates the remaining (non-core) enabled
// names. A nil delegate (Postgres unreachable / bare boot) still yields core, so even a
// backing-store-less server boots the bootstrap zone instead of an empty, login-rejecting world.
func (c coreLayered) LoadPacks(ctx context.Context, enabled []string) ([]Pack, error) {
	var out []Pack
	// core FIRST (embedded-only), so any real pack overrides it by ref in the merge.
	corePacks, err := EmbeddedSource{}.LoadPacks(ctx, []string{CorePack})
	if err != nil {
		return nil, err
	}
	out = append(out, corePacks...)
	if c.delegate == nil {
		return out, nil
	}
	// The delegate serves every REAL (non-core) name; core is never in the delegate, so filter it
	// out rather than double-loading it (EmbeddedSource already contributed it).
	realNames := make([]string, 0, len(enabled))
	for _, n := range enabled {
		if n != CorePack {
			realNames = append(realNames, n)
		}
	}
	packs, err := c.delegate.LoadPacks(ctx, realNames)
	if err != nil {
		return nil, err
	}
	return append(out, packs...), nil
}

// LoadWithCore assembles the world content with the minimal embedded core pack (#212) layered
// UNDER the real enabled packs read from src. It is the production boot read (cmd/telos-world):
// the core bootstrap zone is ALWAYS present so a fresh/empty deployment boots a start room and a
// builder can connect and pull real content; real content read from src overrides core by ref.
// A nil src (Postgres unreachable) yields the core pack alone — the degraded-but-bootable path.
//
// CorePack is prepended to the enabled list so the list is never empty (Load short-circuits to
// empty content on an empty enabled list, which would skip core) and core is always merged
// FIRST — real packs from src override it by ref.
func LoadWithCore(ctx context.Context, src Source, enabled []string) (*LoadedContent, error) {
	packs, err := LoadPacksWithCore(ctx, src, enabled)
	if err != nil {
		return nil, err
	}
	LintPacks(packs)
	return Merge(packs), nil
}

// LoadPacksWithCore returns the RAW core-layered pack slice LoadWithCore merges: the embedded core pack
// FIRST, then src's packs for the enabled names, in that order. It is LoadWithCore stopped one step
// early — same read, same layering, same order — so a caller can INSPECT the packs before deciding
// whether to merge and publish them.
//
// That seam is what the shard's live content-snapshot refresh needs (#423): it validates the packs and
// publishes content.Merge of the SAME slice, so the thing gated is byte-identically the thing deployed.
// Doing it with two reads instead would reintroduce a TOCTOU — an import committing between them would
// let unvalidated rows go live under a validated read's approval.
//
// It skips the boot content-lints (LintPacks) deliberately: those are logging, and a caller that
// re-reads on every content event would emit them on a loop. LoadWithCore still runs them.
func LoadPacksWithCore(ctx context.Context, src Source, enabled []string) ([]Pack, error) {
	// CorePack is prepended for the same two reasons LoadWithCore has always prepended it: the list is
	// then never empty, and core merges FIRST so real packs override it by ref.
	full := append([]string{CorePack}, enabled...)
	return coreLayered{delegate: src}.LoadPacks(ctx, full)
}

// LoadCorePack loads just the embedded core pack into a LoadedContent, for tests and the
// core-only degraded boot. Mirrors LoadDemoPack.
func LoadCorePack() (*LoadedContent, error) {
	return Load(context.Background(), EmbeddedSource{}, []string{CorePack})
}

// CoreRefViolation is one finding: a NON-core pack shipped a world-ref (zone/room/item/mob) under
// the reserved core: namespace. Such a ref clobbers a bootstrap-pack ref via the last-write-wins
// merge, so the namespace is reserved for the embedded core pack alone.
type CoreRefViolation struct {
	Pack string // the offending pack name
	Kind string // "zone" | "room" | "item" | "mob"
	Ref  string // the reserved-namespace ref it shipped
}

// LintReservedCoreRefs returns a finding for every world-ref a NON-core pack ships under the
// reserved CoreRefPrefix. Build-time, non-fatal (the caller logs, like the world content-lints):
// the merge still applies, but a real pack clobbering the bootstrap room is almost certainly a
// mistake. The core pack's own core: refs are exempt (it owns the namespace). Only zone/room/item/
// mob refs are checked — pack-global def refs (attributes/resources/…) are deliberately NOT
// core-prefixed and a real pack overriding e.g. max_hp is the intended layering, not a violation.
func LintReservedCoreRefs(packs []Pack) []CoreRefViolation {
	var out []CoreRefViolation
	for _, p := range packs {
		if p.Pack == CorePack {
			continue // the core pack owns the namespace
		}
		for _, z := range p.Zones {
			// The bare "core" zone token is reserved too (it collides with the bootstrap zone even
			// without a trailing colon), as is anything under the core: prefix.
			if z.Ref == CoreZone || strings.HasPrefix(z.Ref, CoreRefPrefix) {
				out = append(out, CoreRefViolation{Pack: p.Pack, Kind: "zone", Ref: z.Ref})
			}
			for _, r := range z.Rooms {
				if strings.HasPrefix(r.Ref, CoreRefPrefix) {
					out = append(out, CoreRefViolation{Pack: p.Pack, Kind: "room", Ref: r.Ref})
				}
			}
			for _, it := range z.Items {
				if strings.HasPrefix(it.Ref, CoreRefPrefix) {
					out = append(out, CoreRefViolation{Pack: p.Pack, Kind: "item", Ref: it.Ref})
				}
			}
			for _, mb := range z.Mobs {
				if strings.HasPrefix(mb.Ref, CoreRefPrefix) {
					out = append(out, CoreRefViolation{Pack: p.Pack, Kind: "mob", Ref: mb.Ref})
				}
			}
		}
	}
	return out
}
