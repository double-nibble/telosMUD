package world

import (
	"bytes"
	"encoding/json"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// gmcp.go is the world-side GMCP emitter (Phase 9.2+): it builds the structured HUD payloads and emits
// them as ServerFrame_Gmcp frames ALONGSIDE the text prompt, from the same dispatch point — so the HUD
// and the prompt never drift. The gate filters each frame by the client's Core.Supports and only writes
// it to a GMCP-enabled client (Phase 9.1), so the world emits unconditionally: a plain-telnet player's
// gate silently drops these. Change-detection (per-session last-sent) keeps it from re-emitting an
// identical payload on every prompt.

// gmcpFrame wraps a GMCP package name + JSON payload as a world->gate ServerFrame.
func gmcpFrame(pkg string, payload []byte) *playv1.ServerFrame {
	return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Gmcp{Gmcp: &playv1.GmcpOut{Pkg: pkg, Json: payload}}}
}

// charVitalsJSON builds the Char.Vitals payload from the entity's CONTENT-DEFINED resource pools: for
// every registered resource it emits "<ref>": current and "max<ref>": max (the GMCP maxhp/maxmp
// convention). Nothing is hardcoded — a pack that defines hp/mana/move yields all three, the engine
// names none — honoring the "engine = mechanism, content = flavor" pillar. Deterministic (map marshal
// sorts keys), so change-detection compares cleanly.
func (z *Zone) charVitalsJSON(e *Entity) []byte {
	m := make(map[string]int)
	for ref := range z.defs.res.table() {
		m[ref] = resourceCurrent(e, ref)
		m["max"+ref] = resourceMax(e, ref)
	}
	b, _ := json.Marshal(m)
	return b
}

// charStatusJSON builds the Char.Status payload: the player's position state and, if fighting, the
// target's name. state is "fighting" / "dead" / "standing" from the engine's position; target is the
// current Living.fighting opponent. (Char.Stats — str/dex/level/xp — is deferred pending a content
// "which attributes are player-facing stats" flag; see docs/FOLLOW-UPS.md.)
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
		st.Target = e.living.fighting.Name()
	}
	b, _ := json.Marshal(st)
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
	}
	s.send(promptFrame())
}
