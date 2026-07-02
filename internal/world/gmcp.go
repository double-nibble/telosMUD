package world

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/colormarkup"
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

// gmcpText strips the known {{TOKEN}} color vocabulary from a content-authored display string bound for
// a GMCP payload. Unknown {{...}} runs stay literal, exactly matching what a color-off telnet client
// sees (colormarkup.Strip is the shared edge tokenizer, so the two can't drift).
func gmcpText(s string) string { return colormarkup.Strip(s) }

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
		st.Target = gmcpText(e.living.fighting.Name())
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

// gmcpItem is one entry in a Char.Items.List (Phase 9.4): a stable per-instance id, the display name,
// and an attrib string of single-char flags (w=wearable, c=container, W=currently worn/wielded) for the
// client's inventory/equipment panel.
type gmcpItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Attrib string `json:"attrib,omitempty"`
}

// itemEntry builds a gmcpItem for e. wr (the holder's Wearer, or nil) decides the worn flag.
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
	return gmcpItem{ID: fmt.Sprintf("i%v", e.RuntimeID()), Name: gmcpText(e.Name()), Attrib: attrib}
}

// charItemsJSON builds a Char.Items.List payload {location, items}. location is "inv" (everything the
// player carries, worn flagged with "W") or "room" (ground items in the player's room — items only, not
// players/mobs). The Mudlet-standard one-message-per-location shape, so the client routes to the right
// panel.
func charItemsInvJSON(e *Entity) []byte {
	wr, _ := Get[*Wearer](e)
	items := []gmcpItem{}
	for _, it := range e.contents {
		items = append(items, itemEntry(it, wr))
	}
	b, _ := json.Marshal(map[string]any{"location": "inv", "items": items})
	return b
}

func charItemsRoomJSON(e *Entity) []byte {
	items := []gmcpItem{}
	if e.location != nil {
		for _, occ := range e.location.contents {
			// A ground item is any room occupant that is NOT a living creature: real items, CORPSES (a
			// Container, no Physical), and dropped containers all qualify; players and mobs (Living) do
			// not. Filtering on Has[*Living] rather than Has[*Physical] is what lets a corpse — which
			// carries only a Container — show up in the panel so a client can see loot on the ground.
			if occ == e || Has[*Living](occ) {
				continue
			}
			items = append(items, itemEntry(occ, nil))
		}
	}
	b, _ := json.Marshal(map[string]any{"location": "room", "items": items})
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
		if iv := charItemsInvJSON(e); !bytes.Equal(iv, s.lastInv) {
			s.lastInv = iv
			s.send(gmcpFrame("Char.Items.List", iv))
		}
		if ri := charItemsRoomJSON(e); !bytes.Equal(ri, s.lastRoomItems) {
			s.lastRoomItems = ri
			s.send(gmcpFrame("Char.Items.List", ri))
		}
	}
	s.send(promptFrameMarkup(z.promptMarkup(s))) // vitals-bearing prompt when `vitals on` (#40), else "> "
}
