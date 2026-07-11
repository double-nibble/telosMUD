package world

import (
	"strings"

	"github.com/double-nibble/telosmud/internal/content"
)

// toggledefs.go — content-defined PLAYER TOGGLES (#358): an on/off preference a pack defines, a player
// flips with a verb, and Lua content reads via self:toggle("<ref>"). This is the generic version of the
// hard-coded `vitals`/`color` switches — the engine names no toggle (no built-in `overworld`), it only
// knows the SHAPE (toggleDef); a pack names which toggles exist. The overworld minimap is the first
// consumer (its `room` display template renders only when the viewer's `overworld` toggle is on).
//
// Storage/persistence mirror the content-channel per-player override EXACTLY (commsstate.go): the state is
// stored as an OVERRIDE keyed by ref (present => forced on/off; absent => the def's default_on), so a player
// who never touched a toggle picks up a changed default on the next rebuild. (Unlike channels, toggles have
// NO live hot-reload loop — they are a reboot-only shared def, listed in reloadvalidate.sharedDefKinds — so a
// changed default_on lands on a rolling reboot, not a live `reload`.) The override rides the same
// CommsStateJSON subtree, so it PERSISTS across relog and SURVIVES a cross-shard handoff for free. Verbs
// are dispatched by EXACT match after the built-in table (parser.go), like channel/custom verbs, so a
// toggle verb can never shadow or abbreviate a core verb.

// toggleDef is the runtime form of a content.ToggleDTO. Immutable after build (registered once, then
// read-only), like channelDef.
type toggleDef struct {
	ref       string
	name      string
	words     []string // lower-cased verb words that report/flip this toggle
	defaultOn bool
	desc      string
}

// buildToggleDef maps a content.ToggleDTO onto the runtime toggleDef (defineGlobals). Verb words are
// lower-cased + trimmed here so dispatch (which lower-cases the input verb) matches directly.
func buildToggleDef(t content.ToggleDTO) *toggleDef {
	words := make([]string, 0, len(t.Words))
	for _, w := range t.Words {
		if lw := strings.ToLower(strings.TrimSpace(w)); lw != "" {
			words = append(words, lw)
		}
	}
	return &toggleDef{ref: t.Ref, name: t.Name, words: words, defaultOn: t.DefaultOn, desc: t.Desc}
}

// displayName is the toggle's human label for the report line, falling back to the ref.
func (d *toggleDef) displayName() string {
	if d.name != "" {
		return d.name
	}
	return d.ref
}

// toggleForVerb returns the toggle a verb reports/flips (lower-cased), or nil. Derived from the registry
// on each lookup (the table is tiny) so a hot-reloaded def stays consistent for free — the same pattern as
// channelForVerb. dispatch consults this AFTER baseTable + abilities + custom + channel verbs (a toggle
// verb never shadows or abbreviates any of them). Read-only, safe from any zone goroutine.
func (z *Zone) toggleForVerb(v string) *toggleDef {
	for _, def := range z.toggleDefs().table() {
		for _, w := range def.words {
			if w == v {
				return def
			}
		}
	}
	return nil
}

// cmdToggle handles a content-toggle verb: bare `<word>` reports the current state, `<word> on|off` sets
// the per-player override. Purely mutates the session comms-state override (persisted by the same
// dumpCommsState path channels use); it never releases ownership, so dispatch prompts on return. Zone
// goroutine (single-writer over the session state).
func (z *Zone) cmdToggle(s *session, def *toggleDef, arg string) {
	cs := commsOf(s)
	name := def.displayName()
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "":
		z.reportToggle(s, name, def.desc, cs.toggleEnabled(def))
	case "on", "enable":
		cs.toggleOverride[def.ref] = true
		s.send(textFrame(name + " is now ON."))
	case "off", "disable":
		cs.toggleOverride[def.ref] = false
		s.send(textFrame(name + " is now OFF."))
	default:
		z.reportToggle(s, name, def.desc, cs.toggleEnabled(def))
		s.send(textFrame("Usage: " + def.words[0] + " on|off"))
	}
}

// reportToggle sends the bare-verb status line, appending the toggle's description when present so the
// player learns what the switch does.
func (z *Zone) reportToggle(s *session, name, desc string, on bool) {
	state := "OFF"
	if on {
		state = "ON"
	}
	line := name + " is " + state + "."
	if desc != "" {
		line += " " + desc
	}
	s.send(textFrame(line))
}
