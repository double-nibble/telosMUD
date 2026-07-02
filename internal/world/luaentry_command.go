package world

import (
	"log/slog"
	"strings"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/textsan"
	lua "github.com/yuin/gopher-lua"
)

// luaentry_command.go — custom Lua commands (slice 7.4e). Content registers a brand-new verb
// implemented in Lua; dispatch runs it like any command. PRECEDENCE (security/UX): a custom verb
// is consulted AFTER the built-in baseTable AND content abilities, by EXACT match only — so it
// can NEVER shadow or abbreviate a core/movement/ability verb. A custom verb that collides with a
// built-in verb word is REJECTED at load (logged), never silently shadowing.
//
// This is a Lua binding file (it builds handle binds) and is covered by the funnel-reuse lint.

// registerCustomCommand registers one content custom command's verb + aliases into the per-shard
// customCmds table, mapping each word to the Lua body. A word that collides with a BUILT-IN verb
// (baseTable, by exact name/alias) is skipped + logged — a custom command may not shadow a core
// verb. Build-time only (single goroutine).
func registerCustomCommand(d *defRegistries, cmd content.CommandDTO) {
	body := strings.TrimSpace(cmd.Lua)
	if body == "" {
		return
	}
	if d.customCmds == nil {
		d.customCmds = map[string]string{}
	}
	for _, w := range append([]string{cmd.Verb}, cmd.Aliases...) {
		lw := strings.ToLower(strings.TrimSpace(w))
		if lw == "" {
			continue
		}
		// No shadowing a built-in verb (exact name or alias). Log LOUDLY so an author sees their
		// custom verb was REJECTED (not silently dropped) and can rename it.
		if _, builtin := baseTable.byExact[lw]; builtin {
			slog.Warn("content: custom command rejected — its verb collides with a built-in command; rename it",
				"verb", lw, "command", cmd.Verb)
			continue
		}
		d.customCmds[lw] = body
	}
}

// customCommandFor returns the Lua body registered for verb v (exact match), or "" — the dispatch
// consultation point. Lock-free read of the published per-shard table.
func (z *Zone) customCommandFor(v string) string {
	b := z.defBundle()
	if b == nil || b.customCmds == nil {
		return ""
	}
	return b.customCmds[v]
}

// displayDef returns the Lua render body registered for a display surface ("score"/"who"/…), or "" when the
// pack defines none (the caller then uses its built-in fallback). Lock-free read of the published per-shard table.
func (z *Zone) displayDef(surface string) string {
	b := z.defBundle()
	if b == nil || b.displayDefs == nil {
		return ""
	}
	return b.displayDefs[surface]
}

// runCustomCommand runs a custom command's Lua body for actor `s.entity`, binding `self` (the
// actor handle) and `arg` (the verb's argument tail, textsan-cleaned). It is a clean ROOT
// invocation (a player-issued command, not inside a cascade): depth 0, nil eventBudget — exactly
// like a built-in command handler. Compile-once-per-zone + fail-closed (a broken/erroring body
// fizzles with a generic message; the breaker is 7.5). The body composes effect ops via the
// handles, all gated per 7.3c.
func (z *Zone) runCustomCommand(s *session, verb, arg, body string) {
	if z.lua == nil || s == nil || s.entity == nil {
		return
	}
	rt := z.lua
	ch := rt.chunkFor("command:"+verb, body)
	if ch == nil {
		s.send(textFrame("Nothing happens."))
		return
	}
	c := rt.rootCtx(s.entity) // clean root: the actor is the invocation actor
	binds := map[string]lua.LValue{
		"self": rt.newHandle(s.entity),
		"arg":  lua.LString(textsan.CleanMarkup(arg)),
	}
	if err := rt.invoke(ch, &luaInvocation{actor: c.actor}, binds); err != nil {
		// Fail-closed: a generic player-facing message; the raw error went to the ops log.
		s.send(textFrame("Something goes wrong."))
	}
}
