package world

import (
	"sort"
	"strings"
)

// recipe_target.go — #34 CROSS-CONTENT ALIAS / KEYWORD TARGETING, recipes-first slice. A player types
// `craft <name>` and the engine resolves the phrase to a content recipe via the SAME Diku isname/ordinal
// grammar the item/mob resolver uses (targeting.go) — no second targeting dialect. Pairs with the `recipes`
// discovery listing, whose printed names are exactly what a player then types (the discoverability pillar).
// Single-writer: the zone goroutine, reading its own recipe registry.

// recipeDisplayName is the label a discovery listing prints for a recipe: its content Name, or the ref when
// unnamed. It is one of the phrases a player can type back (the ref leaf + every alias also resolve).
func recipeDisplayName(def *recipeDef) string {
	if def.name != "" {
		return def.name
	}
	return def.ref
}

// recipeKeywords is the matchable token set for a recipe (#34): every alias split into words, the display
// name's words, and the ref's leaf split on the ref/word separators. So `craft:leather_vest` with aliases
// ["leather vest","vest"] resolves to any of `craft vest`, `craft leather vest`, `craft leather_vest`. Tokens
// are lower-cased to match parseTargetSpec (which lower-cases the typed words).
func recipeKeywords(def *recipeDef) []string {
	var out []string
	add := func(s string) {
		for _, w := range strings.FieldsFunc(s, isRecipeSep) {
			if w != "" {
				out = append(out, strings.ToLower(w))
			}
		}
	}
	for _, a := range def.aliases {
		add(a)
	}
	add(def.name)
	// The ref leaf (after the last ':') is always an implicit alias so a recipe is craftable-by-ref. Add it
	// both split into words AND as the whole leaf token, so `leather vest` and the literal `leather_vest`
	// both resolve.
	leaf := def.ref
	if i := strings.LastIndexByte(leaf, ':'); i >= 0 {
		leaf = leaf[i+1:]
	}
	add(leaf)
	if leaf != "" {
		out = append(out, strings.ToLower(leaf))
	}
	return out
}

// isRecipeSep splits alias/name/ref text into keyword tokens on whitespace and the ref/word punctuation
// (':', '_', '-'), so an authored `leather_vest` or `craft:leather_vest` tokenizes the same as `leather vest`.
func isRecipeSep(r rune) bool {
	return r == ' ' || r == '\t' || r == ':' || r == '_' || r == '-'
}

// craftableRecipes lists the recipes the actor is eligible to craft — those whose required profession the
// actor is a member of (an unprofessioned/open recipe is always eligible). Sorted by display name (stable,
// deterministic) so the discovery listing and the `N.` ordinal selector agree on order. Zone goroutine.
func (z *Zone) craftableRecipes(actor *Entity) []*recipeDef {
	var out []*recipeDef
	for _, def := range z.recipeDefs().table() {
		if def == nil {
			continue
		}
		if def.profession != "" && !hasProfession(actor, def.profession) {
			continue
		}
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool {
		ni, nj := recipeDisplayName(out[i]), recipeDisplayName(out[j])
		if ni == nj {
			return out[i].ref < out[j].ref
		}
		return ni < nj
	})
	return out
}

// opListRecipes: list_recipes — the `recipes` discovery verb (#34). It prints the recipes the actor can
// craft (their profession's recipes + the open ones), each with the exact names `craft <name>` accepts, so a
// player never has to guess an alias. A mob invoker (no session) is a no-op. Zone goroutine.
func opListRecipes(c *effectCtx, _ *effectOp) error {
	if c.actor == nil {
		return nil
	}
	s, ok := sessionOf(c.actor)
	if !ok {
		return nil
	}
	recipes := c.z.craftableRecipes(c.actor)
	if len(recipes) == 0 {
		s.send(textFrame("You don't know how to craft anything yet."))
		return nil
	}
	var b strings.Builder
	b.WriteString("You can craft:\r\n")
	for _, def := range recipes {
		b.WriteString("  ")
		b.WriteString(recipeDisplayName(def))
		// Show the typeable aliases (deduped against the display name) so `craft <one of these>` is obvious.
		if extra := recipeAliasHints(def); extra != "" {
			b.WriteString(" (")
			b.WriteString(extra)
			b.WriteString(")")
		}
		b.WriteString("\r\n")
	}
	s.send(textFrame(strings.TrimRight(b.String(), "\r\n")))
	return nil
}

// recipeAliasHints is the comma-joined list of a recipe's typeable aliases, minus any that merely repeat the
// display name (case-insensitively) — so the listing reads "Leather Vest (vest)" not "Leather Vest (leather
// vest, vest)". Returns "" when the aliases add nothing beyond the display name.
func recipeAliasHints(def *recipeDef) string {
	name := strings.ToLower(recipeDisplayName(def))
	var hints []string
	seen := map[string]bool{name: true}
	for _, a := range def.aliases {
		la := strings.ToLower(strings.TrimSpace(a))
		if la == "" || seen[la] {
			continue
		}
		seen[la] = true
		hints = append(hints, a)
	}
	return strings.Join(hints, ", ")
}

// resolveRecipe resolves a `craft <name>` argument to a single recipe the actor can craft, applying the Diku
// isname + ordinal grammar (targeting.go) against the craftable set: `craft vest` -> first match, `craft
// 2.vest` -> the 2nd. Returns nil when the argument is empty, matches nothing, or the ordinal is out of
// range — the caller messages the refusal. The candidate order matches craftableRecipes (display-name sort),
// so the `N.` selector is stable. Zone goroutine.
func (z *Zone) resolveRecipe(actor *Entity, arg string) *recipeDef {
	spec := parseTargetSpec(arg)
	if spec.empty() || spec.all {
		// `make` is a single-target verb: an empty phrase or a bare/`all.`-selector (`make all`,
		// `make all.vest`) names no ONE recipe, so refuse rather than silently craft the first match.
		return nil
	}
	var hits []*recipeDef
	for _, def := range z.craftableRecipes(actor) {
		if spec.matchesKeywords(recipeKeywords(def)) {
			hits = append(hits, def)
		}
	}
	switch {
	case spec.index > 0:
		if spec.index <= len(hits) {
			return hits[spec.index-1]
		}
		return nil
	case spec.index < 0: // an explicit "0.x" selector matches nothing
		return nil
	default:
		if len(hits) > 0 {
			return hits[0]
		}
		return nil
	}
}
