package world

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

// vitals.go — the live-vitals HUD (#40, Track 5). A player toggles `vitals on` to get their resource
// pools (hp/mana/…) shown IN the prompt and refreshed LIVE during combat, instead of only after their
// own next command. It drives both surfaces from one pref:
//
//   - the plain TEXT prompt gains a "[hp: 80/100 mana: 30/50]" prefix (z.promptMarkup); and
//   - a change-detected GMCP Char.Vitals + a fresh prompt are pushed at each combat-round boundary
//     (z.flushLiveVitals, called from runCombatRound) so a plain client's prompt and a rich client's
//     gauge both track a round's HP drain in real time.
//
// Coalescing / cadence: the live push is at the combat-ROUND boundary (one PULSE_VIOLENCE), NOT per
// setResourceCurrent — a per-change push would re-emit the text prompt every heartbeat during passive
// regen (spam). The round-boundary flush is change-detected (bytes.Equal against s.lastVitals), so it
// emits at most once per round and only when a vital actually moved. Live updates for non-combat
// changes (passive regen ticks) are a documented follow-up — they want a slower prompt cadence than the
// per-heartbeat tick to stay readable. Default OFF: a fresh session keeps the classic bare "> " prompt
// and updates on its own command, so existing behavior + tests are unchanged until a player opts in.

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
		c.Send("Live vitals ON.")
	case "off", "disable":
		s.vitalsLive = false
		c.Send("Live vitals OFF.")
	default:
		c.Send("Usage: vitals on|off")
	}
	return nil
}

// vitalsPrompt builds the "[hp: 80/100 mana: 30/50] " prefix from the entity's pooled resources (those
// with a derived max), sorted by ref for a stable order. Empty for a Living-less/contentless entity.
// Zone-goroutine read.
func (z *Zone) vitalsPrompt(e *Entity) string {
	if e == nil || e.living == nil || e.zone == nil {
		return ""
	}
	refs := make([]string, 0)
	for ref, def := range e.zone.resourceDefs().table() {
		if def.maxAttr == "" {
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
	sort.Strings(refs)
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

// flushLiveVitals pushes a live vitals update for session s when it has `vitals on` AND a vital actually
// changed since the last emit (change-detected against s.lastVitals, the SAME buffer sendPrompt uses).
// It emits the GMCP Char.Vitals delta and a fresh (vitals-bearing) prompt. Called at combat-round
// boundaries (runCombatRound). A no-op when live vitals are off or nothing moved. Zone-goroutine only.
func (z *Zone) flushLiveVitals(s *session) {
	if s == nil || !s.vitalsLive || s.entity == nil {
		return
	}
	v := z.charVitalsJSON(s.entity)
	if bytes.Equal(v, s.lastVitals) {
		return // no vital changed this round
	}
	s.lastVitals = v
	s.send(gmcpFrame("Char.Vitals", v))
	s.send(promptFrameMarkup(z.promptMarkup(s)))
}
