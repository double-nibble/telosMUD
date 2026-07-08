package content

// packselect.go — the SINGLE authoritative resolver for "which content packs does this process load"
// (#212 slice 4, shared since #246). Both telos-world and telos-account must resolve the SAME pack set: the
// world APPLIES the content-defined trust ladder as engine flags, while telos-account AUTHORIZES promotes
// against it, so if the two loaded different packs a pack could redefine a tier's flags on one side only and
// escalate (builder→admin) past a ceiling the other side never sees. Before this, telos-world resolved via
// this precedence but telos-account hardcoded the demo pack — the divergence #246 closes.

// ResolveEnabledPacks picks the content packs a process loads from Postgres, by precedence:
//
//  1. An explicit operator override (cfg.ContentPacks / TELOS_CONTENT_PACKS) always wins — for pinning to a
//     subset, or dev.
//  2. Else the packs the currently imported content version REGISTERED (from content_pack_registry, read as
//     ContentVersionInfo.Packs) — so a process auto-serves exactly what telos-pull / the director last
//     imported, with no operator list to keep in sync.
//  3. Else the demo pack — a fresh DB that was never pulled (dev / bootstrap).
//
// The embedded core bootstrap pack is layered UNDER these by LoadWithCore, so it is never listed here.
// Callers on both services MUST pass the same two inputs (the operator override and the registry packs) so
// the resolution is identical process-to-process.
func ResolveEnabledPacks(explicit, registry []string) []string {
	if len(explicit) > 0 {
		return explicit
	}
	if len(registry) > 0 {
		return registry
	}
	return []string{DemoPack}
}
