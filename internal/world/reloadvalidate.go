package world

import (
	"fmt"
	"sort"
	"strings"

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
// COVERAGE: the attribute checks cover a BOOT concern (attributes do NOT hot-swap on this path — they need
// a rolling reboot), so they are a pack-HEALTH proxy ("is this pack sound to push?"). validateChannels
// (#197 slice 1) adds real PAYLOAD validation for the FIRST propagated kind: channels DO hot-swap here
// (reloadChannel/buildChannelDef), so a structurally-dead channel is rejected against the same build path.
// validateRoomExits (#197 slice 2) adds the FIRST room-graph payload check: a dangling exit — one pointing
// to a room that provably does not exist. Rooms also hot-swap here (the per-ref applier + resyncRoom, #191),
// so an edit that adds a room with a broken exit is caught before it propagates. Remaining #197 work: item/
// mob prototype buildability and a full-graph dry-run for cross-pack cycles a scoped reload misses. The
// framework here — collect problems -> gate the publish -> report to the builder — is what each check slots into.
//
// Validation is PURE over the parsed DTOs (it reuses parseAttributeBase + lintAttributeCycles and builds a
// THROWAWAY attributeDef map — never the shard's live registries), so it is safe on the off-zone-goroutine
// republish path and never touches live state. Only definitively-broken content blocks; a transient re-read
// blip stays best-effort (handled in republish), preserving reload's optional posture.

// reloadOutcome is the result of a republish attempt, shaped so the builder readout can distinguish the
// three cases: a clean propagation, a validation REJECTION (nothing published — the content is broken), and
// a best-effort INFRA failure (a re-read/publish blip; the applier's per-ref fail-safe is the backstop).
type reloadOutcome struct {
	published int      // invalidations put on the wire (0 when rejected or check-only)
	rejected  []string // validation problems; NON-EMPTY => nothing was published (a hard content gate)
	failed    bool     // an infrastructure failure (re-read/publish error) — logged, best-effort
	checkOnly bool     // true => a `reload --check` dry run that validated OK and deliberately published nothing
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
	// Payload validation for the propagated kinds. Channels and rooms DO hot-swap on this path, so they are
	// checked against the same build path boot uses (#197 slices 1 + 2).
	problems = append(problems, validateChannels(loaded)...)
	problems = append(problems, validateRoomExits(loaded)...)
	return problems
}

// validateRoomExits reports one problem per room exit that PROVABLY points nowhere in the re-read content —
// a dangling exit (RoomDTO.Exits maps a direction to a destination room ref; roomComponents wires it into
// the live Room graph verbatim, no trimming, so the runtime routes on the raw target). Rooms hot-swap on
// this path (the per-ref applier + resyncRoom, #191), so a reload adding a room with a broken exit is caught
// before it propagates. Boot merely tolerates a dangling exit (the move just fails), but this gate is
// deliberately stricter — a dangling exit is an authoring error, not an intended degradation.
//
// The check holds the NO-FALSE-POSITIVE invariant on a scoped `reload <onepack>`, where the loaded set is a
// SUBSET of the world's content, by judging only INTRA-ZONE targets. The validator sees the RAW []content.Pack
// LoadPacks returns (not the content.Load-merged graph), and each pack's zone is already WHOLE: the tree
// loader unions a zone across its files within the pack (packtree mergeZone) before it leaves the source, so
// a pack that defines a zone carries that zone's COMPLETE authored room list. Another pack can only REPLACE a
// zone wholesale (cross-pack merge is whole-zone last-write-wins, loader.go), never add rooms to it — so an
// intra-zone target absent from its zone is provably dangling regardless of what else the live world holds
// (worst case across packs is a false NEGATIVE, never a false positive). A CROSS-zone target may resolve to a
// room in a pack outside this reload's scope, so it is deliberately left to the full merged-graph check (#197
// slice 3, which loads every pack) rather than risk a false rejection here. An empty/whitespace target is
// always a dead exit, judged in any scope.
//
// This intra-zone judgment assumes a room's ref prefix equals its owning zone — the same convention cross-
// zone exit routing (parseRef) already load-bears on, and which lintRoomZonePrefixes (build.go) warns about
// when violated. In a world that trips that lint (a divergent-prefix room authored into another zone in an
// out-of-scope pack), a target could be flagged though a live room exists; such a world is already mis-
// authored (its cross-zone exits misroute and a hot-reload ADD skips it), so it is out of scope here.
func validateRoomExits(loaded []content.Pack) []string {
	// Every room ref present in the loaded set — the resolution target set.
	roomRefs := map[string]bool{}
	for i := range loaded {
		for _, z := range loaded[i].Zones {
			for _, r := range z.Rooms {
				roomRefs[r.Ref] = true
			}
		}
	}
	var problems []string
	for i := range loaded {
		for _, z := range loaded[i].Zones {
			for _, r := range z.Rooms {
				// Deterministic readout: map iteration order is random, so sort the directions.
				dirs := make([]string, 0, len(r.Exits))
				for dir := range r.Exits {
					dirs = append(dirs, dir)
				}
				sort.Strings(dirs)
				for _, dir := range dirs {
					target := r.Exits[dir]
					if strings.TrimSpace(target) == "" {
						problems = append(problems, fmt.Sprintf("room %q: exit %q has an empty target", r.Ref, dir))
						continue
					}
					if roomRefs[target] {
						continue // resolves (raw match, exactly as the runtime routes)
					}
					// An INTRA-zone target (same zone prefix as the owning zone) that is absent is provably
					// dangling: the loaded zone holds that zone's complete room list. A cross-zone target is
					// left to the full-graph check — its zone may be out of this reload's scope.
					if tz, _ := parseRef(ProtoRef(target)); tz == z.Ref {
						problems = append(problems, fmt.Sprintf("room %q: exit %q points to unknown room %q", r.Ref, dir, target))
					}
				}
			}
		}
	}
	return problems
}

// validateChannels reports one problem per definitively-DEAD channel_def in the re-read packs. Unlike the
// attribute checks (a boot-only concern), channels are propagated + hot-swapped by this very reload path
// (contentbus.PublishPack -> reloadChannel -> buildChannelDef), so this is real payload validation of the
// content the builder edited. Channels merge across packs last-write-wins by ref (mirroring content.Load,
// loader.go), so the check runs over the merged set. Faithful to the boot build path — it rejects only a
// channel boot would build STRUCTURALLY DEAD, never one that is merely degraded:
//
//   - an EMPTY ref: the channel registry is keyed by ref and the comms subject is built from it
//     (commbus.ChanSubject) — a ref-less channel can neither be swapped in (reloadChannel keys by ref) nor
//     addressed on the wire;
//   - a SUBJECT-UNSAFE ref: ChanSubject concatenates the ref raw into telos.comms.chan.<ref>, so a ref
//     with whitespace/control bytes, a NATS wildcard (* or >), or an empty dot-delimited token builds a
//     subject NATS rejects on publish (or can't publish to) — the channel can never carry a line. This is
//     the char-safety half of the P8-A8 subject-injection contract ChanSubject's doc defers to here;
//   - NO usable verb word: after the same lower/trim buildChannelDef applies no word survives, so
//     channelForVerb can never resolve the channel — it is unreachable (a channel nobody can speak on);
//   - a format that DROPS the player's message: rendered through the SAME renderChannelFormat boot uses
//     (an empty format defaults to defaultChannelFormat, which carries $t), a non-empty template with no
//     surviving $t silently swallows every line — the channel carries no content.
//
// A merely-degraded channel (an unknown $token that surfaces literally, an over-restrictive access
// predicate) is NOT rejected: only definitively-dead content blocks the publish, preserving the
// no-false-positive invariant.
func validateChannels(loaded []content.Pack) []string {
	var problems []string
	// Merge across packs EXACTLY as content.Load / mergeByKey do: last-write-wins keyed on the RAW ref
	// (loader.go, packtree.go — and buildChannelDef/reloadChannel/ChanSubject all key raw too), order
	// preserved. Keying raw is what keeps this faithful: it can't collapse two whitespace-variant refs that
	// stay distinct live channels and thereby miss (or falsely flag) a dead one. A whitespace-only ref
	// (trims to empty) is separately caught as "missing ref" below.
	merged := map[string]content.ChannelDTO{}
	var order []string
	for i := range loaded {
		for _, ch := range loaded[i].Channels {
			if strings.TrimSpace(ch.Ref) == "" {
				// Empty or whitespace-only: no usable merge key, and an unaddressable subject. Flag each.
				problems = append(problems, fmt.Sprintf("channel %q: missing ref", ch.Name))
				continue
			}
			ref := ch.Ref
			if _, seen := merged[ref]; !seen {
				order = append(order, ref)
			}
			merged[ref] = ch
		}
	}
	// A sentinel that content can't contain (NUL bytes) — its survival through the format proves $t renders.
	const probe = "\x00telos-msg-probe\x00"
	for _, ref := range order {
		ch := merged[ref]
		if reason := channelRefSubjectProblem(ref); reason != "" {
			problems = append(problems, fmt.Sprintf("channel %q: ref builds an invalid comms subject (%s)", ref, reason))
		}
		hasWord := false
		for _, w := range ch.Words {
			if strings.TrimSpace(w) != "" {
				hasWord = true
				break
			}
		}
		if !hasWord {
			problems = append(problems, fmt.Sprintf("channel %q: no usable verb word", ref))
		}
		format := ch.Format
		if format == "" {
			format = defaultChannelFormat
		}
		if !strings.Contains(renderChannelFormat(format, ch.Name, "probe", probe), probe) {
			problems = append(problems, fmt.Sprintf("channel %q: format template drops the message (no $t token)", ref))
		}
	}
	return problems
}

// channelRefSubjectProblem reports why a channel ref would build a malformed or unpublishable NATS comms
// subject (commbus.ChanSubject concatenates it RAW into telos.comms.chan.<ref>), or "" when it is
// subject-safe. This is the char-safety half of the P8-A8 subject-injection contract ChanSubject's doc
// explicitly defers to channel_defs: a ref carrying whitespace/control bytes, a NATS wildcard token
// (* or >), or an empty dot-delimited token yields a subject NATS rejects on publish (or a wildcard the
// world can't publish to). Either way the channel can never carry a line — it is definitively dead — so
// the gate rejects it. Legit refs ([a-z0-9_-], optionally dotted) pass untouched, so there is no
// false-positive risk against good content.
func channelRefSubjectProblem(ref string) string {
	for _, r := range ref {
		switch {
		case r <= ' ' || r == 0x7f:
			return "whitespace or control character"
		case r == '*' || r == '>':
			return "a NATS wildcard character (* or >)"
		}
	}
	for _, tok := range strings.Split(ref, ".") {
		if tok == "" {
			return "an empty dot-delimited subject token"
		}
	}
	return ""
}
