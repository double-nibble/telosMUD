package world

import (
	"bytes"
	"strconv"
	"strings"
)

// vitals.go — the live-vitals HUD (#40, Track 5). A player toggles `vitals on` to get their resource
// pools (hp/mana/…) shown IN the prompt and refreshed LIVE during combat, instead of only after their
// own next command. It drives both surfaces from one pref:
//
//   - the plain TEXT prompt gains a "[hp: 80/100 mana: 30/50]" prefix (z.promptMarkup); and
//   - a change-detected GMCP HUD (Char.Vitals/Status/Stats) + a fresh prompt are pushed at each combat-round
//     boundary (z.flushHUD, from runCombatRound) so a plain client's prompt and a rich client's gauge both
//     track a round's HP drain in real time, and out of combat on a throttled tick (gauges only).
//
// Coalescing / cadence: the shared flush (z.flushHUD) is change-detected against the last-sent buffers, so
// it emits a frame only when that payload actually moved. It fires on TWO cadences (#84):
//   - the combat-ROUND boundary (PULSE_VIOLENCE, runCombatRound) with the text prompt — fast, and the
//     player is watching the fight; and
//   - a throttled NON-combat tick (PULSE_HUD, ~2s, ensureHUDPulse) WITHOUT the text prompt — passive regen
//     pushes the live GMCP gauges to rich clients but never reprints the prompt (which would spam a plain
//     client); a plain client's text prompt catches up on its next command.
// The combat flush also pushes Char.Status/Char.Stats deltas now (#84), so a mid-combat status change with
// zero vital movement reaches a rich client's panels immediately, not only on the next command. Default
// OFF: a fresh session keeps the classic bare "> " prompt (and the HUD pulse is never armed until the first
// `vitals on`), so existing behavior + tests are unchanged until opt-in.

// vitalsCommands returns the `vitals` toggle verb (registered low-priority with the other toggles).
func vitalsCommands() []*Command {
	return []*Command{{Name: "vitals", Run: cmdVitals}}
}

// cmdVitals toggles the live-vitals prompt: `vitals on|enable` / `vitals off|disable`, or bare `vitals`
// to report the current state.
func cmdVitals(c *Context) error {
	s := c.s
	arg := strings.ToLower(strings.TrimSpace(c.Rest()))
	switch arg {
	case "":
		if s.vitalsLive {
			c.Send("Live vitals are ON (your pools show in the prompt and update during combat).")
		} else {
			c.Send("Live vitals are OFF. Use `vitals on` to show your pools in the prompt.")
		}
	case "on", "enable":
		s.vitalsLive = true
		c.z.ensureHUDPulse() // arm the non-combat live-HUD cadence (idempotent; #84)
		c.Send("Live vitals ON.")
	case "off", "disable":
		s.vitalsLive = false
		c.Send("Live vitals OFF.")
	default:
		c.Send("Usage: vitals on|off")
	}
	return nil
}

// vitalsPrompt builds the "[hp: 80/100 mana: 30/50] " prefix from the entity's HUD-visible pooled
// resources — the same gauge-filtered set as GMCP Char.Vitals (z.hudResourceRefs, #50), further limited
// to pools that can render a cur/max (a derived max that's > 0), sorted by ref for a stable order. Empty
// for a Living-less/contentless entity. Zone-goroutine read.
func (z *Zone) vitalsPrompt(e *Entity) string {
	if e == nil || e.living == nil || e.zone == nil {
		return ""
	}
	table := e.zone.resourceDefs().table()
	refs := make([]string, 0)
	for _, ref := range e.zone.hudResourceRefs() {
		if def := table[ref]; def == nil || def.maxAttr == "" {
			continue
		}
		if resourceMax(e, ref) <= 0 {
			continue
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, ref := range refs {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(ref)
		b.WriteString(": ")
		b.WriteString(strconv.Itoa(resourceCurrent(e, ref)))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(resourceMax(e, ref)))
	}
	b.WriteString("] ")
	return b.String()
}

// promptMarkup is the prompt string for session s: the classic "> ", prefixed with the live-vitals
// block when the player has `vitals on`. This is the single place the prompt text is built, so
// sendPrompt and the live flush agree.
func (z *Zone) promptMarkup(s *session) string {
	if s != nil && s.vitalsLive {
		if vp := z.vitalsPrompt(s.entity); vp != "" {
			return vp + "> "
		}
	}
	return "> "
}

// flushHUD pushes the change-detected HUD deltas for a `vitals on` session — the GMCP Char.Vitals,
// Char.Status AND Char.Stats frames whose payload moved since the last emit (#84) — and, when withPrompt,
// a fresh vitals-bearing TEXT prompt. It is the shared HUD flush both live surfaces use: change-detected
// against the SAME last-sent buffers sendPrompt uses, so it never duplicates a frame.
//
// withPrompt distinguishes the two live cadences (#84):
//   - COMBAT round boundary (runCombatRound, PULSE_VIOLENCE): withPrompt=true — the player is actively
//     watching the fight, so the text prompt tracks a round's HP drain live, AND status/stats deltas
//     (a mid-combat position/stat change with zero vital movement) now reach a rich client's panels.
//   - NON-combat throttled tick (ensureHUDPulse, PULSE_HUD): withPrompt=false — passive regen pushes the
//     live GMCP gauges but does NOT reprint the text prompt (that would spam a plain-text client every few
//     seconds); a plain client's text prompt catches up on its next command.
//
// The text prompt carries only the vitals gauge, so it is reprinted only when a VITAL actually moved (not a
// status/stats-only change). Returns whether anything was sent. A no-op when live vitals are off. Zone-only.
func (z *Zone) flushHUD(s *session, withPrompt bool) bool {
	if s == nil || !s.vitalsLive || s.entity == nil || s.frozen || s.pending {
		return false // a mid-handoff (frozen/pending) session's frames belong to the handoff machinery, not us
	}
	sent, vitalsChanged := false, false
	if v := z.charVitalsJSON(s.entity); !bytes.Equal(v, s.lastVitals) {
		s.lastVitals = v
		s.send(gmcpFrame("Char.Vitals", v))
		sent, vitalsChanged = true, true
	}
	if st := z.charStatusJSON(s.entity); !bytes.Equal(st, s.lastStatus) {
		s.lastStatus = st
		s.send(gmcpFrame("Char.Status", st))
		sent = true
	}
	if ss := z.charStatsJSON(s.entity); !bytes.Equal(ss, s.lastStats) {
		s.lastStats = ss
		s.send(gmcpFrame("Char.Stats", ss))
		sent = true
	}
	if withPrompt && vitalsChanged {
		s.send(promptFrameMarkup(z.promptMarkup(s)))
	}
	return sent
}

// PULSE_HUD is the throttled cadence for pushing live HUD updates to `vitals on` players OUT of combat
// (passive regen etc.): ~2s at the 250ms base pulse. Slower than the combat round (PULSE_VIOLENCE) — a
// non-combat vital moves slowly, and this pushes GMCP gauges only (no text prompt), so a couple of seconds
// keeps a rich client's gauge live without spamming a plain client's screen.
const PULSE_HUD uint64 = 8 //nolint:revive // PULSE_HUD parallels PULSE_VIOLENCE (the pulse-cadence naming convention).

// ensureHUDPulse arms the per-zone non-combat HUD cadence the first time a player enables live vitals. It
// stays armed for the zone's life (a cheap per-tick loop that skips players without `vitals on`); the
// combat round has its own faster, prompt-bearing flush, and change-detection makes an overlapping tick a
// no-op. Zone-goroutine only (called from cmdVitals).
func (z *Zone) ensureHUDPulse() {
	if z == nil || z.hudPulse != nil || z.pulses == nil {
		return
	}
	z.hudPulse = z.pulses.every(PULSE_HUD, func(uint64) bool {
		for _, s := range z.players {
			// Only NON-combat players: an in-combat player is flushed (with the prompt) by runCombatRound's
			// own faster per-round flush, so this throttled tick leaves them to it — no overlap.
			if s.vitalsLive && (s.entity == nil || s.entity.living == nil || s.entity.living.fighting == nil) {
				z.flushHUD(s, false) // GMCP gauges only — the text prompt catches up on the next command
			}
		}
		return true // keep ticking while the zone runs
	})
}
