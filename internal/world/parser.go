package world

// Command parser (docs/MUDLIB.md §6). This file turns a raw line of player input into
// a resolved Command and runs it. It replaces the hardcoded verb switch slice 1 carried
// in commands.go. Everything here runs on the zone goroutine (single-writer): the parser
// is pure CPU over zone-owned data and never blocks or spawns.
//
// The pipeline (MUDLIB §6):
//
//	line
//	 └─ split -> (verb, rest)              (alias expansion is a later phase)
//	 └─ resolve verb in the active table   (abbreviation-aware, §6)
//	 └─ Position / Level / flag gating      (shape only this slice)
//	 └─ cmd.Run(ctx)                        (targets resolved lazily inside, §7)
//
// Untrusted-input discipline: the line is raw player bytes. An unknown verb fails with
// "Huh?" (unchanged); abbreviation is deterministic (priority order, documented below);
// no verb's arguments are ever interpreted as a format string (see act.go). A command
// handler must never block the zone loop.

import "strings"

// CmdFlag is a bitmask of command modifiers (MUDLIB §6). None are exercised this slice;
// the field carries the documented shape so later slices (combat lag, hidden admin
// verbs, socials) gate without churning the registry.
type CmdFlag uint32

const (
	// CmdHidden omits the command from help/abbreviation suggestion listings. Shape only.
	CmdHidden CmdFlag = 1 << iota
	// CmdNoWhileFighting refuses the command while the actor is in combat. Shape only
	// (no combat until Phase 6).
	CmdNoWhileFighting
)

// Command is one registered verb (MUDLIB §6). Name is the canonical spelling; Aliases
// are additional exact spellings (e.g. "l" for look, "'" for say) that are NOT
// abbreviation candidates but match exactly. Run executes the verb against a Context.
//
// priority orders abbreviation: when a typed prefix matches several command names, the
// lowest priority value wins, so common verbs (movement, look) beat rare ones — "n"
// resolves to north, never to a hypothetical "nuke" (MUDLIB §6). priority is assigned at
// registration from the table's declared order (see registerCommands), so it is stable
// and data-driven, not scattered across handlers.
type Command struct {
	Name     string
	Aliases  []string
	MinPos   int     // minimum Living.position to run; shape only this slice (MUDLIB §6)
	Level    int     // minimum level / admin gate; shape only this slice
	Flags    CmdFlag // modifiers; shape only this slice
	Run      func(*Context) error
	priority int // abbreviation tie-break; set by the registry, lower wins
}

// commandTable is the resolvable command set. It holds the canonical commands in
// priority order plus an exact-spelling index (canonical names AND aliases) for the
// fast/unambiguous path. A real "active table stack" (line editor, OLC, menus pushing
// their own table — MUDLIB §6) is a later concern; this slice has a single base table,
// but resolve() takes the table as a parameter so the stack drops in without touching
// call sites.
type commandTable struct {
	byExact map[string]*Command // exact name or alias -> command (exact match wins, §6)
	ordered []*Command          // canonical commands, ascending priority, for prefix scan
}

// baseTable is the always-available command set, built once at init from the declared
// verb list. The parser resolves against it for every line (the active-table stack would
// layer on top; absent this slice).
var baseTable = newCommandTable(registerCommands())

// newCommandTable indexes a priority-ordered slice of commands. The input order IS the
// priority order (index 0 = highest priority), which is how "n" deterministically beats
// any later-registered prefix collision. Exact spellings (name + every alias) index into
// byExact; only canonical names participate in prefix/abbreviation matching.
func newCommandTable(cmds []*Command) *commandTable {
	t := &commandTable{byExact: make(map[string]*Command, len(cmds)*2)}
	for i, c := range cmds {
		c.priority = i
		t.ordered = append(t.ordered, c)
		t.byExact[c.Name] = c
		for _, a := range c.Aliases {
			t.byExact[a] = c
		}
	}
	return t
}

// resolve maps a typed verb to a command using the documented precedence (MUDLIB §6):
//
//  1. an exact match (canonical name or alias) always wins;
//  2. otherwise the verb is treated as an abbreviation: among canonical names that have
//     the verb as a prefix, the highest-priority (lowest priority value) command wins.
//
// The scan is over a fixed, small command list, so it is O(commands) per line with no
// allocation — bounded regardless of input. An empty verb or one matching nothing
// returns (nil, false), which dispatch renders as "Huh?". The match is case-insensitive;
// verb is expected already lower-cased by the caller.
func (t *commandTable) resolve(verb string) (*Command, bool) {
	if verb == "" {
		return nil, false
	}
	if c, ok := t.byExact[verb]; ok {
		return c, true
	}
	// Abbreviation: first command (in priority order) whose name has verb as a prefix.
	for _, c := range t.ordered {
		if len(verb) <= len(c.Name) && c.Name[:len(verb)] == verb {
			return c, true
		}
	}
	return nil, false
}

// Context is what a command handler works through (MUDLIB §6). It hides the zone/entity
// plumbing: the actor's session and entity, the parsed argument string, and the helpers
// a handler calls (Send to the actor, Act for perspective messaging, Target/Targets for
// Diku resolution). Constructed per dispatched line and never escapes the zone goroutine.
type Context struct {
	z     *Zone
	s     *session // the actor's connection (output sink); never nil for a dispatched line
	Actor *Entity  // the actor's in-world entity (s.entity)
	arg   string   // the verb's argument tail, trimmed ("hi" in `say hi`)
	moved bool     // set by movement handlers when they released ownership (see dispatch)
}

// Rest returns the full argument string after the verb (trimmed). For `say hello there`
// it is "hello there".
func (c *Context) Rest() string { return c.arg }

// Arg returns the i-th whitespace-delimited argument word (0-based), or "" when absent.
// Bounded: it stops at the requested index and never materializes an unbounded slice.
func (c *Context) Arg(i int) string {
	rest := c.arg
	for ; i >= 0; i-- {
		w, r := split(rest)
		if i == 0 {
			return w
		}
		rest = r
		if rest == "" && i > 0 {
			return ""
		}
	}
	return ""
}

// Send queues markup to the actor's own stream (MUDLIB §6). Plain passthrough to the
// session sink; markup is treated as data, never a format string.
func (c *Context) Send(markup string) { c.s.send(textFrame(markup)) }

// Target resolves a single entity for c's argument, searching the given scopes in order
// and applying the visibility filter (§7). It uses Arg(0) as the target token — the
// classic `get sword` form. Returns (nil, false) when nothing matches.
func (c *Context) Target(scopes ...Scope) (*Entity, bool) {
	matches := c.z.Resolve(c.Actor, parseTargetSpec(c.Arg(0)), scopes...)
	if len(matches) == 0 {
		return nil, false
	}
	return matches[0], true
}

// Targets resolves every entity matching c's argument across the given scopes (§7), the
// `all.coin` / `all` form. Returns nil when nothing matches.
func (c *Context) Targets(scopes ...Scope) []*Entity {
	return c.z.Resolve(c.Actor, parseTargetSpec(c.Arg(0)), scopes...)
}

// dispatch parses and runs one line of player input. It is called only from the zone
// goroutine (via handle -> inputMsg), so every handler runs lock-free against zone state
// (MUDLIB §4). It replaces the slice-1 hardcoded switch with the command registry while
// preserving every external behavior: a blank line re-prompts, an unknown verb is "Huh?",
// movement that released ownership early-returns (no prompt — the destination owns it),
// and every other path ends with a fresh prompt.
func (z *Zone) dispatch(s *session, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		// Blank line: just re-prompt.
		z.sendPrompt(s)
		return
	}

	verb, rest := split(line)
	lower := strings.ToLower(verb)

	// AFK clears on the player's NEXT input (Phase 8.6): any non-blank command they type un-AFKs them and
	// refreshes presence/who. The `afk` command runs AFTER this clear, so `afk <msg>` still SETS afk (it
	// re-sets it inside the handler) — only a DIFFERENT command clears a standing AFK. Cheap: a no-op
	// unless the player is currently AFK.
	z.clearAFKOnInput(s, lower)

	cmd, ok := baseTable.resolve(lower)
	if !ok {
		// Not a built-in verb: try a content-defined ability command (Phase 5.3). The ability table is
		// consulted AFTER the baseTable so a content ability never shadows a core verb. A match enters
		// the ability lifecycle (ability.go); the verb's tail is the target argument. No prompt is sent
		// by the lifecycle, so we prompt here on return (it never releases ownership).
		if def := z.abilityForVerb(lower); def != nil {
			z.log.Debug("dispatch: ability command", "player", s.character, "verb", lower, "ability", def.ref)
			z.castAbility(s, def, rest, nil)
			z.sendPrompt(s)
			return
		}
		// Custom Lua command (7.4e): consulted LAST and by EXACT match only, so it never shadows or
		// abbreviates a core/movement/ability verb. A match runs the Lua body (pcall-isolated).
		if body := z.customCommandFor(lower); body != "" {
			z.log.Debug("dispatch: custom command", "player", s.character, "verb", lower)
			z.runCustomCommand(s, lower, rest, body)
			z.sendPrompt(s)
			return
		}
		// Content channel verb (Phase 8.3): a `gossip`/`newbie` verb defined by a channel_def. Consulted
		// by EXACT match only (no abbreviation — a channel verb never shadows or abbreviates a core verb),
		// AFTER abilities + custom commands. The handler runs the SOURCE publish path (access, rate-limit,
		// sanitize, engine-set author) and publishes to the comms bus; it never releases ownership, so we
		// prompt on return. An empty pack defines no channels => this is always nil => no channel verbs.
		if def := z.channelForVerb(lower); def != nil {
			z.log.Debug("dispatch: channel command", "player", s.character, "verb", lower, "channel", def.ref)
			z.cmdChannel(s, def, rest)
			z.sendPrompt(s)
			return
		}
		z.log.Debug("unknown verb", "player", s.character, "verb", lower)
		s.send(textFrame("Huh?"))
		z.sendPrompt(s)
		return
	}
	z.log.Debug("dispatch", "player", s.character, "verb", lower, "cmd", cmd.Name, "line", line)

	ctx := &Context{z: z, s: s, Actor: s.entity, arg: rest}
	_ = cmd.Run(ctx)

	if ctx.moved {
		// The command (movement) released ownership of s/its entity: an intra-shard
		// transfer handed them to another zone goroutine, or a cross-shard handoff froze
		// the session. This goroutine must not read or write s/its entity again — and the
		// prompt is the destination's job. Returning here keeps single-writer (the
		// slice-1 dispatch early-return invariant, preserved verbatim).
		return
	}
	z.sendPrompt(s)
}

// split returns the first whitespace-delimited word and the trimmed remainder.
func split(line string) (verb, rest string) {
	i := strings.IndexFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimSpace(line[i+1:])
}
