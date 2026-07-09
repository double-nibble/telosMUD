package world

import (
	"encoding/json"
	"time"
)

// gmcprequest.go — the INBOUND GMCP request path (#92). Unlike the rest of GMCP (server-push), a rich
// client can ASK the world for data: it sends an IAC SB 201 message the gate forwards (whitelisted) as a
// ClientFrame GmcpIn, the world routes it to the owning zone as a gmcpRequestMsg, and the zone replies with
// a normal push frame. The only request today is Char.Items.Contents (open a container's panel).
//
// TRUST: this is client->world input, so it is treated as hostile. The gate forwards only a whitelist of
// request packages; the world caps the payload (server.go) and, here, resolves any named entity ONLY within
// the requester's OWN reach (inventory + their room floor, dark-room- and canSee-filtered) — so a crafted id
// can never peek into another player's container or an entity the requester couldn't otherwise see.

// maxInboundGMCPBytes caps an inbound GMCP request payload at the world's own gRPC ingress (defense-in-depth
// behind the gate's telnet-side cap) — a request payload is a small JSON object, never bulk data.
const maxInboundGMCPBytes = 4096

// gmcpReqRate / gmcpReqBurst size the per-session inbound-GMCP-request token bucket (#92, security review
// M1): a sustained ~gmcpReqRate requests/sec with a small burst. A request forces O(container) work on the
// shared zone goroutine, so a flood is dropped here — cheaply — before it can degrade the zone's latency.
const (
	gmcpReqRate  = 5.0
	gmcpReqBurst = 10.0
)

// allowGMCPRequest reports whether this session may issue one more inbound GMCP request now, refilling its
// token bucket by the elapsed time. Zone-goroutine only (single-writer), so it needs no lock.
func (s *session) allowGMCPRequest(now time.Time) bool {
	if s.gmcpReqRefill.IsZero() {
		s.gmcpReqTokens, s.gmcpReqRefill = gmcpReqBurst, now
	}
	s.gmcpReqTokens += now.Sub(s.gmcpReqRefill).Seconds() * gmcpReqRate
	if s.gmcpReqTokens > gmcpReqBurst {
		s.gmcpReqTokens = gmcpReqBurst
	}
	s.gmcpReqRefill = now
	if s.gmcpReqTokens < 1 {
		return false // over budget — silently drop (a hostile flood, or a client bug)
	}
	s.gmcpReqTokens--
	return true
}

// gmcpRequestMsg is an inbound GMCP request routed to the owning zone (the character's home zone). Handled
// on the zone goroutine, like every other player-scoped message.
type gmcpRequestMsg struct {
	id   string // character
	pkg  string // GMCP package, e.g. "Char.Items.Contents"
	json []byte // raw payload (already size-capped at the world ingress)
}

func (gmcpRequestMsg) zoneMsg() {}

// handleGMCPRequest dispatches an inbound GMCP request for a resident player. An unknown package is a silent
// no-op (the gate whitelist should already have dropped it; this is defense-in-depth). Zone-goroutine only.
func (z *Zone) handleGMCPRequest(m gmcpRequestMsg) {
	s := z.players[m.id]
	if s == nil || s.entity == nil || s.frozen || s.pending {
		return // gone, mid-handoff, or not yet placed — nothing to answer to
	}
	if !s.allowGMCPRequest(time.Now()) {
		return // rate-limited (#92 M1): drop before the parse/scan/marshal so a flood can't tax the zone
	}
	switch m.pkg {
	case "Char.Items.Contents":
		z.sendContainerContents(s, m.json)
	}
}

// sendContainerContents answers a Char.Items.Contents request: resolve the named container within the
// requester's reach, and reply with a Char.Items.List keyed to the container id. It SILENTLY drops a request
// it can't satisfy (unreachable/invisible id, not a container, closed, or a corpse still in its loot-owner
// window) — never revealing whether such an entity exists elsewhere. Zone-goroutine only.
func (z *Zone) sendContainerContents(s *session, raw []byte) {
	var req struct {
		Container string `json:"container"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.Container == "" {
		return
	}
	box := z.reachableItemByGMCPID(s.entity, req.Container)
	if box == nil {
		return // not in the requester's inventory or their (visible) room floor
	}
	cc, ok := Get[*Container](box)
	if !ok || cc.closed {
		return // not a container, or closed — a closed box reveals nothing (parity with `look in`)
	}
	// Corpse loot-ownership window (anti-ninja-loot): a bystander can't even PEEK into a fresh kill's corpse
	// until the window lapses — same gate as `get from` (container.go), so GMCP can't bypass it.
	if co, ok := Get[*CorpseOwner](box); ok && co.owned() && !co.looterIsOwner(s) && time.Now().Before(co.until) {
		return
	}
	// VISIBILITY PARITY: contents are listed unfiltered, exactly like roomItems/invItems on the push path —
	// safe today because item-level concealment (flagInvisible/flagHidden on a non-Living item) is not a
	// feature. If it is ever added, this list AND reachableItemByGMCPID's floor scan below MUST gain a
	// per-item z.canSee filter (three sites total, incl. roomItems), or GMCP would reveal what text hides.
	b, _ := json.Marshal(map[string]any{"location": itemGMCPID(box), "items": coalesceGMCPItems(box.contents, nil)})
	s.send(gmcpFrame("Char.Items.List", b))
}

// reachableItemByGMCPID resolves an item the actor can currently reach — its inventory or its room floor —
// by the stable GMCP id (itemGMCPID). It is the SECURITY SCOPE for an inbound request: only items the actor
// legitimately sees are addressable, so a guessed/stale id can't reach another player's or a hidden entity.
// Room-floor items respect the same dark-room concealment as roomItems (#99). Returns nil if unreachable.
func (z *Zone) reachableItemByGMCPID(actor *Entity, id string) *Entity {
	if actor == nil {
		return nil
	}
	for _, it := range actor.contents { // carried inventory
		if itemGMCPID(it) == id {
			return it
		}
	}
	if actor.location != nil && canSeeRoomContents(actor) { // room floor (dark-room filtered, like roomItems)
		for _, occ := range actor.location.contents {
			if occ == actor || Has[*Living](occ) {
				continue
			}
			if itemGMCPID(occ) == id {
				return occ
			}
		}
	}
	return nil
}
