package world

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/double-nibble/telosmud/internal/textsan"
)

// aliases.go — per-character command aliases (#353): the Unix-shell-style `alias`/`unalias`
// player convenience. A player defines `alias bc burn corpse`; typing `bc` then behaves exactly
// as `burn corpse`, and `bc altar` as `burn corpse altar` (trailing input appended). The alias map
// is per-character durable state: it survives logout/login (StateJSON.Aliases) and a cross-shard
// handoff (the dedicated snapshot field, handoff.go), mirroring the receiver-side comms-state
// subtree (commsstate.go) one-for-one.
//
// # Where expansion happens (the whole security posture)
//
// Expansion runs at the SPLIT step of dispatch (parser.go), BEFORE verb resolution — exactly the
// phase the parser's own doc comment reserved for it ("alias expansion is a later phase"). The
// consequences of that placement are the feature's safety story:
//
//   - An alias can target ANYTHING the resolver handles (base verbs, ability/custom/channel/toggle
//     verbs) because expansion produces an ordinary command line that is then resolved normally.
//   - It grants NO privilege: the trust-rank gate (Command.MinRank) is evaluated on the RESOLVED
//     verb, AFTER expansion, so a mortal aliasing a token to a staff verb still hits the gate and
//     the verb stays invisible (TestAliasNoPrivilegeEscalation).
//   - The expansion is player-authored text re-fed to the parser — identical to the player having
//     TYPED that line — so it inherits the same untrusted-input discipline (no verb's arguments are
//     ever interpreted as a format string; act.go). We only ever concatenate; never Sprintf the
//     expansion as a format string. The stored expansion is textsan.CleanLine'd at definition, so a
//     control-rune-laden expansion can never be smuggled past the world's ingress boundary.
//
// # Cycle / recursion safety
//
// An alias whose expansion begins with another alias (or itself — `alias look look`, or a 2-cycle
// `alias a b` / `alias b a`) must not loop. expandAlias bounds the expansion two ways: a VISITED set
// (each alias key expands at most once per line — the real terminator, so a cycle stops the first
// time it revisits a key) and a hard depth cap (aliasMaxDepth, a backstop). On a cycle/cap the loop
// stops and the line resolves with whatever verb it currently holds (a self/cyclic alias thus
// degrades to a normal resolve, never a hang).

const (
	// aliasMaxCount caps how many aliases a character may define — the size guard on this durable,
	// player-authored, open-ended collection (the commsIgnoreMaxIDs / tellCursorMaxSenders precedent).
	aliasMaxCount = 64
	// aliasNameMaxRunes bounds an alias NAME (the verb token). A verb is a single short word; this keeps
	// the durable key bounded and rejects a pathologically long name.
	aliasNameMaxRunes = 32
	// aliasBodyMaxRunes bounds an alias EXPANSION. Comfortably fits a multi-word command while keeping the
	// durable value bounded (a player cannot grow the state subtree without bound).
	aliasBodyMaxRunes = 256
	// aliasMaxDepth is the hard backstop on expansion iterations for one input line. The VISITED set in
	// expandAlias is the real cycle terminator (each alias key expands at most once, so ≤ aliasMaxCount
	// iterations for any acyclic chain); this cap is set above aliasMaxCount so a legitimate long chain
	// always completes and only a bug could ever trip it.
	aliasMaxDepth = aliasMaxCount + 1
)

// reservedAliasNames are verbs an alias may NOT shadow. Only the alias-management verbs themselves are
// reserved: they must always be typeable so a player can never brick their ability to remove an alias
// they defined. Every OTHER verb MAY be shadowed (Unix-style, the player's choice, always recoverable
// via `unalias`); expansion running before resolution is what makes that shadowing work.
var reservedAliasNames = map[string]struct{}{
	"alias":   {},
	"unalias": {},
}

// aliasState is the in-memory per-character alias map, owned by the zone goroutine that owns the
// session (single-writer, like commsState) — no locks. nil on a session until first touched
// (loadAliasState / a define lazily creates it); a nil state has no aliases.
type aliasState struct {
	// m maps a lowercased verb token to its expansion (a full command prefix). Lowercased keys mirror
	// the case-insensitive verb resolution in dispatch, so `BC` and `bc` are the same alias.
	m map[string]string
}

func newAliasState() *aliasState { return &aliasState{m: map[string]string{}} }

// aliasesOf returns the session's alias state, lazily creating an empty one. Zone goroutine.
func aliasesOf(s *session) *aliasState {
	if s.aliases == nil {
		s.aliases = newAliasState()
	}
	return s.aliases
}

// expandAlias applies the player's alias map to a split (verb, rest) at the dispatch split step,
// returning the possibly-rewritten (verb, rest) and whether any expansion happened. verb is expected
// already lowercased (as dispatch passes it). Trailing input is appended to the expansion Unix-style
// (`bc altar` with bc="burn corpse" -> "burn corpse altar"). A VISITED set makes each alias key expand
// at most once (so a self/cyclic alias terminates), and a hard depth cap backs that up. The reconstructed
// line is re-capped + control-stripped (textsan.CleanLine) so an expansion can never exceed the
// MaxLineBytes ingress cap a typed line gets. Pure CPU over zone-owned session state; zone goroutine, no
// allocation on the no-alias fast path.
func (z *Zone) expandAlias(s *session, verb, rest string) (string, string, bool) {
	if s == nil || s.aliases == nil || len(s.aliases.m) == 0 {
		return verb, rest, false
	}
	m := s.aliases.m
	expanded := false
	var seen map[string]struct{}
	for depth := 0; depth < aliasMaxDepth; depth++ {
		body, ok := m[verb]
		if !ok {
			break // the current verb is not an alias: expansion is complete
		}
		if _, cycle := seen[verb]; cycle {
			break // revisiting an already-expanded key: a cycle — stop, resolve verb as-is
		}
		if seen == nil {
			seen = make(map[string]struct{}, 4)
		}
		seen[verb] = struct{}{}
		line := body
		if rest != "" {
			line = body + " " + rest // Unix-style: append the trailing argument tail to the expansion
		}
		nv, nr := split(line)
		verb, rest = strings.ToLower(nv), nr
		expanded = true
	}
	if expanded {
		// Restore the ingress MaxLineBytes invariant (security review, #353): a pathological chained alias
		// set (up to aliasMaxCount bodies, each up to aliasBodyMaxRunes, feeding one another) can reconstruct
		// a line LONGER than the 4096-byte cap every TYPED line already gets at world ingress (server.go), and
		// downstream handlers assume that cap. Re-cap + control-strip the reconstructed line exactly as ingress
		// treats typed input, then re-split — so an alias can never turn a 2-byte keystroke into an oversized
		// per-occupant broadcast. A no-op (unallocated) on the common in-bounds line.
		verb, rest = split(textsan.CleanLine(verb + " " + rest))
		verb = strings.ToLower(verb)
	}
	return verb, rest, expanded
}

// --- the commands ---------------------------------------------------------------------------

// aliasCommands returns the `alias` / `unalias` verbs. Registered LOW-priority (with the other QoL
// commands) so they never shadow or abbreviate a movement/look/say verb — and they carry no single-
// letter alias for the same reason. Both are RESERVED names (reservedAliasNames), so an alias can
// never be defined over them; a player can therefore always manage their aliases.
func aliasCommands() []*Command {
	return []*Command{
		{Name: "alias", Run: cmdAlias},
		{Name: "unalias", Run: cmdUnalias},
	}
}

// cmdAlias lists, queries, or defines a command alias (#353):
//
//	alias                      — list every defined alias (sorted).
//	alias <name>               — show one alias's expansion (or report it is undefined).
//	alias <name> <expansion…>  — define/redefine <name> to expand to <expansion> + trailing input.
//
// Defining validates: a non-reserved single-token name within length, a non-empty expansion within
// length, and the per-character count cap (a NEW name at cap is refused; redefining an existing one is
// always allowed). The stored expansion is textsan.CleanLine'd (the world-ingress control-strip + cap),
// so it is safe to re-feed to the parser. Zone goroutine (mutates zone-owned session state).
func cmdAlias(c *Context) error {
	s := c.s
	name := strings.ToLower(strings.TrimSpace(c.Arg(0)))
	if name == "" {
		// Bare `alias`: list current aliases.
		listAliases(c)
		return nil
	}
	// The expansion is everything after the name token. Rest() is the full tail after `alias`; strip the
	// leading name to get the body (preserving the body's internal spacing verbatim).
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Rest()), c.Arg(0)))
	if body == "" {
		// `alias <name>`: query one alias.
		if s.aliases != nil {
			if exp, ok := s.aliases.m[name]; ok {
				c.Send("alias " + name + " " + exp)
				return nil
			}
		}
		c.Send("You have no alias '" + name + "'.")
		return nil
	}
	// --- define / redefine ---
	if _, reserved := reservedAliasNames[name]; reserved {
		c.Send("You can't alias '" + name + "' — it is reserved.")
		return nil
	}
	if utf8.RuneCountInString(name) > aliasNameMaxRunes {
		c.Send("That alias name is too long.")
		return nil
	}
	exp := textsan.CleanLine(body)
	if exp == "" {
		// The body was entirely control runes stripped away by CleanLine.
		c.Send("That alias expansion is empty.")
		return nil
	}
	if utf8.RuneCountInString(exp) > aliasBodyMaxRunes {
		c.Send("That alias expansion is too long.")
		return nil
	}
	as := aliasesOf(s)
	if _, exists := as.m[name]; !exists && len(as.m) >= aliasMaxCount {
		c.Send("You have too many aliases (limit " + strconv.Itoa(aliasMaxCount) + "). Remove one with `unalias`.")
		return nil
	}
	as.m[name] = exp
	c.Send("Alias set: " + name + " -> " + exp)
	return nil
}

// cmdUnalias removes one alias (`unalias <name>`). Reports whether it existed. Zone goroutine.
func cmdUnalias(c *Context) error {
	s := c.s
	name := strings.ToLower(strings.TrimSpace(c.Arg(0)))
	if name == "" {
		c.Send("Usage: unalias <name>")
		return nil
	}
	if s.aliases == nil {
		c.Send("You have no alias '" + name + "'.")
		return nil
	}
	if _, ok := s.aliases.m[name]; !ok {
		c.Send("You have no alias '" + name + "'.")
		return nil
	}
	delete(s.aliases.m, name)
	c.Send("Alias removed: " + name)
	return nil
}

// listAliases sends the player their aliases in a stable sorted order (or a friendly note when none).
func listAliases(c *Context) {
	s := c.s
	if s.aliases == nil || len(s.aliases.m) == 0 {
		c.Send("You have no aliases defined.")
		return
	}
	names := make([]string, 0, len(s.aliases.m))
	for name := range s.aliases.m {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("Your aliases:")
	for _, name := range names {
		b.WriteString("\n  ")
		b.WriteString(name)
		b.WriteString(" -> ")
		b.WriteString(s.aliases.m[name])
	}
	c.Send(b.String())
}

// --- dump / load (the StateJSON.Aliases subtree) --------------------------------------------

// dumpAliasState renders the session's alias map into its durable form, or nil when empty (a player
// with no aliases writes no subtree — the omitempty precedent). Bounded by aliasMaxCount. Runs on the
// zone goroutine; copies into a fresh map so the saver never aliases live session state.
func dumpAliasState(s *session) map[string]string {
	if s == nil || s.aliases == nil || len(s.aliases.m) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.aliases.m))
	for name, exp := range s.aliases.m {
		if len(out) >= aliasMaxCount {
			break
		}
		out[name] = exp
	}
	return out
}

// loadAliasState installs a persisted alias map onto the session (#353). A nil/empty map installs the
// empty state (no aliases). Bounded + re-sanitized on the way in (the durable/handoff producer is
// trusted — the state_json is signed — but a bounded, control-stripped install is cheap defense-in-
// depth against a forged/corrupt snapshot; the commsstate load precedent). Runs on the zone goroutine.
func loadAliasState(s *session, m map[string]string) {
	if s == nil || len(m) == 0 {
		return
	}
	as := newAliasState()
	for name, exp := range m {
		if len(as.m) >= aliasMaxCount {
			break
		}
		// CleanLine the NAME symmetrically with the body (security review, #353): the define path gets the
		// name control-stripped for free (it comes from an ingress-CleanLine'd typed line), but a forged/
		// corrupt snapshot could carry a control-laden key — strip it here so the guard is symmetric.
		name = strings.ToLower(strings.TrimSpace(textsan.CleanLine(name)))
		exp = textsan.CleanLine(strings.TrimSpace(exp))
		if name == "" || exp == "" {
			continue
		}
		if _, reserved := reservedAliasNames[name]; reserved {
			continue // a reserved name can never be a live alias; drop a forged one
		}
		if utf8.RuneCountInString(name) > aliasNameMaxRunes || utf8.RuneCountInString(exp) > aliasBodyMaxRunes {
			continue
		}
		as.m[name] = exp
	}
	s.aliases = as
}

// --- handoff carry (the dedicated snapshot field, handoff-transparency) ---------------------

// dumpAliasStateJSON marshals the session's alias map to the JSON string carried on the handoff
// snapshot (handoff.go aliases field), or "" when empty. Reuses dumpAliasState so the handoff form and
// the durable form are byte-identical (one shape). Zone goroutine.
func dumpAliasStateJSON(s *session) string {
	m := dumpAliasState(s)
	if len(m) == 0 {
		return ""
	}
	b, err := marshalAliasState(m)
	if err != nil {
		return ""
	}
	return b
}

// loadAliasStateJSON installs alias state carried on a handoff snapshot onto a pending/destination
// session. An empty string (no aliases / a pre-#353 snapshot) installs nothing. Zone goroutine.
func loadAliasStateJSON(s *session, raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	m, err := unmarshalAliasState(raw)
	if err != nil || len(m) == 0 {
		return
	}
	loadAliasState(s, m)
}
