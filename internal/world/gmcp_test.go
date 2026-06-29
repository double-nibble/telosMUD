package world

import (
	"encoding/json"
	"testing"
)

// gmcp_test.go covers the world-side GMCP HUD (Phase 9.2): the content-driven Char.Vitals / Char.Status
// payload builders and the change-detected emit in sendPrompt.

// drainGMCP collects the latest GMCP payload per package from a session's out channel (non-blocking).
func drainGMCP(s *session) map[string]string {
	out := map[string]string{}
	for {
		select {
		case f := <-s.out:
			if g := f.GetGmcp(); g != nil {
				out[g.GetPkg()] = string(g.GetJson())
			}
		default:
			return out
		}
	}
}

func TestCharVitalsJSONContentDriven(t *testing.T) {
	z, caster := abilityTestZone(t) // defines hp (max 100) + mana (max 100)
	setResourceCurrent(caster.entity, "hp", 70)
	setResourceCurrent(caster.entity, "mana", 30)

	var m map[string]int
	if err := json.Unmarshal(z.charVitalsJSON(caster.entity), &m); err != nil {
		t.Fatalf("Char.Vitals not valid JSON: %v", err)
	}
	// Content-driven: every registered resource appears as <ref> + max<ref>; the engine names none.
	want := map[string]int{"hp": 70, "maxhp": 100, "mana": 30, "maxmana": 100}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("Char.Vitals[%q] = %d, want %d (full payload %v)", k, m[k], v, m)
		}
	}
}

func TestCharStatusJSONReflectsCombat(t *testing.T) {
	z, caster := abilityTestZone(t)

	// Standing by default.
	var st struct {
		State  string `json:"state"`
		Target string `json:"target"`
	}
	json.Unmarshal(z.charStatusJSON(caster.entity), &st)
	if st.State != "standing" || st.Target != "" {
		t.Fatalf("idle status = %+v, want standing + no target", st)
	}

	// Fighting a mob → state fighting + the target's name.
	mob := makeMobTarget(z, caster.entity, "goblin")
	z.startFight(caster.entity, mob)
	json.Unmarshal(z.charStatusJSON(caster.entity), &st)
	if st.State != "fighting" || st.Target != "goblin" {
		t.Fatalf("combat status = %+v, want fighting + goblin", st)
	}
}

func TestSendPromptEmitsHUDOnChangeOnly(t *testing.T) {
	z, caster := abilityTestZone(t)
	setResourceCurrent(caster.entity, "hp", 100)

	// First prompt: the initial HUD is emitted (last-sent is empty).
	drainGMCP(caster) // clear
	z.sendPrompt(caster)
	first := drainGMCP(caster)
	if _, ok := first["Char.Vitals"]; !ok {
		t.Fatal("first sendPrompt did not emit Char.Vitals")
	}
	if _, ok := first["Char.Status"]; !ok {
		t.Fatal("first sendPrompt did not emit Char.Status")
	}

	// Second prompt, nothing changed: NO new HUD frame (only the prompt).
	z.sendPrompt(caster)
	second := drainGMCP(caster)
	if _, ok := second["Char.Vitals"]; ok {
		t.Fatal("unchanged Char.Vitals was re-emitted on the next prompt (change-detection failed)")
	}

	// HP changes → Char.Vitals re-emitted with the new value.
	setResourceCurrent(caster.entity, "hp", 55)
	z.sendPrompt(caster)
	third := drainGMCP(caster)
	v, ok := third["Char.Vitals"]
	if !ok {
		t.Fatal("a vitals change did not re-emit Char.Vitals")
	}
	var m map[string]int
	json.Unmarshal([]byte(v), &m)
	if m["hp"] != 55 {
		t.Fatalf("re-emitted Char.Vitals hp = %d, want 55", m["hp"])
	}
}

func TestSendPromptReEmitsStatusOnStateChange(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.sendPrompt(caster)
	drainGMCP(caster) // clear the initial HUD

	// Enter combat: vitals unchanged, but Char.Status changes (standing → fighting) and must re-emit.
	mob := makeMobTarget(z, caster.entity, "goblin")
	z.startFight(caster.entity, mob)
	z.sendPrompt(caster)
	got := drainGMCP(caster)
	st, ok := got["Char.Status"]
	if !ok {
		t.Fatal("entering combat did not re-emit Char.Status")
	}
	if !json.Valid([]byte(st)) || !contains([]string{st}, "fighting") {
		t.Fatalf("Char.Status after entering combat = %q, want fighting", st)
	}
}

func TestReconnectReprimesHUD(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.sendPrompt(caster)
	drainGMCP(caster) // initial HUD sent; lastVitals/lastStatus now populated

	// A reconnect reuses the same session but a NEW gate connection with no HUD state. The re-attach
	// handler clears the change-detection buffers (zone.go) so the next prompt re-primes the HUD even
	// though vitals are unchanged — this asserts that contract directly.
	caster.lastVitals, caster.lastStatus = nil, nil
	z.sendPrompt(caster)
	got := drainGMCP(caster)
	if _, ok := got["Char.Vitals"]; !ok {
		t.Fatal("after a HUD-buffer clear (reconnect), Char.Vitals was not re-primed")
	}
	if _, ok := got["Char.Status"]; !ok {
		t.Fatal("after a HUD-buffer clear (reconnect), Char.Status was not re-primed")
	}
}
