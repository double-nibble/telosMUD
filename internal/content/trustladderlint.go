package content

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// trustladderlint.go — the load-time content-lint for the content-defined trust ladder (#111). The ladder
// (trust_tier_defs, TrustTierDTO) is TRUSTED content: it is authored via `make seed` / ImportPack, so a
// malformed ladder is operator error, not a vulnerability. But the ladder is also the ONLY content-writable
// channel to the engine's reserved trust flags (an effect op cannot set them), and several authoring mistakes
// are silent, wide-blast-radius, and discovered only at someone's next login. So they get a lint:
//
//  1. A tier granting a flag that is not a CAPABILITY. world.applyTierFlags applies only holylight/builder/
//     admin; anything else — a typo, an invented name, or `wizinvis` (reserved but session-scoped and
//     deliberately un-grantable, see nonCapabilityFlags) — is silently DROPPED at apply time. WARN: the
//     author's intent does not happen, but nothing is unsafe.
//
//  2. The BASELINE tier granting a capability. The baseline is what every un-elevated account resolves to, so
//     `player: {flags: [holylight]}` gives the whole playerbase see-all and `[admin]` gives it promote/demote.
//     Easy to do by accident (a copy-pasted rung); catastrophic in effect. REJECT.
//
//  3. DUPLICATE RANKS. Rank is the ordinal the promote ceiling compares, and two tiers sharing one leave the
//     ladder's ordering ambiguous. REJECT.
//
//  4. An EMPTY tier name. "" is the sentinel every fail-safe path degrades to (an unknown tier; a session
//     whose tier did not cross a handoff). Both ladders DROP a nameless rung so that degradation stays safe —
//     but an author who wrote one meant something, and silently discarding it is worse than saying so. REJECT.
//
//  5. A NON-NESTED capability ladder: an admin-capable tier that does not dominate a LOWER-ranked tier. Such a
//     tier can never GRANT the richer one (the grant-side promote ceiling is unconditional), so an
//     archon(50,{admin}) in a ladder with builder(20,{holylight,builder}) may demote builders but never create
//     one. Legal, occasionally intended, usually a mistake. WARN.
//
// NOTE ON LAYERING: none of this is the security control. TrustLadder.TierDominates is, and it must hold for
// ANY ladder — including one seeded before this lint existed, or one whose findings were only warnings. This
// file exists so an operator never has to depend on that. Placement mirrors the other content-lints
// (LintRefCharset, LintReservedCoreRefs): WARN-only at boot in content.Load (the engine must still boot on
// imperfect content — the bare-engine invariant), and a hard REJECT at the reload-broadcast gate
// (world/reloadvalidate.go) for Reject-severity findings, so a bad ladder never enters a fleet reload.

// TrustLadderSeverity ranks a finding: a Warn is authoring noise the engine tolerates; a Reject would change
// who is elevated (or leave the rank ordering ambiguous) and must not reach a live fleet.
type TrustLadderSeverity int

const (
	// TrustLadderWarn — the ladder is safe, but the author's intent is silently not honored.
	TrustLadderWarn TrustLadderSeverity = iota
	// TrustLadderReject — the ladder elevates the wrong people, or its rank ordering is ambiguous.
	TrustLadderReject
)

func (s TrustLadderSeverity) String() string {
	if s == TrustLadderReject {
		return "reject"
	}
	return "warn"
}

// TrustLadderViolation is one lint finding against a pack's trust ladder.
type TrustLadderViolation struct {
	Pack     string              // the pack that ships the ladder
	Tier     string              // the offending tier ("" when the finding is about the ladder as a whole)
	Severity TrustLadderSeverity // Warn (tolerated) vs Reject (never broadcast)
	Detail   string              // operator-facing explanation
}

// LintTrustLadder returns a finding per trust-ladder footgun across packs. A pack shipping no trust_tiers is
// skipped: it inherits the engine default ladder, which is lint-clean by construction (pinned by
// TestDefaultLadderIsLintClean).
//
// The BASELINE is resolved per-pack via TrustLadder.Baseline (lowest rank, name as the tiebreak) — NOT the
// literal name "player", which a pack may rename (#112). When several tiers tie for the lowest rank the
// duplicate-rank finding already fires; the baseline check still runs against the tiebreak winner, so fixing
// one finding does not unmask the other on a second pass.
//
// Findings are deterministic in order (tiers by name within a pack, packs in input order), so the reload
// gate's problem list does not churn between runs.
func LintTrustLadder(packs []Pack) []TrustLadderViolation {
	var out []TrustLadderViolation
	for _, p := range packs {
		if len(p.TrustTiers) == 0 {
			continue // no ladder shipped => the engine default applies; nothing to lint
		}
		tiers := make([]TrustTierDTO, len(p.TrustTiers))
		copy(tiers, p.TrustTiers)
		sort.Slice(tiers, func(i, j int) bool { return tiers[i].Name < tiers[j].Name })

		// (4) Nameless rungs, reported first: both ladder constructors DROP them, so every check below would
		// silently not see them, and the author deserves to know their rung does not exist.
		named := make([]TrustTierDTO, 0, len(tiers))
		for _, t := range tiers {
			if t.Name == "" {
				out = append(out, TrustLadderViolation{
					Pack: p.Pack, Severity: TrustLadderReject,
					Detail: fmt.Sprintf("a tier has an empty name (rank %d, flags %v); \"\" is the sentinel an "+
						"unknown or un-carried tier degrades to, so a nameless rung is DROPPED rather than let "+
						"that degradation grant its flags — give it a name", t.Rank, t.Flags),
				})
				continue
			}
			named = append(named, t)
		}
		if len(named) == 0 {
			continue
		}

		// (3) Duplicate ranks — one finding per colliding rank, naming every tier at it.
		byRank := map[int][]string{}
		for _, t := range named {
			byRank[t.Rank] = append(byRank[t.Rank], t.Name)
		}
		ranks := make([]int, 0, len(byRank))
		for r := range byRank {
			ranks = append(ranks, r)
		}
		sort.Ints(ranks)
		for _, r := range ranks {
			names := byRank[r]
			if len(names) < 2 {
				continue
			}
			out = append(out, TrustLadderViolation{
				Pack: p.Pack, Severity: TrustLadderReject,
				Detail: fmt.Sprintf("tiers %s share rank %d; rank is the ordinal the promote ceiling compares, "+
					"so a shared rank leaves the ladder's ordering ambiguous", strings.Join(names, ", "), r),
			})
		}

		ladder := NewTrustLadder(named)
		baseline := ladder.Baseline()
		for _, t := range named {
			for _, f := range t.Flags {
				// (1) A flag the engine will never apply from a tier grant.
				if !IsTierCapabilityFlag(f) {
					out = append(out, TrustLadderViolation{
						Pack: p.Pack, Tier: t.Name, Severity: TrustLadderWarn,
						Detail: fmt.Sprintf("grants %s, which a tier cannot confer; only %s are grantable "+
							"capabilities, so this grant is silently dropped at login%s",
							strconv.Quote(f), strings.Join(TierCapabilityFlags(), ", "), wizinvisHint(f)),
					})
					continue
				}
				// (2) The baseline granting a capability elevates EVERY un-elevated account.
				if t.Name == baseline {
					out = append(out, TrustLadderViolation{
						Pack: p.Pack, Tier: t.Name, Severity: TrustLadderReject,
						Detail: fmt.Sprintf("the BASELINE tier (lowest rank) grants capability %s; every "+
							"un-elevated account resolves to the baseline, so this elevates the entire playerbase",
							strconv.Quote(f)),
					})
				}
			}
		}

		// (5) Non-nested capability ladder: an admin-capable tier that cannot grant a tier beneath it.
		for _, actor := range named {
			if !ladder.GrantsFlag(actor.Name, FlagAdmin) {
				continue
			}
			for _, other := range named {
				// STRICTLY lower rank only: an equal-rank richer peer is already a duplicate-rank REJECT, and
				// "outranks" would misdescribe it. A strictly-higher other is out of reach of a grant anyway.
				if other.Name == actor.Name || other.Rank >= actor.Rank || ladder.TierDominates(actor.Name, other.Name) {
					continue
				}
				out = append(out, TrustLadderViolation{
					Pack: p.Pack, Tier: actor.Name, Severity: TrustLadderWarn,
					Detail: fmt.Sprintf("is admin-capable and outranks %q, but lacks %s that %q grants; the promote "+
						"ceiling will let it DEMOTE a %s yet never CREATE one",
						other.Name, strings.Join(ladder.MissingCapabilities(actor.Name, other.Name), ", "),
						other.Name, other.Name),
				})
			}
		}
	}
	return out
}

// wizinvisHint appends the one-line reason wizinvis in particular is un-grantable — an author naming it has
// made a reasonable-looking mistake rather than a typo, so the generic "unknown flag" message would mislead.
func wizinvisHint(f string) string {
	if f != FlagWizinvis {
		return ""
	}
	return " (wizinvis is a session-scoped staff concealment, cleared at every login and set with `wizinvis on`" +
		" — not a tier grant)"
}
