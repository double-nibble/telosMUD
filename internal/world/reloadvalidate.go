package world

import (
	"fmt"
	"sort"
	"strconv"
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
// so an edit that adds a room with a broken exit is caught before it propagates. validateResets (#197 slice
// 2b) checks zone reset references (unknown op / undefined target room / undefined prototype) — a pack-HEALTH
// check (resets don't hot-swap; they run at boot/repop), catching a broken edit before broadcast.
// validateProtoRefs (#197 slice 2c) rejects a prototype ref collision — two rooms/items/mobs sharing a ref
// silently collapse to one in the shared cache. Remaining #197 work: a full-graph dry-run for cross-pack
// cycles a scoped reload misses. The framework — collect problems -> gate the publish -> report — is what each slots into.
//
// Validation is PURE over the parsed DTOs (it reuses parseAttributeBase + lintAttributeCycles and builds a
// THROWAWAY attributeDef map — never the shard's live registries), so it is safe on the off-zone-goroutine
// republish path and never touches live state. Only definitively-broken content blocks; a transient re-read
// blip stays best-effort (handled in republish), preserving reload's optional posture.

// reloadOutcome is the result of a republish attempt, shaped so the builder readout can distinguish the
// three cases: a clean propagation, a validation REJECTION (nothing published — the content is broken), and
// a best-effort INFRA failure (a re-read/publish blip; the applier's per-ref fail-safe is the backstop).
type reloadOutcome struct {
	published  int      // invalidations put on the wire (0 when rejected or check-only)
	rejected   []string // validation problems; NON-EMPTY => nothing was published (a hard content gate)
	failed     bool     // an infrastructure failure (re-read/publish error) — logged, best-effort
	checkOnly  bool     // true => a `reload --check` dry run that validated OK and deliberately published nothing
	sharedDefs []string // #56: shared-def kinds present in the reloaded packs that are NOT hot-applied (rolling-reboot reminder)
}

// sharedDefKinds returns the sorted, de-duplicated labels of the pack-GLOBAL "shared def" kinds present in
// the reloaded packs that have NO hot-reload loop — the contentbus only emits room/item/mob/zone/channel
// invalidations (contentbus/publish.go), so editing one of these takes effect only after a ROLLING REBOOT of
// the world shards. This is the settled design (#56/#53: a live z.defs swap is optional, not required). The
// reload readout uses this to REMIND the operator, so a shared-def edit is never silently un-applied.
//
// SOURCE OF TRUTH: this list must mirror the pack-global registrations in defineGlobals (build.go) MINUS the
// ones with a hot-reload loop (channels, via KindChannel) and MINUS the ones the world shard doesn't hold
// (regions / spawn schedules / chargens are consumed by directors / telos-account — a WORLD reboot wouldn't
// apply to them). When a new pack-global def kind is added to defineGlobals, add it here too.
func sharedDefKinds(loaded []content.Pack) []string {
	seen := map[string]bool{}
	for i := range loaded {
		pk := &loaded[i]
		mark := func(label string, present bool) {
			if present {
				seen[label] = true
			}
		}
		mark("attributes", len(pk.Attributes) > 0)
		mark("resources", len(pk.Resources) > 0)
		mark("damage types", len(pk.DamageTypes) > 0)
		mark("affects", len(pk.Affects) > 0)
		mark("abilities", len(pk.Abilities) > 0)
		mark("combat profiles", len(pk.CombatProfiles) > 0 || pk.DefaultCombat != "")
		mark("progression tracks", len(pk.Tracks) > 0)
		mark("bundles", len(pk.Bundles) > 0)
		mark("rarity tiers", len(pk.RarityTiers) > 0)
		mark("affixes", len(pk.Affixes) > 0)
		mark("loot tables", len(pk.LootTables) > 0)
		mark("recipes", len(pk.Recipes) > 0)
		mark("wear slots", len(pk.WearSlots) > 0)
		mark("trust tiers", len(pk.TrustTiers) > 0)
		mark("custom commands", len(pk.Commands) > 0)
		mark("display templates", len(pk.DisplayDefs) > 0)
		mark("ruleset formulas", len(pk.Formulas) > 0)
		mark("pvp policy", pk.PvpLua != "")
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
	// Reset-reference sanity (#197 slice 2b) — a pack-HEALTH check (resets do NOT hot-swap; they run at
	// boot/repop), catching a bad edit before it is broadcast rather than at the next reset.
	problems = append(problems, validateResets(loaded)...)
	// Prototype ref sanity (#197 slice 2c): a collision silently collapses content in the shared cache.
	problems = append(problems, validateProtoRefs(loaded)...)
	// Reserved core: namespace (#212): a real pack shipping a core-namespace world-ref would, on
	// broadcast, drive a KindZone reconcile against the embedded bootstrap lobby (which reshapes/tears
	// down rooms) on every shard. The boot lint only WARNS; here — the broadcast gate — it is a hard
	// REJECT so such a ref can never enter a fleet reload. (Defense-in-depth with the reconcile guard
	// in reload.go, which fail-safes if one slips through.)
	for _, v := range content.LintReservedCoreRefs(loaded) {
		problems = append(problems, fmt.Sprintf("pack %q ships %s %q under the reserved core: namespace (would clobber the embedded bootstrap pack)", v.Pack, v.Kind, v.Ref))
	}
	// Ref charset (#66, extended #234): an identity token (ref/verb/surface/tier-name) or an exit-direction
	// key with a character outside ITS safe charset can break a GMCP key, a comms subject, or the targeting
	// tokenizer (all of which assume tokens are metacharacter-free). Directions get a colon-excluding charset.
	// The boot lint only WARNS; here — the broadcast gate — it is a hard REJECT so a bad token never reloads.
	for _, v := range content.LintRefCharset(loaded) {
		problems = append(problems, fmt.Sprintf("pack %q %s %q has characters outside its safe charset %s (would break GMCP keys / comms subjects / the tokenizer)", v.Pack, v.Field, v.Value, v.Charset))
	}
	// Trust ladder (#111): a baseline tier granting a capability elevates EVERY un-elevated account on the next
	// login, fleet-wide; a duplicate/nameless rung leaves the promote ceiling's rank ordering ambiguous. The
	// boot lint only warns (the engine must boot on imperfect content); here — the broadcast gate — the
	// Reject-severity findings are a hard REJECT so such a ladder can never enter a fleet reload. Warn-severity
	// findings (un-grantable flags, a non-nested ladder) are authoring noise and do not block a reload.
	for _, v := range content.LintTrustLadder(loaded) {
		if v.Severity != content.TrustLadderReject {
			continue
		}
		where := "pack " + strconv.Quote(v.Pack)
		if v.Tier != "" {
			where += " tier " + strconv.Quote(v.Tier)
		}
		problems = append(problems, fmt.Sprintf("%s trust ladder: %s", where, v.Detail))
	}
	return problems
}

// validateProtoRefs reports a problem per prototype (room/item/mob) that would be SILENTLY DROPPED from the
// shared per-shard proto cache: an empty ref, or a ref that COLLIDES with another prototype's. defineContent
// (build.go) registers every loaded zone's rooms, items and mobs into ONE cache via protoCache.define, which
// does `next[ref] = p` — a last-write-wins overwrite. So two prototypes sharing a ref (a room and an item, a
// mob in another zone, a copy-paste that forgot to rename) silently collapse to one at boot with NO error;
// the dropped content just never spawns. This gate rejects that before it is broadcast.
//
// Faithful with no false positives. It first mirrors content.Load's cross-pack ZONE dedup (whole-zone
// last-write-wins by ref — loader.go): the validator sees the raw []content.Pack, so on a bare `reload`
// two packs that legitimately OVERRIDE the same zone both appear, and their (identical) rooms would look
// like collisions; deduping zones by ref first collapses the override to the single zone the cache actually
// builds, exactly as boot does. After dedup, each zone ref appears once (rooms/items/mobs already unioned
// within a pack by packtree), so a surviving duplicate ref is a genuine cross-zone / cross-kind collision.
func validateProtoRefs(loaded []content.Pack) []string {
	// Cross-pack zone dedup: last-write-wins by zone ref, order preserved — the set defineContent builds.
	zonesByRef := map[string]content.ZoneDTO{}
	var order []string
	for i := range loaded {
		for _, z := range loaded[i].Zones {
			if _, seen := zonesByRef[z.Ref]; !seen {
				order = append(order, z.Ref)
			}
			zonesByRef[z.Ref] = z
		}
	}
	var problems []string
	// owner records the first zone that claimed a ref, so a collision names both sides.
	owner := map[string]string{}
	check := func(zoneRef, kind, ref string) {
		if strings.TrimSpace(ref) == "" {
			problems = append(problems, fmt.Sprintf("zone %q: %s prototype with empty ref", zoneRef, kind))
			return
		}
		if prev, ok := owner[ref]; ok {
			problems = append(problems, fmt.Sprintf("prototype ref %q is defined more than once (zones %q and %q) — the later one silently overwrites the earlier in the shared cache", ref, prev, zoneRef))
			return
		}
		owner[ref] = zoneRef
	}
	for _, zref := range order {
		z := zonesByRef[zref]
		for _, r := range z.Rooms {
			check(zref, "room", r.Ref)
		}
		for _, p := range z.Items {
			check(zref, "item", p.Ref)
		}
		for _, p := range z.Mobs {
			check(zref, "mob", p.Ref)
		}
	}
	return problems
}

// validateResets reports one problem per zone reset that would spawn NOTHING — a reset applyReset (reset.go)
// silently logs-and-skips: an unknown op, a target room that is not a room of the reset's own zone, or a
// prototype that is not defined. Resets do NOT hot-swap on the reload path (PublishPack propagates room/
// item/mob/channel invalidations, never resets — they take effect at boot and on the repop timer), so unlike
// the channel/room checks this is a pack-HEALTH check (catch a broken edit before broadcasting it), in the
// same spirit as the attribute checks. It is faithful to applyReset's resolution rules and holds the no-
// false-positive invariant:
//
//   - OP: applyReset understands only {spawn_item, spawn_mob, ""}; any other op is a dead reset (it warns
//     "op not understood" and spawns nothing). Flagged in any scope — the op vocabulary is engine-fixed.
//   - ROOM: applyReset resolves the target via z.rooms[ref] — the reset's OWN zone's rooms ONLY (a reset
//     never places into another zone). So a Room absent from the owning zone's authored room set is provably
//     dangling in ANY scope (the owning zone is always loaded whole here), regardless of ref prefix — a
//     cross-zone-looking Room can never resolve at runtime. An empty Room is dead.
//   - PROTO: applyReset resolves via z.spawn, which reads the SHARED per-shard proto cache. That cache holds
//     every loaded zone's ROOM, item AND mob prototypes (defineContent registers all three — build.go), so a
//     reset naming a room ref as its proto is degenerate but still spawns SOMETHING; the resolvable set here
//     therefore includes rooms too, to avoid a false positive. An intra-zone prototype (its ref prefix == the
//     reset's zone) absent from the loaded set is provably undefined; a CROSS-zone prototype may live in a
//     pack outside a scoped reload's scope, so it is deferred (never falsely rejected). An empty Proto is dead.
//
// The `into` container target is a RUNTIME instance lookup (intoTargetInRoom finds a live container/mob in
// the room, dependent on earlier resets' spawn order), so it cannot be judged statically and is deliberately
// not checked — no false positive.
func validateResets(loaded []content.Pack) []string {
	// The shared proto cache z.spawn reads: every loaded zone's ROOM, item AND mob prototype refs
	// (defineContent registers all three into one cache — build.go). Rooms are included so a reset naming a
	// room ref as its proto — degenerate, but z.spawn resolves it and applyReset does spawn it — is not a
	// false positive.
	protoRefs := map[string]bool{}
	for i := range loaded {
		for _, z := range loaded[i].Zones {
			for _, r := range z.Rooms {
				protoRefs[r.Ref] = true
			}
			for _, p := range z.Items {
				protoRefs[p.Ref] = true
			}
			for _, p := range z.Mobs {
				protoRefs[p.Ref] = true
			}
		}
	}
	var problems []string
	for i := range loaded {
		for _, z := range loaded[i].Zones {
			// The reset's target room resolves against THIS zone's rooms only (z.rooms).
			zoneRooms := make(map[string]bool, len(z.Rooms))
			for _, r := range z.Rooms {
				zoneRooms[r.Ref] = true
			}
			for ri := range z.Resets {
				r := &z.Resets[ri]
				switch r.Op {
				case "spawn_item", "spawn_mob", "":
					// understood — check its references below.
				default:
					problems = append(problems, fmt.Sprintf("zone %q reset: unknown op %q (understood: spawn_item, spawn_mob)", z.Ref, r.Op))
					continue // a dead reset; its refs are moot
				}
				if strings.TrimSpace(r.Room) == "" {
					problems = append(problems, fmt.Sprintf("zone %q reset (proto %q): empty target room", z.Ref, r.Proto))
				} else if !zoneRooms[r.Room] {
					problems = append(problems, fmt.Sprintf("zone %q reset: target room %q is not a room of this zone", z.Ref, r.Room))
				}
				if strings.TrimSpace(r.Proto) == "" {
					problems = append(problems, fmt.Sprintf("zone %q reset (room %q): empty prototype", z.Ref, r.Room))
				} else if !protoRefs[r.Proto] {
					// Undefined only DEFINITIVELY when the proto belongs to a loaded zone: an intra-zone ref
					// (prefix == this zone) is provably undefined; a cross-zone ref may be out of scope.
					if pz, _ := parseRef(ProtoRef(r.Proto)); pz == z.Ref {
						problems = append(problems, fmt.Sprintf("zone %q reset: prototype %q is not defined", z.Ref, r.Proto))
					}
				}
			}
		}
	}
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
// false-positive risk against good content. NOTE (#66): the ref-charset lint now runs in this same gate and
// hard-rejects '.', so a DOTTED channel ref is caught upstream by that stricter, uniform check — this
// function's dot-tolerance is now effectively moot but kept as the subject-specific belt-and-suspenders
// (whitespace/control/wildcard/empty-token remain its own to catch).
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
