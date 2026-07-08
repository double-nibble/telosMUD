package content

import (
	"fmt"
	"strings"
)

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

// CheckPackSetConsistency detects the #259 runtime divergence axis that the boot-time #246 fix and the #248
// staleness guard both miss: an EXPLICIT operator override (cfg.ContentPacks / TELOS_CONTENT_PACKS) that
// disagrees with the pack set the current content_version actually PUBLISHED (the registry). Because the world
// APPLIES the content trust ladder as engine flags while telos-account AUTHORIZES promotes against it, a
// process pinned to a different (or differently-ordered) set than what was published — while a sibling process
// follows the registry — reopens the builder→admin escalation #246 closed. The #248 guard compares only
// content_version, which is EQUAL here, so it never fires.
//
// The registry is the SHARED source of truth, so each process detects its OWN divergence LOCALLY with no
// inter-process advertisement: if a process's explicit override does not exactly match the published set, it
// is (or a sibling is) serving content other than what was published. A fresh / never-pulled DB (empty
// registry) is the legitimate bootstrap-override path — there is nothing published to diverge from — and
// passes; so does a process with no explicit override (it follows the registry by construction). Load ORDER
// is significant (a later pack overrides an earlier one by ref), so the comparison is order-sensitive.
//
// Returns a non-nil error describing the divergence; callers (cmd/telos-world, cmd/telos-account) turn it into
// a fatal boot refusal unless TELOS_ALLOW_INSECURE explicitly opts in, mirroring the handoff/caller-token
// fail-closed posture.
func CheckPackSetConsistency(explicit, registry []string) error {
	if len(explicit) == 0 || len(registry) == 0 {
		return nil
	}
	if !equalStringSlices(explicit, registry) {
		return fmt.Errorf("content pack-set divergence (#259): this process pins TELOS_CONTENT_PACKS=[%s] but the "+
			"current content_version published [%s] — the world and telos-account would load DIFFERENT trust "+
			"ladders at the same content_version (builder→admin escalation). Remove the override to follow the "+
			"published set, pin it to exactly [%s], or set TELOS_ALLOW_INSECURE on a trusted rig",
			strings.Join(explicit, ","), strings.Join(registry, ","), strings.Join(registry, ","))
	}
	return nil
}

// equalStringSlices reports whether two string slices are equal element-for-element, in order.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
