package content

import (
	"fmt"
	"strings"
)

// channelaccesslint.go — the load-time content-lint for present-but-ZERO comms channel access conditions
// (#60). A channel's `access` / `hear_access` block is a CONTENT predicate the world evaluates before it lets
// a speaker publish (or a listener hear). An EMPTY predicate is a legitimate, common shape: `access: {}` is an
// open channel and `hear_access: {}` is the "announce" channel (privileged speakers, everyone hears). The
// footgun is a predicate that LOOKS restrictive but resolves to open because a condition was left blank:
//
//   - `require_flag:` with no value — a present-but-null key. It decodes identically to an absent key, so a
//     builder who typos a restriction silently gets the open shape. Detected from the raw YAML node
//     (ChannelAccessDTO.emptyConds); only the YAML authoring path can see it, which is exactly where the
//     mistake is made.
//   - a PARTIAL `min_attr` — `attr:` without `min:` (min defaults 0, so every attribute passes) or `min:`
//     without `attr:` (no attribute named). "Both Attr and Min must be set to take effect" (the DTO), so a
//     half-filled floor is a no-op. Detected at the struct level, so it fires on every load path.
//
// Like the other content-lints (LintTrustLadder, LintRefCharset, LintReservedCoreRefs) this is WARN-only and
// non-fatal: the channel is trusted content, the engine still boots to the (open) shape, and an author's typo
// is authoring noise, not a vulnerability. It exists so the mistake is SAID rather than discovered when a
// restricted channel turns out to be world-readable.

// ChannelAccessViolation is one present-but-zero access-condition finding against a pack's channel.
type ChannelAccessViolation struct {
	Pack    string // the pack that ships the channel
	Channel string // the channel ref
	Field   string // "access" or "hear_access"
	Detail  string // operator-facing explanation
}

// LintChannelAccess returns a finding per present-but-zero access condition across packs' channels. A channel
// with no access blocks, or with a deliberately-empty one (`{}`), is clean — only a condition KEY that is
// present but blank (or a half-specified min_attr) is a finding. Deterministic order: packs in input order,
// channels in pack order, access before hear_access.
func LintChannelAccess(packs []Pack) []ChannelAccessViolation {
	var out []ChannelAccessViolation
	for _, p := range packs {
		for _, ch := range p.Channels {
			out = append(out, lintAccessBlock(p.Pack, ch.Ref, "access", &ch.Access)...)
			if ch.HearAccess != nil {
				out = append(out, lintAccessBlock(p.Pack, ch.Ref, "hear_access", ch.HearAccess)...)
			}
		}
	}
	return out
}

// lintAccessBlock reports the present-but-zero conditions in one access predicate. The findings split into two
// directions the builder still wants to know about: a blank condition that leaves the channel OPEN (a
// present-null require_flag/min_attr, an omitted min floor), and a blank condition that leaves it UNREACHABLE
// (a whitespace-only flag no entity carries, a min on an unnamed attribute) — both signal a typo.
func lintAccessBlock(pack, channel, field string, a *ChannelAccessDTO) []ChannelAccessViolation {
	var out []ChannelAccessViolation
	add := func(detail string) {
		out = append(out, ChannelAccessViolation{Pack: pack, Channel: channel, Field: field, Detail: detail})
	}
	// Present-but-empty condition KEY (`require_flag:` / `min_attr:` null or `{}`): the condition is dropped,
	// so the channel is OPEN. The common intent for a present key is to restrict — a silent open is the footgun.
	for _, cond := range a.emptyConds {
		add(fmt.Sprintf("`%s` names `%s:` but leaves it empty — a present-but-null condition decodes the same as "+
			"an absent one, so the channel is left OPEN. If you meant to restrict, fill it in; if you meant an "+
			"open/announce channel, drop the key.", field, cond))
	}
	// A min_attr that names an attribute but OMITS `min` (raw-node detected — Min==0 is indistinguishable from
	// an explicit floor of 0 at the struct level). min defaults to 0, which is very likely not the floor the
	// author intended.
	if a.minAttrMissingMin {
		add(fmt.Sprintf("`%s.min_attr` names an attribute but omits `min` — it defaults to 0, very likely not the "+
			"floor you intended; set `min:` explicitly (or drop min_attr for no floor).", field))
	}
	// A whitespace-only require_flag survives to the struct as a non-empty-but-blank value; catch it here so it
	// fires on a PG/JSON reload too (where emptyConds is gone). Unlike the empty case this fails CLOSED — no
	// entity carries a blank flag, so the channel is unreachable. On the YAML path emptyConds already flagged a
	// blank require_flag, so skip to avoid double-reporting.
	if rf := a.RequireFlag; rf != "" && strings.TrimSpace(rf) == "" && !containsStr(a.emptyConds, "require_flag") {
		add(fmt.Sprintf("`%s` requires a flag that is whitespace-only (%q) — no entity carries it, so the "+
			"condition can never pass and the channel is unreachable; name a real flag or drop it.", field, rf))
	}
	// A min_attr with a floor but NO attribute named: the floor applies to attribute "" (which resolves to 0),
	// so it can never pass — unreachable. Struct-level (fires on every load path). Skip if emptyConds already
	// flagged a wholly-empty min_attr.
	if m := a.MinAttr; m != nil && strings.TrimSpace(m.Attr) == "" && m.Min != 0 && !containsStr(a.emptyConds, "min_attr") {
		add(fmt.Sprintf("`%s.min_attr` sets min=%g but names no attribute — the floor resolves against nothing and "+
			"can never pass; add `attr:` or drop min_attr.", field, m.Min))
	}
	return out
}

// containsStr reports whether s is in xs.
func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
