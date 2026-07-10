package world

import (
	"sort"
	"strings"

	"github.com/double-nibble/telosmud/internal/content"
)

// help.go — the builder-defined `help` system (#64). A browsable `help` / `help <topic>` backed by the
// content help_defs table (a topic ref, title, category, body, keyword aliases, and "see also"), with the
// registered built-in command set AUTO-INCLUDED so an empty pack still yields a usable command index.
//
// A help topic is pure CONTENT (like a recipe or a channel): the engine knows the KIND (help_defs) and runs
// the `help` command, but names no topic — a pack supplies the text. The auto-included command listing is the
// one engine-owned part: it is DERIVED from the base command table at render time, honoring the same
// visibility rules dispatch uses (a staff verb above the actor's rank stays invisible; a CmdHidden verb is
// omitted), so the index never leaks a command a mortal cannot see.
//
// Boot-load-only (like recipe_defs): there is no single-ref hot-reload kind for help this slice; a fleet
// reload rebuilds the shard from the whole pack. Single-writer: cmdHelp runs on the zone goroutine (dispatch).

// init registers the `help` verb (#64) into the base command table AFTER it is built. It cannot be registered
// inside registerCommands (like every other verb) because cmdHelp transitively reads baseTable to auto-include
// the command set, which would make baseTable's initializer depend on itself — a package-init cycle. An init()
// runs after all package vars are initialized, so baseTable is complete; the append lands `help` at the lowest
// precedence so it never wins an abbreviation against a movement/look/say verb. `help` is a mortal verb.
func init() { baseTable.register(&Command{Name: "help", Run: cmdHelp}) }

// helpDef is the runtime form of a content HelpDTO. Immutable after build — shared read-only across zone
// goroutines via the registry, exactly like a *recipeDef. keywords is the pre-lowered lookup set (the DTO
// keywords plus the ref's own leaf token, an implicit keyword so a topic is always reachable by-ref-leaf).
type helpDef struct {
	ref      string
	title    string
	category string
	keywords []string // lower-cased lookup words a player types after `help`; ref leaf is implicit
	body     string
	seeAlso  []string
}

// buildHelpDef maps a content HelpDTO onto the runtime helpDef. It lower-cases the declared keywords and adds
// the ref's leaf token (after the last ':') as an implicit keyword, so `help combat` reaches `help:combat`
// even when the author declared no keywords — mirroring the recipe ref-leaf alias rule (#34).
func buildHelpDef(d content.HelpDTO) *helpDef {
	def := &helpDef{
		ref: d.Ref, title: d.Title, category: d.Category, body: d.Body,
		seeAlso: append([]string(nil), d.SeeAlso...),
	}
	seen := map[string]bool{}
	add := func(w string) {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" || seen[w] {
			return
		}
		seen[w] = true
		def.keywords = append(def.keywords, w)
	}
	if leaf := helpRefLeaf(d.Ref); leaf != "" {
		add(leaf)
	}
	for _, k := range d.Keywords {
		add(k)
	}
	return def
}

// helpRefLeaf returns the token after the last ':' of a ref ("help:combat" -> "combat"), or the whole ref
// when it carries no ':'. The implicit by-ref-leaf keyword.
func helpRefLeaf(ref string) string {
	if i := strings.LastIndexByte(ref, ':'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// indexName is how a topic is listed in the `help` index: its title, or the ref leaf when it has no title.
func (d *helpDef) indexName() string {
	if d.title != "" {
		return d.title
	}
	return helpRefLeaf(d.ref)
}

// resolveHelp resolves a typed `help <topic>` argument to a single topic, or nil. Precedence (deterministic,
// ties broken by ref so the result never depends on map iteration order):
//
//  1. an exact REF match ("help:combat");
//  2. an exact KEYWORD match (a declared keyword or the implicit ref leaf);
//  3. a keyword PREFIX match ("comb" -> combat).
//
// The scan is over the (small) topic table on the zone goroutine — no allocation beyond the match set,
// bounded regardless of input. An empty argument returns nil (the caller renders the index instead).
func (z *Zone) resolveHelp(arg string) *helpDef {
	q := strings.ToLower(strings.TrimSpace(arg))
	if q == "" {
		return nil
	}
	table := z.helpDefs().table()
	if d, ok := table[q]; ok { // exact ref
		return d
	}
	var exact, prefix []*helpDef
	for _, d := range table {
		// Scan ALL of a def's keywords before bucketing so an exact match always beats a prefix match,
		// independent of keyword order within the def (a `["combatant","combat"]` def still classifies as
		// exact for query "combat", not prefix).
		var isExact, isPrefix bool
		for _, kw := range d.keywords {
			if kw == q {
				isExact = true
				break
			}
			if strings.HasPrefix(kw, q) {
				isPrefix = true
			}
		}
		switch {
		case isExact:
			exact = append(exact, d)
		case isPrefix:
			prefix = append(prefix, d)
		}
	}
	if len(exact) > 0 {
		return smallestByRef(exact)
	}
	if len(prefix) > 0 {
		return smallestByRef(prefix)
	}
	return nil
}

// smallestByRef returns the def with the lexicographically smallest ref, so a multi-match resolves
// deterministically (map iteration order is random in Go).
func smallestByRef(defs []*helpDef) *helpDef {
	best := defs[0]
	for _, d := range defs[1:] {
		if d.ref < best.ref {
			best = d
		}
	}
	return best
}

// cmdHelp implements `help` (a browsable index) and `help <topic>` (one topic). Topic resolution runs FIRST,
// so a builder-authored topic whose keyword matches a command name shows the rich text rather than the
// minimal auto-entry. A typed argument matching no topic falls back to the auto-included command set: every
// visible built-in command is at least minimally help-addressable. Zone goroutine (dispatch).
func cmdHelp(c *Context) error {
	arg := strings.TrimSpace(c.Rest())
	if arg == "" {
		c.Send(c.z.renderHelpIndex(c.s))
		return nil
	}
	if def := c.z.resolveHelp(arg); def != nil {
		c.Send(renderHelpTopic(def))
		return nil
	}
	if entry, ok := c.z.commandHelpEntry(c.s, arg); ok {
		c.Send(entry)
		return nil
	}
	c.Send("There is no help on '" + arg + "'.  Type 'help' for the index.")
	return nil
}

// renderHelpIndex builds the browsable index: content topics grouped by category (each category and its
// topics sorted so the output is stable), then the auto-included built-in command set the actor can see.
// The command list is DERIVED from the base command table under the same visibility rules dispatch uses, so
// it never lists a staff verb above the actor's rank nor a CmdHidden verb.
func (z *Zone) renderHelpIndex(s *session) string {
	var b strings.Builder
	b.WriteString(colorize("Help", "FG_CYAN"))
	b.WriteString("\n")

	// Content topics, grouped by category (uncategorized => "General").
	if topics := z.helpDefs().table(); len(topics) > 0 {
		byCat := map[string][]*helpDef{}
		for _, d := range topics {
			cat := d.category
			if cat == "" {
				cat = "General"
			}
			byCat[cat] = append(byCat[cat], d)
		}
		cats := make([]string, 0, len(byCat))
		for cat := range byCat {
			cats = append(cats, cat)
		}
		sort.Strings(cats)
		for _, cat := range cats {
			list := byCat[cat]
			sort.Slice(list, func(i, j int) bool { return list[i].ref < list[j].ref })
			b.WriteString("\n")
			b.WriteString(colorize(cat, "FG_YELLOW"))
			b.WriteString("\n")
			for _, d := range list {
				b.WriteString("  ")
				b.WriteString(d.indexName())
				b.WriteString("\n")
			}
		}
	}

	// Auto-included command set (#64): the registered built-in verbs the actor can see.
	if cmds := z.visibleCommands(s); len(cmds) > 0 {
		b.WriteString("\n")
		b.WriteString(colorize("Commands", "FG_YELLOW"))
		b.WriteString("\n")
		b.WriteString(formatCommandColumns(cmds))
	}

	b.WriteString("\nType 'help <topic>' for more.")
	return b.String()
}

// renderHelpTopic renders one topic: its title (falling back to the ref), an inline category tag, the body
// text, and a "see also" line. The body is TRUSTED content and may carry engine {{TOKEN}} color markup —
// it is not a player-supplied string, so it flows through the same color layer as any other content text.
func renderHelpTopic(d *helpDef) string {
	var b strings.Builder
	title := d.title
	if title == "" {
		title = d.ref
	}
	b.WriteString(colorize(title, "FG_CYAN"))
	if d.category != "" {
		b.WriteString("  [" + d.category + "]")
	}
	b.WriteString("\n")
	if d.body != "" {
		b.WriteString("\n")
		b.WriteString(d.body)
	}
	if len(d.seeAlso) > 0 {
		b.WriteString("\n\n")
		b.WriteString(colorize("See also:", "FG_YELLOW"))
		b.WriteString(" ")
		b.WriteString(strings.Join(d.seeAlso, ", "))
	}
	return b.String()
}

// commandHelpEntry returns a minimal help entry for a registered built-in command whose verb the actor typed,
// so every visible command is help-addressable even without an authored topic. It honors the SAME visibility
// rules dispatch uses: a staff verb above the actor's rank, or a CmdHidden verb, resolves to (,"",false) —
// indistinguishable from "no help", so a hidden/gated verb's existence never leaks. The first typed word is
// resolved through the base table (abbreviation-aware), so `help sc` documents `score`.
func (z *Zone) commandHelpEntry(s *session, arg string) (string, bool) {
	verb, _ := split(arg) // arg is already trimmed by cmdHelp; take the first word
	cmd, ok := baseTable.resolve(strings.ToLower(verb))
	if !ok || !z.commandVisible(cmd, s) {
		return "", false
	}
	var b strings.Builder
	b.WriteString(colorize(cmd.Name, "FG_CYAN"))
	if len(cmd.Aliases) > 0 {
		b.WriteString("  (aliases: ")
		b.WriteString(strings.Join(cmd.Aliases, ", "))
		b.WriteString(")")
	}
	b.WriteString("\n\nThis is a built-in command.  No detailed help has been written for it yet.")
	return b.String(), true
}

// visibleCommands returns the canonical names of the base-table commands the actor can see, sorted, honoring
// the CmdHidden + MinRank visibility rules. Derived fresh each call (the table is small and `help` is rare).
func (z *Zone) visibleCommands(s *session) []string {
	var names []string
	for _, cmd := range baseTable.ordered {
		if z.commandVisible(cmd, s) {
			names = append(names, cmd.Name)
		}
	}
	sort.Strings(names)
	return names
}

// commandVisible reports whether the actor may see cmd in help — the shared predicate for the index listing
// and the direct `help <cmd>` entry. A CmdHidden verb is never listed; a staff verb (MinRank > 0) is hidden
// below its rank, mirroring dispatch's trust gate so help discloses exactly what dispatch would run.
func (z *Zone) commandVisible(cmd *Command, s *session) bool {
	if cmd.Flags&CmdHidden != 0 {
		return false
	}
	if cmd.MinRank > 0 && z.trustLadder().rank(s.tier) < cmd.MinRank {
		return false
	}
	return true
}

// formatCommandColumns lays out command names in a fixed-width grid (helpCommandColumns per row), each cell
// padded to the widest name so the columns align. A stable, allocation-bounded render of the (sorted) names.
func formatCommandColumns(names []string) string {
	const cols = helpCommandColumns
	width := 0
	for _, n := range names {
		if len(n) > width {
			width = len(n)
		}
	}
	var b strings.Builder
	for i, n := range names {
		b.WriteString("  ")
		if (i%cols) == cols-1 || i == len(names)-1 {
			b.WriteString(n) // last cell in a row: no trailing pad
			b.WriteString("\n")
			continue
		}
		b.WriteString(n)
		for p := len(n); p < width; p++ {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// helpCommandColumns is how many command names the index lays out per row.
const helpCommandColumns = 6
