package world

import (
	"log/slog"

	"github.com/double-nibble/telosmud/internal/content"
)

// bootvalidate.go — the BOOT half of the #423 content publish gate.
//
// #423 made the shard's runtime content-snapshot refresh (reload.go refreshContentSnapshot) run its packs
// through validatePacks and REFUSE to publish broken content, keeping the previous snapshot. That closed a
// real hole — a reload the operator watched get REJECTED no longer went live anyway at the next unrelated
// content event — but it bought that with a divergence: boot has always read rows raw, so a shard restarted
// while bad rows are deployed serves them while a long-running shard is still refusing them. Two shards on
// the same rows, different content, forever, decided by uptime.
//
// # Why boot LOGS and boots anyway rather than refusing
//
// The asymmetry is principled, not an oversight. The runtime gate can afford to be strict because it has a
// strictly better fallback available: a known-good previous snapshot it can keep serving. Boot has no
// previous snapshot. Refusing there would convert a content defect into an OUTAGE, which contradicts the
// whole reason the embedded core pack exists (content.LoadWithCore layers it precisely so a fresh or broken
// deployment still boots a start room and lets a builder connect and fix things).
//
// So boot's job is not to gate — it is to make the runtime gate's freeze OPERATIONALLY VISIBLE. Without this
// file, the divergence above is silent: the long-running shards log an Error nobody correlates, the restarted
// ones log nothing at all, and the fleet splits with no single line saying so. With it, every shard that
// boots on rejected content says exactly that, at Error, naming the same problems the runtime gate names.
//
// # The concrete cost of the divergence, so the Error line means something
//
// Mostly the split is a staleness difference. There is one player-facing edge worth naming: a restarted
// shard's snapshot can contain a zone that a frozen peer's does not. If the restarted shard hosts that zone
// and then drains, the frozen peer REFUSES to adopt it (HostZone -> errNoZoneContent -> FailedPrecondition),
// so that zone cannot be evacuated and its drain stalls. Not a dupe and not a disconnect — the zone keeps
// running where it is — but a drain that cannot complete is an operator problem, and this Error is the only
// warning that the precondition for it exists.
//
// NOTE the narrowing (#423): the RUNTIME gate rejects only zone-graph findings, while this boot report runs
// the FULL validatePacks. That asymmetry is intentional — detection here is deliberately broader than the
// veto there — so a finding reported at boot will not always correspond to a runtime freeze.
//
// This is deliberately the SAME validatePacks the runtime gate and the staff `reload` gate use, so the three
// paths cannot drift into disagreeing about what "broken" means.

// ReportBootContentProblems validates the packs a shard is about to boot on and LOGS any problems at Error,
// returning them so the caller (and a test) can see what was found. It NEVER refuses: the returned slice is
// diagnostics, not a gate — see the file header for why boot's posture differs from the runtime refresh's.
//
// `enabled` is the shard's enabled pack list; the embedded core pack is excluded from the provenance scope
// exactly as the runtime refresh excludes it (snapshotScope), since a core finding is an engine bug the
// operator cannot fix by editing rows and must not be reported as their broken deploy.
func ReportBootContentProblems(packs []content.Pack, enabled []string) []string {
	scoped := make(map[string]bool, len(enabled))
	for _, p := range enabled {
		if p != content.CorePack {
			scoped[p] = true
		}
	}
	problems := validatePacks(packs, scoped)
	if len(problems) == 0 {
		return nil
	}
	slog.Error("BOOTING ON CONTENT THAT FAILS VALIDATION. Boot does not refuse content (there is no previous "+
		"snapshot to fall back on, and refusing would turn a content defect into an outage) — but a RUNNING "+
		"shard's content-snapshot refresh DOES refuse these rows, so this shard now serves different content "+
		"than shards that have not restarted. Fix the rows and re-deploy to converge the fleet",
		"packs", enabled, "problems", problems)
	return problems
}
