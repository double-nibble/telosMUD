package world

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/colormarkup"
	"github.com/double-nibble/telosmud/internal/textsan"
)

// roomNum maps a room's ProtoRef to the stable integer id GMCP Room.Info uses (`num`, and the exit
// targets). A 32-bit FNV-1a hash is stateless and process-independent — the SAME ref always yields the
// SAME num across shards and restarts, so a client's accreted minimap stays consistent — with a
// negligible collision rate at MUD room counts. (A tool-minted DB-PK lookup table is the eventual
// stronger form; see the room-identity note — this is the stateless v1.)
func roomNum(ref ProtoRef) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(ref))
	return int(h.Sum32())
}

// gmcp.go is the world-side GMCP emitter (Phase 9.2+): it builds the structured HUD payloads and emits
// them as ServerFrame_Gmcp frames ALONGSIDE the text prompt, from the same dispatch point — so the HUD
// and the prompt never drift. The gate filters each frame by the client's Core.Supports and only writes
// it to a GMCP-enabled client (Phase 9.1), so the world emits unconditionally: a plain-telnet player's
// gate silently drops these. Change-detection (per-session last-sent) keeps it from re-emitting an
// identical payload on every prompt.
//
// Content-authored NAMES may carry {{TOKEN}} color markup, which only the telnet edge renders — GMCP is
// structured data a rich client displays raw, so every name-shaped field below goes through gmcpText
// (JSON-escaping makes a leaked token injection-safe, but the client would SHOW the literal braces).

// gmcpText sanitizes a content-authored display string bound for a GMCP payload. It (1) strips the known
// {{TOKEN}} color vocabulary — unknown {{...}} runs stay literal, exactly matching what a color-off telnet
// client sees (colormarkup.Strip is the shared edge tokenizer, so the two can't drift) — and (2) neutralizes
// the Trojan-Source bidi-override subset (#22). Step 2 is essential HERE because these frames reach the
// client via gate.go's verbatim WriteGMCP forward, which BYPASSES telnet.sanitizeOutput (where the telnet
// text path drops the same subset); JSON-escaping keeps a leaked override wire-safe but NOT display-safe, so
// a content-authored mob/item/room name could otherwise spoof a rich client. This is the single funnel for
// every GMCP display name (Char.Status target, Room.Info name/environment, Char.Items/Room.Players names),
// so both guarantees hold for all of them in one place, and telnet vs GMCP display stays consistent.
func gmcpText(s string) string { return textsan.NeutralizeBidi(colormarkup.Strip(s)) }

// gmcpFrame wraps a GMCP package name + JSON payload as a world->gate ServerFrame.
func gmcpFrame(pkg string, payload []byte) *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Gmcp{Gmcp: &playv1.GmcpOut{Pkg: pkg, Json: payload}}}
}

// charVitalsJSON builds the Char.Vitals payload from the entity's CONTENT-DEFINED resource pools: for
// each HUD-visible resource (hudResourceRefs — the gauge filter, #50) it emits "<ref>": current and
// "max<ref>": max (the GMCP maxhp/maxmp convention). Nothing is hardcoded — the engine names no pool —
// honoring the "engine = mechanism, content = flavor" pillar. Deterministic (map marshal sorts keys),
// so change-detection compares cleanly.
func (z *Zone) charVitalsJSON(e *Entity) []byte {
	m := make(map[string]int)
	for _, ref := range z.hudResourceRefs() {
		m[ref] = resourceCurrent(e, ref)
		m["max"+ref] = resourceMax(e, ref)
	}
	b, _ := json.Marshal(m)
	return b
}

// hudResourceRefs returns the resource refs that appear in the PLAYER-FACING HUD — the GMCP Char.Vitals
// gauges AND the live-vitals prompt (#40/#50), which must agree on "what's player-visible". The gauge
// filter (#50): when ANY resource opts in with gauge:true, only gauged pools are returned (an internal
// pool like a per-round reaction budget stays out of the HUD); when NONE are flagged (an un-flagged
// pack), all pools are returned (backward-compat). Sorted for deterministic output. Zone-goroutine read.
func (z *Zone) hudResourceRefs() []string {
	table := z.defs.res.table()
	anyGauge := false
	for _, def := range table {
		if def != nil && def.gauge {
			anyGauge = true
			break
		}
	}
	refs := make([]string, 0, len(table))
	for ref, def := range table {
		if anyGauge && (def == nil || !def.gauge) {
			continue
		}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

// charStatsJSON builds the Char.Stats payload from the CONTENT-flagged player-facing attributes: every
// attribute a pack marked `stat: true` (AttributeDTO.Stat) → its resolved value. The engine names no
// stat — content chooses which attributes are stats — so derived/internal attributes (max_hp, accuracy,
// soak_*) stay out of the panel. Values are the attr()-resolved numbers; an integer-valued float
// marshals without a decimal (14, not 14.0). Deterministic (map marshal sorts keys).
func (z *Zone) charStatsJSON(e *Entity) []byte {
	m := make(map[string]float64)
	for ref, def := range z.defs.attr.table() {
		if def != nil && def.stat {
			m[ref] = attr(e, ref)
		}
	}
	b, _ := json.Marshal(m)
	return b
}

// charStatusJSON builds the Char.Status payload: the player's position state and, if fighting, the
// target's name. state is "fighting" / "dead" / "standing" from the engine's position; target is the
// current Living.fighting opponent.
func (z *Zone) charStatusJSON(e *Entity) []byte {
	st := struct {
		State  string `json:"state"`
		Target string `json:"target,omitempty"`
	}{State: "standing"}
	switch position(e) {
	case posFighting:
		st.State = "fighting"
	case posDead:
		st.State = "dead"
	}
	if e.living != nil && e.living.fighting != nil {
		// Route the opponent's name through the canSee chokepoint (#32/#28): an invisible foe the viewer
		// can't perceive renders as "Someone" (the same leak guard as act()), never its real name.
		st.Target = gmcpText(z.nameFor(e, e.living.fighting, false))
	}
	b, _ := json.Marshal(st)
	return b
}

// roomInfoJSON builds the GMCP Room.Info payload for the room entity r: the stable num, name, zone, the
// environment (the room's content sector), and exits as direction→destination-num. The client accretes
// a minimap from num + exits across rooms it has seen; zone groups them; environment picks terrain
// coloring. (Coordinates — coord [zone,x,y,z] — are 9.3b, pending a content coord schema.)
func (z *Zone) roomInfoJSON(r *Entity) []byte {
	zoneName, _ := parseRef(r.proto)
	info := struct {
		Num         int            `json:"num"`
		Name        string         `json:"name"`
		Zone        string         `json:"zone"`
		Environment string         `json:"environment,omitempty"`
		Coord       []int          `json:"coord,omitempty"`
		Exits       map[string]int `json:"exits"`
	}{
		Num:   roomNum(r.proto),
		Name:  gmcpText(r.Name()),
		Zone:  zoneName,
		Exits: map[string]int{},
	}
	if r.room != nil {
		info.Environment = gmcpText(r.room.sector)
		for dir, dst := range r.room.exits {
			info.Exits[dir] = roomNum(dst)
		}
		// coord is [zone-id, x, y, z]: the content's [x,y,z] prefixed with a stable per-zone id so the
		// client groups rooms by zone for layout. Omitted when the room has no authored coords.
		if len(r.room.coord) == 3 {
			info.Coord = append([]int{roomNum(ProtoRef(zoneName))}, r.room.coord...)
		}
	}
	b, _ := json.Marshal(info)
	return b
}

// gmcpItem is one entry in a Char.Items panel (Phase 9.4): a stable id, the display name, an attrib
// string of single-char flags (w=wearable, c=container, W=currently worn/wielded), and an optional
// coalescing count (#26) — N when identical discrete items GROUP into one "torch (5)" entry, omitted for
// a singleton. The id is STABLE across counts: a grouped discrete entry uses "g<hash>" derived from the
// group's identity (proto+delta), so raising/lowering the count is a Char.Items.Update on the same id;
// a non-grouping item (worn gear, a material, a container — each individually meaningful) uses its
// per-instance "i<runtimeID>". Stable ids are what make the incremental Add/Remove/Update deltas (#48)
// diff cleanly.
type gmcpItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Attrib string `json:"attrib,omitempty"`
	Count  int    `json:"count,omitempty"`
}

// itemEntry builds a gmcpItem for e (singleton id, count 0). wr (the holder's Wearer, or nil) decides the
// worn flag. Callers that group override ID/Count via coalesceGMCPItems.
func itemEntry(e *Entity, wr *Wearer) gmcpItem {
	attrib := ""
	if Has[*Wearable](e) {
		attrib += "w"
	}
	if Has[*Container](e) {
		attrib += "c"
	}
	if wr != nil && wr.slotOf(e) != WearLocNone {
		attrib += "W"
	}
	return gmcpItem{ID: itemGMCPID(e), Name: gmcpText(e.Name()), Attrib: attrib}
}

// itemGMCPID is the stable per-instance GMCP id for a non-grouping item — "i<runtimeID>". A container is
// never coalesced (its hidden contents differ), so a container the client names in a Char.Items list always
// carries this id; #92's Char.Items.Contents request resolves back to the entity by it.
func itemGMCPID(e *Entity) string { return fmt.Sprintf("i%v", e.RuntimeID()) }

// coalesceGMCPItems groups a slice of items into Char.Items entries the SAME way the plain-telnet listing
// coalesces (coalesceItemLines): identical DISCRETE items merge into one entry carrying a Count (#26).
// Materials (their own stack count), containers (hidden differing contents), and WORN gear (each slot is
// individually meaningful and toggles) NEVER group — each keeps its per-instance "i<id>". A grouped entry
// gets a count-stable "g<hash>" id (see gmcpItem). First-appearance order, matching the text listing.
func coalesceGMCPItems(items []*Entity, wr *Wearer) []gmcpItem {
	type grp struct {
		item gmcpItem
		n    int
	}
	order := make([]string, 0, len(items))
	groups := map[string]*grp{}
	uniq := 0
	for _, it := range items {
		worn := wr != nil && wr.slotOf(it) != WearLocNone
		grouping := !isMaterial(it) && !Has[*Container](it) && !worn
		var key string
		if grouping {
			// STABLE group identity: prototype + per-instance delta (bound state + rolled quality), the
			// same key coalesceItemLines uses — encoding/json sorts map keys so identical affix maps group.
			key = string(it.proto) + "\x00" + string(dumpItemDelta(it))
		} else {
			uniq++
			key = "\x00u" + strconv.Itoa(uniq) // never groups — its own line/entry
		}
		g := groups[key]
		if g == nil {
			entry := itemEntry(it, wr)
			if grouping {
				entry.ID = "g" + gmcpGroupID(key) // count-stable id (not the first instance's runtime id)
			}
			g = &grp{item: entry}
			groups[key] = g
			order = append(order, key)
		}
		g.n++
	}
	out := make([]gmcpItem, 0, len(order))
	for _, key := range order {
		g := groups[key]
		item := g.item
		if g.n > 1 {
			item.Count = g.n
		}
		out = append(out, item)
	}
	return out
}

// gmcpGroupID hashes a coalescing group key to a short stable hex id (fnv-64a). It is NOT sent as raw
// bytes (the key holds JSON/NUL) — the client only ever sees the hex, and the id is stable as long as the
// group (proto+delta) exists, so a count change is a same-id Update, not a Remove+Add.
func gmcpGroupID(key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return strconv.FormatUint(h.Sum64(), 16)
}

// gmcpOccupant is one entry in Room.Players (#33): a room creature (player or mob) the viewer can see.
type gmcpOccupant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // "player" | "mob"
}

// roomPlayersJSON builds the GMCP Room.Players payload — the room's VISIBLE creature occupants (players +
// mobs) as seen BY viewer, routed through the canSee chokepoint (#33/#28) so an occupant the viewer can't
// perceive (invisible, no detect) never appears; a holylight viewer sees all. Excludes the viewer itself
// and non-creature contents (ground items ride Char.Items). Room-contents order → deterministic bytes for
// clean change-detection. Names go through gmcpText (the shared {{token}} strip) like every GMCP name.
func (z *Zone) roomPlayersJSON(viewer *Entity) []byte {
	occ := []gmcpOccupant{}
	if viewer != nil && viewer.location != nil {
		for _, o := range viewer.location.contents {
			if o == viewer {
				continue
			}
			isPlayer := Has[*PlayerControlled](o)
			if o.living == nil && !isPlayer {
				continue // not a creature (a ground item / corpse) — belongs to Char.Items
			}
			if !z.canSee(viewer, o) {
				continue // the visibility chokepoint (#28): an unseen occupant is omitted
			}
			typ := "mob"
			if isPlayer {
				typ = "player"
			}
			occ = append(occ, gmcpOccupant{ID: fmt.Sprintf("i%v", o.RuntimeID()), Name: gmcpText(o.Name()), Type: typ})
		}
	}
	b, _ := json.Marshal(occ)
	return b
}

// invItems / roomItems build the coalesced Char.Items entry list for a location. inv is everything the
// player carries (worn flagged "W"); room is ground items in the player's room (items/corpses, NOT
// players/mobs — a corpse carries only a Container, so filtering on Has[*Living] keeps loot visible).
func invItems(e *Entity) []gmcpItem {
	wr, _ := Get[*Wearer](e)
	return coalesceGMCPItems(e.contents, wr)
}

func roomItems(e *Entity) []gmcpItem {
	// VISIBILITY: entity-level concealment (flagInvisible) is Living-scoped and this list excludes all Living
	// occupants, so no concealed CREATURE can appear here. But ROOM-level darkness (#99) conceals the whole
	// room — ground loot included — so this walk must respect it, or a GMCP client would receive Char.Items
	// frames naming floor loot a light-blind player can't see (a text-vs-GMCP parity break, the exact leak
	// lookRoom's pitch-black short-circuit closes on the telnet path). Returning empty here lets diffItems
	// emit Remove frames on entering the dark and a full re-list on relighting. If item-level invisibility is
	// ever added, this walk MUST also gain a per-item z.canSee(e, occ) filter (like lookRoom).
	if e.location == nil || !canSeeRoomContents(e) {
		return []gmcpItem{}
	}
	ground := make([]*Entity, 0, len(e.location.contents))
	for _, occ := range e.location.contents {
		if occ == e || Has[*Living](occ) {
			continue
		}
		ground = append(ground, occ)
	}
	return coalesceGMCPItems(ground, nil)
}

// charItemsInvJSON / charItemsRoomJSON build the FULL Char.Items.List {location, items} snapshot — the
// login / reconnect / handoff-arrival payload and the diff baseline. The Mudlet-standard one-message-per-
// location shape. Steady-state changes ride the incremental deltas (diffItems) instead of a full re-send.
func charItemsInvJSON(e *Entity) []byte {
	b, _ := json.Marshal(map[string]any{"location": "inv", "items": invItems(e)})
	return b
}

func charItemsRoomJSON(e *Entity) []byte {
	b, _ := json.Marshal(map[string]any{"location": "room", "items": roomItems(e)})
	return b
}

// sendPrompt emits the HUD frames whose payload CHANGED since the last prompt, then the text prompt —
// the single hook every dispatch path ends on (replacing the bare promptFrame send). The HUD rides the
// same event as the prompt so a client's gauge and the "> " never disagree. Zone-goroutine only (it
// reads/writes the session's last-sent buffers), so no locking is needed.
func (z *Zone) sendPrompt(s *session) {
	if e := s.entity; e != nil {
		if v := z.charVitalsJSON(e); !bytes.Equal(v, s.lastVitals) {
			s.lastVitals = v
			s.send(gmcpFrame("Char.Vitals", v))
		}
		if st := z.charStatusJSON(e); !bytes.Equal(st, s.lastStatus) {
			s.lastStatus = st
			s.send(gmcpFrame("Char.Status", st))
		}
		if ss := z.charStatsJSON(e); !bytes.Equal(ss, s.lastStats) {
			s.lastStats = ss
			s.send(gmcpFrame("Char.Stats", ss))
		}
		s.lastInvItems = z.diffItems(s, "inv", invItems(e), s.lastInvItems)
		s.lastRoomItems = z.diffItems(s, "room", roomItems(e), s.lastRoomItems)
		if rp := z.roomPlayersJSON(e); !bytes.Equal(rp, s.lastRoomPlayers) {
			s.lastRoomPlayers = rp
			s.send(gmcpFrame("Room.Players", rp))
		}
	}
	s.send(promptFrameMarkup(z.promptMarkup(s))) // vitals-bearing prompt when `vitals on` (#40), else "> "
}

// diffItems emits the minimal GMCP Char.Items frames for one location and returns the new per-id snapshot
// to store on the session. On the FIRST emit for the location (last == nil — a login, reconnect, or
// handoff arrival) it sends the FULL Char.Items.List so a fresh client gets the whole panel in one frame;
// thereafter it sends only Char.Items.Add / .Remove / .Update for the entries that changed (#48), so a
// single pickup no longer re-ships the whole inventory. Entries are keyed by their STABLE id (gmcpItem),
// so a coalescing count change (#26) is a same-id Update. Removes/adds are emitted in sorted-id order for
// a deterministic frame sequence. Zone-goroutine only (it reads/writes session state).
func (z *Zone) diffItems(s *session, location string, items []gmcpItem, last map[string]gmcpItem) map[string]gmcpItem {
	next := make(map[string]gmcpItem, len(items))
	for _, it := range items {
		next[it.ID] = it
	}
	if last == nil {
		b, _ := json.Marshal(map[string]any{"location": location, "items": items})
		s.send(gmcpFrame("Char.Items.List", b))
		return next
	}
	// Removes + Updates: walk the previous set in sorted-id order.
	oldIDs := make([]string, 0, len(last))
	for id := range last {
		oldIDs = append(oldIDs, id)
	}
	sort.Strings(oldIDs)
	for _, id := range oldIDs {
		nw, ok := next[id]
		if !ok {
			b, _ := json.Marshal(map[string]any{"location": location, "item": gmcpItem{ID: id}})
			s.send(gmcpFrame("Char.Items.Remove", b))
			continue
		}
		if nw != last[id] {
			b, _ := json.Marshal(map[string]any{"location": location, "item": nw})
			s.send(gmcpFrame("Char.Items.Update", b))
		}
	}
	// Adds: new ids not previously present, sorted.
	newIDs := make([]string, 0, len(next))
	for id := range next {
		if _, had := last[id]; !had {
			newIDs = append(newIDs, id)
		}
	}
	sort.Strings(newIDs)
	for _, id := range newIDs {
		b, _ := json.Marshal(map[string]any{"location": location, "item": next[id]})
		s.send(gmcpFrame("Char.Items.Add", b))
	}
	return next
}
