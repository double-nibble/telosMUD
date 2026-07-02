package world

import (
	"context"
	"sort"
	"strings"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// commsstate.go is the SOURCE-WORLD half of Phase-8 slice 8.6: the per-player receiver-side comms
// preferences (channel toggles, ignore list, AFK) and the mechanism by which the world communicates the
// player's EFFECTIVE {enabled ∩ hearable} hear-set + ignore list to the gate (docs/PHASE8-PLAN.md 8.6,
// P8-D7, P8-A6; the 8.3 carried-forward HEAR-filter obligation).
//
// # Why the world computes the hear-set (the central design problem)
//
// A player sees a channel line only if they ENABLED it (their comms state) AND CAN HEAR it (the channel
// access predicate against THEIR live *Entity). The hear-access needs the entity + the channel_defs —
// both WORLD-side; the toggle state is the player's comms state — WORLD-side too; the gate owns only the
// socket and has NO content. So the WORLD computes the effective {enabled ∩ hearable} ref set and
// publishes it (with the ignore list) to the player's personal config subject (commbus.ConfigSubject).
// The gate subscribes its OWN config subject (concrete, never a wildcard — no cross-player config leak),
// re-subscribes exactly those concrete channel subjects (dropping the chan.* wildcard — the receiver
// HEAR-filter), and caches the ignore list for the single receiver-side funnel (P8-A6). This closes the
// content guardrail: a RESTRICTED-access channel now reaches only sockets whose world put it in their
// hear-set.
//
// # Handoff-transparency
//
// The gate's hear-set + ignore list live on the CONNECTION (commsClient), so a re-dial leaves them
// untouched (like the tell subscription). The comms STATE itself rides the handoff snapshot
// (handoff.go CommsState) so a cross-shard walk does not reset toggles; the destination re-publishes the
// effective config on arrival (it has the entity + ITS channel_defs, which may differ), keeping the
// hear-set correct for the destination's content. A LOGIN, a HANDOFF arrival, and a TOGGLE re-publish.
// A mid-session hear-ACCESS change (an affect apply/expire crossing a channel's `min_attr` floor or a
// require-flag) ALSO re-publishes, via republishCommsOnAccessChange hooked at the affect apply/expire sites
// (affect_runtime.go), so a player who drops below a restricted channel's threshold stops hearing it at
// once rather than at their next toggle/handoff/relog. That hook is cheap-guarded (a no-op unless some
// channel actually gates hearing), so a world with no access-gated channels pays nothing.

// commsState is the in-memory form of a player's CommsStateJSON (character.go), owned by the zone
// goroutine that owns the session (single-writer, like tellCursor) — no locks. nil on a session until
// first touched (loadCommsState / a toggle lazily creates it); a nil state is all-defaults.
type commsState struct {
	// chanOverride[ref] forces a channel on (true) / off (false) vs its default_on; an absent ref uses
	// the channel's default_on. Storing only the override (not the resolved set) keeps a default_on
	// hot-reload meaningful for untouched channels (the attribute-base-override precedent).
	chanOverride map[string]bool
	// ignore is the set of author ids this player ignores (the receiver-side funnel input, P8-A6).
	ignore map[string]struct{}
	afk    bool
	afkMsg string
}

func newCommsState() *commsState {
	return &commsState{chanOverride: map[string]bool{}, ignore: map[string]struct{}{}}
}

// commsOf returns the session's comms state, lazily creating an empty (all-default) one. Zone goroutine.
func commsOf(s *session) *commsState {
	if s.comms == nil {
		s.comms = newCommsState()
	}
	return s.comms
}

// --- dump / load (the StateJSON.Comms subtree, P8-D7) ----------------------------------------

// dumpCommsState renders the session's comms state into its durable form, or nil when it is all-default
// (no overrides, no ignores, not AFK) so a never-toggled player writes no subtree (the omitempty
// precedent). Size-guarded (commsChanMaxRefs / commsIgnoreMaxIDs). Runs on the zone goroutine; copies
// into fresh collections so the saver never aliases live session state.
func dumpCommsState(s *session) *CommsStateJSON {
	if s == nil || s.comms == nil {
		return nil
	}
	cs := s.comms
	if len(cs.chanOverride) == 0 && len(cs.ignore) == 0 && !cs.afk && cs.afkMsg == "" {
		return nil
	}
	var chans map[string]bool
	if len(cs.chanOverride) > 0 {
		chans = make(map[string]bool, len(cs.chanOverride))
		for ref, on := range cs.chanOverride {
			if len(chans) >= commsChanMaxRefs {
				break
			}
			chans[ref] = on
		}
	}
	var ignore []string
	if len(cs.ignore) > 0 {
		ignore = make([]string, 0, len(cs.ignore))
		for id := range cs.ignore {
			ignore = append(ignore, id)
		}
		sort.Strings(ignore) // a stable, deterministic on-disk order
		if len(ignore) > commsIgnoreMaxIDs {
			ignore = ignore[:commsIgnoreMaxIDs]
		}
	}
	return &CommsStateJSON{Channels: chans, Ignore: ignore, AFK: cs.afk, AFKMsg: cs.afkMsg}
}

// loadCommsState installs a persisted comms-state subtree onto the session (Phase 8.6). A nil/all-empty
// subtree (a pre-8.6 save or a never-toggled player) installs the all-default state (lazily created on
// first touch). Runs on the zone goroutine.
func loadCommsState(s *session, c *CommsStateJSON) {
	if s == nil || c == nil {
		return
	}
	cs := newCommsState()
	for ref, on := range c.Channels {
		cs.chanOverride[ref] = on
	}
	for _, id := range c.Ignore {
		if id != "" {
			cs.ignore[id] = struct{}{}
		}
	}
	cs.afk = c.AFK
	cs.afkMsg = c.AFKMsg
	s.comms = cs
}

// --- the effective hear-set + the config publish (the receiver HEAR-filter) -------------------

// channelEnabled reports whether the player has this channel ENABLED: the override if present, else the
// channel's default_on. Zone goroutine.
func (cs *commsState) channelEnabled(def *channelDef) bool {
	if cs != nil {
		if on, ok := cs.chanOverride[def.ref]; ok {
			return on
		}
	}
	return def.defaultOn
}

// ignored reports whether author id is on the player's ignore list (the receiver-side funnel input).
func (cs *commsState) ignored(author string) bool {
	if cs == nil {
		return false
	}
	_, ok := cs.ignore[author]
	return ok
}

// effectiveHearSet computes the player's {enabled ∩ hearable} channel refs against THIS shard's loaded
// channel_defs and the player's live *Entity (Phase 8.6). A channel is in the set iff the player has it
// ENABLED (toggle/override vs default_on) AND CAN HEAR it (the access predicate, canHear). This is the
// authoritative receiver HEAR-filter — it needs the entity + the defs, both world-side. Pure read of the
// immutable channel registry + the live entity + the session comms state; zone goroutine.
func (z *Zone) effectiveHearSet(s *session) []string {
	if s == nil || s.entity == nil {
		return nil
	}
	cs := s.comms
	var refs []string
	for _, def := range z.channelDefs().table() {
		if !cs.channelEnabled(def) {
			continue
		}
		if !def.canHear(s.entity) {
			continue
		}
		refs = append(refs, def.ref)
	}
	sort.Strings(refs) // deterministic for tests + stable wire order
	return refs
}

// anyChannelGatesHearing reports whether ANY loaded channel gates HEARING behind an access predicate (a
// require-flag or a min-attr floor). It is the cheap guard for republishCommsOnAccessChange: a world with
// no access-gated channels (the common case) pays only this immutable-registry scan and never republishes.
func (z *Zone) anyChannelGatesHearing() bool {
	for _, def := range z.channelDefs().table() {
		// The EFFECTIVE hear predicate (hearPredicate dispatches split-vs-mirror): a hear-restricted
		// channel (open speak, gated hear_access) must trigger the republish, and an announce channel
		// (gated speak, EMPTY hear_access) must not — hearing there is open.
		if ha := def.hearPredicate(); ha.requireFlag != "" || ha.minAttrName != "" {
			return true
		}
	}
	return false
}

// republishCommsOnAccessChange re-publishes a player's comms config after a mid-session change that could
// cross a channel's hear-access predicate — an affect apply/expire that moved an attribute across a
// channel's min_attr floor, or set/cleared a require-flag — so the gate's receiver HEAR-filter stops (or
// starts) matching a restricted channel WITHOUT waiting for the player's next toggle/handoff/relog
// (docs/REMAINING.md §1). A no-op unless the entity is a live player and at least one channel actually
// gates hearing. Zone goroutine (a pure read handed to the bus, like publishCommsConfig).
func (z *Zone) republishCommsOnAccessChange(e *Entity) {
	if e == nil {
		return
	}
	s, ok := sessionOf(e)
	if !ok || s == nil {
		return
	}
	if !z.anyChannelGatesHearing() {
		return
	}
	z.publishCommsConfig(s)
}

// ignoreList returns the player's ignore list as a sorted slice for the config payload. Zone goroutine.
func (z *Zone) ignoreList(s *session) []string {
	if s == nil || s.comms == nil || len(s.comms.ignore) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.comms.ignore))
	for id := range s.comms.ignore {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// publishCommsConfig computes the player's effective {enabled ∩ hearable} hear-set + ignore list and
// publishes it to their personal config subject (commbus.ConfigSubject) so the GATE re-subscribes its
// concrete channel subjects (the HEAR-filter) and caches the ignore funnel. Called on login, on a
// handoff arrival, and on every toggle/ignore mutation — so the gate's receiver-side filter is always
// recomputed by the authoritative world. Never-fatal: a disabled/closed bus is a clean no-op (comms
// degraded). Runs on the zone goroutine (a pure read of zone-owned state into a value handed to the bus,
// like the channel publish path). It does NOT block on the gate.
func (z *Zone) publishCommsConfig(s *session) {
	bus := z.commsBus()
	if bus == nil || s == nil || s.entity == nil {
		return
	}
	payload := commbus.ConfigPayload{
		HearChannels: z.effectiveHearSet(s),
		Ignore:       z.ignoreList(s),
	}
	body, err := commbus.MarshalConfig(payload)
	if err != nil {
		z.log.Debug("comms config marshal failed", "player", s.character, "err", err)
		return
	}
	// The config subject is engine mechanism (not chan/tell), so the RoleWorld handle publishes it freely.
	// AuthorID/Name are the player themselves (the config is theirs); the gate keys nothing off them.
	if err := bus.Publish(context.Background(), commbus.ConfigSubject(s.character), commbus.Message{
		AuthorID:   s.character,
		AuthorName: s.entity.Name(),
		Body:       body,
	}); err != nil {
		z.log.Debug("comms config publish failed", "player", s.character, "err", err)
	}
}

// --- handoff carry (CommsState on the snapshot, handoff-transparency) -------------------------

// dumpCommsStateJSON marshals the session's comms state to the JSON string carried on the handoff
// snapshot (handoff.go), or "" when all-default. It reuses dumpCommsState so the handoff form and the
// durable form are byte-identical (one shape). Zone goroutine.
func dumpCommsStateJSON(s *session) string {
	js := dumpCommsState(s)
	if js == nil {
		return ""
	}
	b, err := marshalCommsState(js)
	if err != nil {
		return ""
	}
	return b
}

// loadCommsStateJSON installs comms state carried on a handoff snapshot onto a pending/destination
// session. An empty string (all-default / pre-8.6 snapshot) installs nothing (defaults). Zone goroutine.
func loadCommsStateJSON(s *session, raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	js, err := unmarshalCommsState(raw)
	if err != nil || js == nil {
		return
	}
	loadCommsState(s, js)
}
