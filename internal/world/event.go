package world

import (
	"context"
	"log/slog"
	"math/rand"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// event.go is the IN-ZONE event bus ([G3], docs/PHASE6-PLAN.md §1.2) — the universal glue the gap
// analysis named the highest-leverage unbuilt mechanism. Content subscribes op-lists to engine-named
// events via `on_event` on resource/affect/ability defs; the engine FIRES those events at fixed points
// and runs the subscribed handlers SYNCHRONOUSLY, single-writer, on the zone goroutine — the same
// consistent view a command or affect tick has.
//
// # Scope boundary (P6-D3)
//
// This bus is IN-ZONE, synchronous, and TRANSIENT. It is NOT the cross-zone scoped + durable
// (JetStream, ordered, idempotent) event bus — that is Phase 10 (docs/WORLD-EVENTS.md). A handler that
// needs a cross-zone consequence enqueues for the (Phase-10) director; nothing here crosses a zone.
//
// # Re-entrancy + the depth budget
//
// A handler runs INSIDE the action that fired it (an OnHit handler that deals damage fires
// OnDamageTaken, whose handler might reflect, …). effectCtx.depth counts how many event-fires deep we
// are; a fresh (command/cast/tick) ctx is depth 0. fireEvent runs handlers at depth+1 and REFUSES to
// fire once depth reaches maxEventDepth — killing any OnHit→damage→OnDamageTaken→… cycle. Ordering
// within an event is DETERMINISTIC: resources (sorted by ref) before affects (active-list order).
// A WIDTH budget (maxEventHandlers) additionally bounds the cascade's TOTAL handler runs (event.go).
//
// # The harm gate still applies
//
// A handler's op-list is ordinary ops: a harmful op (deal_damage / a detrimental apply_affect / a
// cross-player resource write) funnels the SAME guardHarmful (effect_op.go). An event handler is NOT a
// PvP-gate bypass — the gate lives at the op, not at the subscription.
//
// # Named interruptible checkpoints ([G9], P6-D4, docs/PHASE6-PLAN.md §1.3.1)
//
// The swing/cast/movement pipelines fire events at NAMED checkpoints so content (and Phase-7 Lua) can
// react. v1 reactions are DECLARATIVE: a handler may run a granted op-list (an opportunity attack) +
// spend a per-round reaction resource; it may NOT alter the in-flight action (cancel a cast, add to AC
// after the roll) — that RESULT-ALTERING path is the Phase-7 Lua hatch, which only adds handlers at
// these same checkpoints, never pipeline surgery. The checkpoints:
//
//	OnHit / OnDamageTaken  — the swing landed / a target took damage (combat.go, live 6.3a)
//	OnLeaveRoom            — an engaged foe is LEAVING a foe's room (combat.go fireLeaveRoom, live 6.4b)
//	BeforeCastCommit       — an ability is about to commit (ability.go, reserved fire point, live 6.4b)
//	OnKill                 — a target died (death.go, live 6.3b)
//
// # The OnLeaveRoom subject convention (the opportunity attack)
//
// fireLeaveRoom fires OnLeaveRoom about each ENGAGED REACTOR (subject = a foe fighting the leaver), with
// `other` = the LEAVER. The bus gathers handlers from the SUBJECT, so the reactor's OWN subscription
// (a `reactions` resource carrying on_event[OnLeaveRoom]) runs; its ops bind the leaver via `target:
// other` and deal the granted swing through the SAME gated deal_damage. The reactor must be engaged with
// the leaver in the same room (fireLeaveRoom's gather) — and content decides whether to spend a reaction
// (an `if reactions>=1` guard), so a spent budget = no opportunity attack (the second flee gets none).

// eventKind is an engine-named in-zone event. The engine OWNS this closed set (content subscribes to
// it, never defines new kinds); an unknown name in content `on_event` is dropped at parse with a lint
// warning. Phase 6.2 FIRES OnCheck + OnAbilityResolved; the combat/movement/rest points are named here
// and lit as their slices land (6.3/6.4), so a subscription authored against them parses now.
type eventKind string

const (
	evOnCheck           eventKind = "OnCheck"           // a check/save/contested resolved (check.go) — live 6.2
	evOnAbilityResolved eventKind = "OnAbilityResolved" // an ability finished step 8 (ability.go) — live 6.2
	evOnHit             eventKind = "OnHit"             // a swing landed (combat) — live 6.3a
	evOnDamageTaken     eventKind = "OnDamageTaken"     // an entity took damage — live 6.3a
	evOnKill            eventKind = "OnKill"            // an entity died to the subject — live 6.3b
	evOnLeaveRoom       eventKind = "OnLeaveRoom"       // an engaged foe is leaving a room (the OA checkpoint) — live 6.4b
	evBeforeCastCommit  eventKind = "BeforeCastCommit"  // an ability is about to commit (the cast checkpoint) — live 6.4b
	evOnRest            eventKind = "OnRest"            // the subject rested — reserved 6.2b (regen/rest)
	evOnApplyAffect     eventKind = "OnApplyAffect"     // an affect attached — reserved
	evOnAffectTick      eventKind = "OnAffectTick"      // an affect ticked — reserved
	evOnAffectExpire    eventKind = "OnAffectExpire"    // an affect expired — reserved
)

const (
	evOnEnter eventKind = "OnEnter" // an entity entered a room (the movement hook) — live 7.8
	// evToHit is the to-hit REACTION checkpoint (7.9, P7-D8): fired about the DEFENDER right BEFORE a
	// swing's to-hit roll (combat.go), carrying the attacker as `other`. It is a RESULT-ALTERING
	// reaction checkpoint (a Lua Shield hook raises the defender's AC for the triggering swing via
	// rx:modify("ac", +delta)) — the engine consults the reaction's recorded "ac" delta at the seam.
	// Unlike the other lit kinds it is fired through the reaction path (luareact.go), not a plain
	// declarative fireEvent, because only a Lua body carrying an `rx` can alter the to-hit result.
	evToHit eventKind = "ToHit"
)

// knownEventKinds is the closed set the parser validates content `on_event` keys against. It is the
// ENGINE-OWNED set: a content `on_event` key that is neither one of these NOR a namespaced custom
// kind (the pack: lane below) is dropped at parse with a lint warning — content can't invent a BARE
// engine event (that would silently fail to fire forever).
var knownEventKinds = map[eventKind]bool{
	evOnCheck: true, evOnAbilityResolved: true, evOnHit: true, evOnDamageTaken: true,
	evOnKill: true, evOnLeaveRoom: true, evBeforeCastCommit: true, evOnRest: true,
	evOnApplyAffect: true, evOnAffectTick: true, evOnAffectExpire: true, evOnEnter: true,
	evToHit: true,
}

// customEventSep is the namespace separator that makes a custom (content-defined) event kind
// SYNTACTICALLY distinguishable from an engine kind (the bare OnX consts above). A custom kind is
// "<pack>:Name" (e.g. "sailing:OnShipDock"); the prefix namespaces it BY PACK so two packs' same-
// named events ("a:OnDock" vs "b:OnDock") never collide. A bare name with no separator is an ENGINE
// kind and MUST be in knownEventKinds — so an unknown bare kind is still rejected/lint-caught (the
// pack: lane does NOT open a hole where any string becomes a valid engine event).
const customEventSep = ":"

// isCustomEventKind reports whether `kind` is a content-namespaced custom event (the pack: lane,
// PHASE7-PLAN.md §5 7.8a) rather than a bare engine kind. The separator is the discriminator: an
// engine kind is a bare OnX; a custom kind carries the "<pack>:" namespace. A custom kind is NOT in
// knownEventKinds (the engine never heard of it) — it dispatches to per-entity on(name,fn) trigger
// handlers, under the SAME depth/width budget + harm gate as an engine event (no privileged status).
func isCustomEventKind(kind eventKind) bool {
	return strings.Contains(string(kind), customEventSep)
}

// isFireableEventKind reports whether `kind` is a name content may fire/subscribe: a known ENGINE
// kind OR a namespaced CUSTOM kind. A bare name not in knownEventKinds is neither (an unknown engine
// kind) and is rejected — the lane stays namespaced.
func isFireableEventKind(kind eventKind) bool {
	return knownEventKinds[kind] || isCustomEventKind(kind)
}

// maxEventDepth bounds how many event-fires may NEST before the bus refuses to fire further (the
// re-entrancy guard). 8 is far past any sane proc chain (hit→rage→…); the cap exists so a content
// loop (A fires B fires A …) can never spin the single-writer zone goroutine.
const maxEventDepth = 8

// maxEventHandlers bounds the TOTAL handler executions in one event cascade (rooted at one action) —
// the WIDTH guard the depth cap alone can't provide. Depth bounds the tree's HEIGHT; a wide
// subscription set (N handlers each firing M events) fans out multiplicatively within the depth
// envelope, so without a total-work bound a pathological-but-non-recursive content graph could pin the
// zone goroutine and starve the heartbeat. 256 is far past any legitimate proc cascade; exhausting it
// truncates the cascade with a warning (the triggering action still completes).
const maxEventHandlers = 256

// maxEventCascadeDepth is the ZONE-LEVEL recursion backstop (Zone.eventCascadeDepth) — the can't-
// forget guard that bounds NESTED fireEvent calls REGARDLESS of whether each fire threaded its
// parent ctx. The per-ctx depth guard (maxEventDepth) only holds when every fire site remembers to
// thread its parent; a site that passes nil parent resets depth to 0, and a self-perpetuating cycle
// through such a site would recurse the Go stack unbounded (the 7.8 affect-lifecycle CRITICAL). This
// cap trips on the COUNT OF LIVE fireEvent FRAMES on the goroutine stack, so it bounds the Go
// recursion even when the logical-depth guard is defeated. It is set ABOVE maxEventDepth (so a
// legitimately deep, properly-threaded cascade — at most maxEventDepth logical levels — never trips
// it) and a small multiple over (so a short chain of distinct ROOT cascades, each itself depth-
// bounded, still completes), but FAR below the goroutine-stack-exhaustion frontier. 32 = 4×
// maxEventDepth: generous for any real proc graph, decisive against an unbounded loop.
const maxEventCascadeDepth = 4 * maxEventDepth

// eventHandler is one subscribed handler plus where it came from (for logging/lint). It is EITHER
// an op-list (ops) OR a Lua body (luaSrc) — a content event handler is one or the other (7.4g).
type eventHandler struct {
	ops    []effectOp
	luaSrc string // a Lua-BODY handler (7.4g); empty => an op-list handler (ops)
	origin string // "resource:rage" | "affect:bloodlust" — diagnostic + the compile-cache key tail
}

// gatherEventHandlers collects the op-lists on `subject` subscribed to `kind`, from the entity's
// content: the resources it HAS (a positive max or a stored current) and its ACTIVE affects. (Ability/
// item subscriptions await the Skilled/equipment components — a later slice.) Resources are gathered
// before affects so ordering is stable. O(resource defs + active affects) — local to the firing
// entity, never a global scan. Zone-goroutine-owned reads only.
func gatherEventHandlers(e *Entity, kind eventKind) []eventHandler {
	if e == nil || e.zone == nil {
		return nil
	}
	var out []eventHandler
	// Resource handlers, gathered in a DETERMINISTIC order (sorted by ref): Go map iteration is
	// randomized, and a content graph with two order-dependent handlers on the same event must run them
	// reproducibly (and the seeded rng threaded into handler ctxs would otherwise be partly defeated).
	table := e.zone.resourceDefs().table()
	refs := make([]string, 0, len(table))
	for ref, def := range table {
		if def.onEvent != nil && len(def.onEvent[kind]) > 0 && entityHasResource(e, ref) {
			refs = append(refs, ref)
		}
	}
	// Also gather resources with a Lua handler for this kind (7.4g).
	luaRefs := make([]string, 0, len(table))
	for ref, def := range table {
		if def.onEventLua != nil && def.onEventLua[kind] != "" && entityHasResource(e, ref) {
			luaRefs = append(luaRefs, ref)
		}
	}
	sort.Strings(refs)
	sort.Strings(luaRefs)
	for _, ref := range refs {
		out = append(out, eventHandler{ops: table[ref].onEvent[kind], origin: "resource:" + ref})
	}
	for _, ref := range luaRefs {
		out = append(out, eventHandler{luaSrc: table[ref].onEventLua[kind], origin: "resource:" + ref + ":lua"})
	}
	if a, ok := Get[*Affected](e); ok {
		for _, inst := range a.list {
			if inst.def == nil {
				continue
			}
			if inst.def.onEvent != nil {
				if ops := inst.def.onEvent[kind]; len(ops) > 0 {
					out = append(out, eventHandler{ops: ops, origin: "affect:" + inst.def.ref})
				}
			}
			if inst.def.onEventLua != nil {
				if src := inst.def.onEventLua[kind]; src != "" {
					out = append(out, eventHandler{luaSrc: src, origin: "affect:" + inst.def.ref + ":lua"})
				}
			}
		}
	}
	return out
}

// entityHasResource reports whether e "has" resource ref — a stored current OR a positive derived max.
// This is how a content resource (rage/combo) is owned by an entity without a per-entity grant list
// (which arrives with progression, Phase 11): if your content gave you a rage pool (max > 0) you build
// rage; a contentless entity has none.
func entityHasResource(e *Entity, ref string) bool {
	if e == nil || e.living == nil {
		return false
	}
	if _, ok := e.living.resCur[ref]; ok {
		return true
	}
	return resourceMax(e, ref) > 0
}

// fireEvent dispatches in-zone event `kind` about `subject` (the entity the event is ABOUT: the hitter
// for OnHit, the checker for OnCheck), with `other` the counterpart (the victim, the contested foe;
// nil if none) and `mag` an event magnitude (e.g. damage dealt; 1 where N/A). It gathers the subject's
// subscribed handlers and runs each on a fresh, depth-incremented ctx — SYNCHRONOUSLY on the zone
// goroutine. Refuses to fire past maxEventDepth (the re-entrancy guard). A handler op's `target: other`
// selector (runOps) binds the counterpart; harmful ops still funnel guardHarmful. Single-writer.
func (z *Zone) fireEvent(parent *effectCtx, kind eventKind, subject, other *Entity, mag float64) {
	if z == nil || subject == nil {
		return
	}
	// ZONE-LEVEL RECURSION BACKSTOP (the can't-forget guard, maxEventCascadeDepth): bound the COUNT
	// of live fireEvent frames on the goroutine stack, independent of whether each fire threaded its
	// parent ctx. This is what stops an unbounded Go-stack recursion when a fire site forgets to
	// thread `parent` (the 7.8 affect-lifecycle CRITICAL: a depth-0-resetting cycle the per-ctx
	// maxEventDepth never catches). The engine enforces the bound; it does not trust every fire site
	// to thread correctly. Single-writer: the counter is plain zone-owned int (no lock).
	if z.eventCascadeDepth >= maxEventCascadeDepth {
		z.log.Warn("event cascade depth backstop tripped; cascade truncated (a fire site likely did not thread its parent ctx)",
			"event", string(kind), "subject", subject.short, "cascade_depth", z.eventCascadeDepth)
		return
	}
	z.eventCascadeDepth++
	defer func() { z.eventCascadeDepth-- }()

	depth := 0
	var rng *rand.Rand
	var budget *int
	if parent != nil {
		depth = parent.depth
		rng = parent.rng            // thread the deterministic rng (tests) into handler ctxs
		budget = parent.eventBudget // share the cascade's total-work budget
	}
	if depth >= maxEventDepth {
		z.log.Warn("event depth budget exhausted; handlers dropped",
			"event", string(kind), "subject", subject.short, "depth", depth)
		return
	}
	if budget == nil {
		// Root of a new cascade: allocate the shared total-work budget the whole tree decrements.
		b := maxEventHandlers
		budget = &b
	}
	// A CUSTOM (namespaced) kind has no engine handler source (resource/affect defs subscribe to
	// engine kinds only) — it dispatches to the subject's on(name,fn) TRIGGER handler. Route it
	// through the SAME budget-threading we just computed (depth + the shared width budget) so a
	// content custom event has NO privileged status: a custom handler that re-fires is bounded by
	// maxEventDepth, the cascade's TOTAL handler runs by maxEventHandlers, and any harm op in it
	// funnels guardHarmful exactly like an engine-event handler. (PHASE7-PLAN.md §5 7.8a.)
	if isCustomEventKind(kind) {
		z.fireCustomEvent(kind, subject, other, mag, rng, depth, budget)
		return
	}
	handlers := gatherEventHandlers(subject, kind)
	for _, h := range handlers {
		if *budget <= 0 {
			z.log.Warn("event handler budget exhausted; cascade truncated",
				"event", string(kind), "subject", subject.short)
			return
		}
		*budget--
		if z.log.Enabled(context.Background(), slog.LevelDebug) {
			z.log.Debug("event fire", "event", string(kind), "subject", subject.short,
				"origin", h.origin, "depth", depth+1)
		}
		c := &effectCtx{
			z: z, actor: subject, source: subject, target: subject, other: other,
			mag: mag, disp: dispNeutral, rng: rng, depth: depth + 1, eventBudget: budget,
		}
		if h.luaSrc != "" {
			// A Lua-BODY handler (7.4g): run it via invokeFromCtx threading THIS cascade ctx `c` —
			// the SAME depth+budget pointer the op-list handlers share, so a Lua handler decrements
			// the same width budget and cannot re-fire to escape it (invariant 1). `self` = the
			// subject, `ev` carries the counterpart + magnitude. Fail-closed (pcall-isolated).
			z.runLuaEventHandler(c, h, subject, other, mag)
			continue
		}
		runOps(c, h.ops)
	}
}

// runLuaEventHandler runs a Lua bus handler (7.4g) under the firing cascade ctx `c`. The handler
// reads `self` (the subject), `ev.other` (the counterpart), and `ev.mag` (the event magnitude).
// Compile-once-per-zone (keyed by origin + kind) + fail-closed. It threads c (depth + the shared
// eventBudget pointer) so the handler runs under the same width/depth cap as an op-list handler.
func (z *Zone) runLuaEventHandler(c *effectCtx, h eventHandler, subject, other *Entity, mag float64) {
	if z.lua == nil {
		return
	}
	rt := z.lua
	ch := rt.chunkFor("event:"+h.origin, h.luaSrc)
	if ch == nil {
		return
	}
	ev := rt.L.NewTable()
	if other != nil {
		ev.RawSetString("other", rt.newHandle(other))
	}
	ev.RawSetString("mag", lua.LNumber(mag))
	_ = rt.invokeFromCtx(ch, c, subject, map[string]lua.LValue{"ev": ev})
}

// fireCustomEvent dispatches a CONTENT-NAMESPACED custom event (the pack: lane, PHASE7-PLAN.md §5
// 7.8a) to the subject's on(name,fn) TRIGGER handler. It is fired from fireEvent (NOT a parallel
// dispatcher) so it threads the SAME cascade state: `depth` (the re-entrancy guard) and the shared
// `budget` pointer (the width guard) the rest of the bus uses. A custom event has NO privileged
// status — it decrements the same width budget, refuses to nest past maxEventDepth (fireEvent
// checked `depth` before calling us), and any harm op in the handler funnels guardHarmful through
// the trigger's threaded ctx (invariant 1) exactly like an engine-event handler.
//
// The handler receives `ev.mag`, `ev.data` (the firer's arbitrary plain-data table — mud.fire's
// third arg), and `ev.other` IF the fire supplied a counterpart (mud.fire passes none, so content
// threads any counterpart through `ev.data`; `other` is here for a future engine-internal custom
// fire). A subject with no on(name,fn) handler for this kind is a clean no-op (the firer doesn't
// know who, if anyone, subscribes — the loose coupling the event bus exists for). Zone goroutine.
func (z *Zone) fireCustomEvent(kind eventKind, subject, other *Entity, mag float64, rng *rand.Rand, depth int, budget *int) {
	if z == nil || z.lua == nil || subject == nil {
		return
	}
	rt := z.lua
	es := rt.ensureEntityScript(subject)
	if es == nil {
		return // the subject carries no trigger handlers (no *Scripted source / no state)
	}
	h, ok := es.handlers.RawGetString(string(kind)).(*lua.LFunction)
	if !ok {
		return // no handler subscribed to this custom kind on this subject
	}
	if *budget <= 0 {
		z.log.Warn("event handler budget exhausted; custom cascade truncated",
			"event", string(kind), "subject", subject.short)
		return
	}
	*budget--
	if z.log.Enabled(context.Background(), slog.LevelDebug) {
		z.log.Debug("custom event fire", "event", string(kind), "subject", subject.short,
			"origin", "trigger:#"+ridStr(subject.rid), "depth", depth+1)
	}
	// The cascade ctx the handler runs under: the SUBJECT acts (a harm op in the handler is
	// attributed to it + gated), the counterpart is `other`, and the depth+budget are threaded so a
	// re-fire from inside the handler stays bounded by the same caps. This is the identical ctx shape
	// an engine-event Lua handler runs under (runLuaEventHandler above).
	c := &effectCtx{
		z: z, actor: subject, source: subject, target: subject, other: other,
		mag: mag, disp: dispNeutral, rng: rng, depth: depth + 1, eventBudget: budget,
	}
	ev := rt.evTable(other, "")
	ev.RawSetString("mag", lua.LNumber(mag))
	if rt.fireData != nil {
		// The firer's plain-data table (mud.fire's 3rd arg), threaded via the runtime (NOT the engine
		// fireEvent signature, which is shared by every engine kind and must not carry a Lua value).
		ev.RawSetString("data", rt.fireData)
	}
	binds := map[string]lua.LValue{
		"self":  rt.newHandle(subject),
		"state": es.state,
		"ev":    ev,
	}
	rt.fireTriggerCall(es, h, c, binds, ev)
}
