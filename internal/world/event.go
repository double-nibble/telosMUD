package world

import (
	"context"
	"log/slog"
	"math/rand"
	"sort"
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

// knownEventKinds is the closed set the parser validates content `on_event` keys against.
var knownEventKinds = map[eventKind]bool{
	evOnCheck: true, evOnAbilityResolved: true, evOnHit: true, evOnDamageTaken: true,
	evOnKill: true, evOnLeaveRoom: true, evBeforeCastCommit: true, evOnRest: true,
	evOnApplyAffect: true, evOnAffectTick: true, evOnAffectExpire: true,
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

// eventHandler is one subscribed op-list plus where it came from (for logging/lint).
type eventHandler struct {
	ops    []effectOp
	origin string // "resource:rage" | "affect:bloodlust" — diagnostic only
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
	sort.Strings(refs)
	for _, ref := range refs {
		out = append(out, eventHandler{ops: table[ref].onEvent[kind], origin: "resource:" + ref})
	}
	if a, ok := Get[*Affected](e); ok {
		for _, inst := range a.list {
			if inst.def == nil || inst.def.onEvent == nil {
				continue
			}
			if ops := inst.def.onEvent[kind]; len(ops) > 0 {
				out = append(out, eventHandler{ops: ops, origin: "affect:" + inst.def.ref})
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
		runOps(c, h.ops)
	}
}
