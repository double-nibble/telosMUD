package world

import (
	"strings"
	"testing"
)

// recipe_target_test.go — #34 cross-content alias targeting, recipes-first slice. Done-when: a player types
// `make <alias>` and the engine resolves the phrase to the right recipe via the Diku isname/ordinal grammar,
// profession-gated to what they can craft, and the `recipes` discovery verb prints the exact typeable names.

// TestResolveRecipeByAlias: the demo leather-vest recipe resolves by each of its typeable phrases (display
// name, an explicit alias, and the ref leaf), and a non-matching / empty phrase resolves to nothing.
func TestResolveRecipeByAlias(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	applyBundleTo(e.z, actor, "leatherworking")

	cases := []struct {
		arg  string
		want string // recipe ref, or "" for no match
	}{
		{"vest", "craft:leather_vest"},         // an explicit alias
		{"leather vest", "craft:leather_vest"}, // a multi-word alias (isname on both words)
		{"leather", "craft:leather_vest"},      // a prefix of the multi-word alias
		{"leather_vest", "craft:leather_vest"}, // the ref leaf (implicit alias), tokenized on '_'
		{"ves", "craft:leather_vest"},          // isname prefix match
		{"sword", ""},                          // matches nothing
		{"", ""},                               // empty argument
		{"all", ""},                            // bare `all` names no ONE recipe -> refuse (not silent first)
		{"all.vest", ""},                       // an `all.` selector likewise refuses on a single-target verb
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			def := e.z.resolveRecipe(actor, tc.arg)
			got := ""
			if def != nil {
				got = def.ref
			}
			if got != tc.want {
				t.Fatalf("resolveRecipe(%q) = %q, want %q", tc.arg, got, tc.want)
			}
		})
	}
}

// TestResolveRecipeProfessionGated: a recipe the actor lacks the profession for is neither craftable-listed
// nor resolvable — a non-leatherworker can't `make vest` even by typing the exact name.
func TestResolveRecipeProfessionGated(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity // no leatherworking membership

	if def := e.z.resolveRecipe(actor, "vest"); def != nil {
		t.Fatalf("a non-leatherworker resolved a leatherworking recipe: %q", def.ref)
	}
	for _, def := range e.z.craftableRecipes(actor) {
		if def.ref == "craft:leather_vest" {
			t.Fatal("the leather-vest recipe should not be craftable without the profession")
		}
	}
}

// TestResolveRecipeOrdinal: when two craftable recipes share an alias, the bare phrase takes the first (by
// the deterministic display-name sort) and the `N.` selector reaches the Nth.
func TestResolveRecipeOrdinal(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	// Two open (unprofessioned) recipes both aliased "widget"; display-name sort puts "Widget Alpha" first.
	e.z.recipeDefs().register("test:widget_a", &recipeDef{ref: "test:widget_a", name: "Widget Alpha", aliases: []string{"widget"}})
	e.z.recipeDefs().register("test:widget_b", &recipeDef{ref: "test:widget_b", name: "Widget Beta", aliases: []string{"widget"}})

	if def := e.z.resolveRecipe(actor, "widget"); def == nil || def.ref != "test:widget_a" {
		t.Fatalf("bare `widget` = %v, want test:widget_a (first by sort)", def)
	}
	if def := e.z.resolveRecipe(actor, "2.widget"); def == nil || def.ref != "test:widget_b" {
		t.Fatalf("`2.widget` = %v, want test:widget_b", def)
	}
	if def := e.z.resolveRecipe(actor, "3.widget"); def != nil {
		t.Fatalf("`3.widget` = %v, want nil (out of range)", def)
	}
}

// TestMakeByNameCraftsResolvedRecipe: the name-resolved `make <alias>` verb runs the resolved recipe end to
// end — at the forge, with the profession + components, `make vest` produces the vest and consumes the inputs.
func TestMakeByNameCraftsResolvedRecipe(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	Move(actor, e.z.rooms["midgaard:room:smithy"])
	applyBundleTo(e.z, actor, "leatherworking")
	giveComponents(e.z, actor)

	e.run("make vest")

	if vestInInventory(actor) == nil {
		t.Fatal("`make vest` should resolve craft:leather_vest and produce the vest")
	}
	if _, leather := leatherStacks(actor); leather != 0 {
		t.Fatalf("the resolved craft must consume all 3 leather: %d left", leather)
	}
}

// TestMakeByNameUnknownRefused: `make <phrase>` that matches no craftable recipe is a clean refuse — no
// output, no consumed inputs, and a message.
func TestMakeByNameUnknownRefused(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	Move(actor, e.z.rooms["midgaard:room:smithy"])
	applyBundleTo(e.z, actor, "leatherworking")
	giveComponents(e.z, actor)

	out, _ := e.run("make dragonplate")

	if vestInInventory(actor) != nil {
		t.Fatal("an unresolved `make` must not craft anything")
	}
	if _, leather := leatherStacks(actor); leather != 3 {
		t.Fatalf("a refused craft must not consume inputs: leather = %d, want 3", leather)
	}
	if !strings.Contains(strings.ToLower(strings.Join(out, "\n")), "craft") {
		t.Fatalf("expected a refusal message, got %q", out)
	}
}

// TestRecipesDiscoveryLists: the `recipes` discovery verb prints the recipes the actor can craft with their
// exact typeable names (display name + alias), and hides recipes they can't craft.
func TestRecipesDiscoveryLists(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	applyBundleTo(e.z, actor, "leatherworking")

	out, _ := e.run("recipes")

	if !strings.Contains(strings.Join(out, "\n"), "Leather Vest") {
		t.Fatalf("recipes listing should name the leather vest, got %q", out)
	}
	if !strings.Contains(strings.ToLower(strings.Join(out, "\n")), "vest") {
		t.Fatalf("recipes listing should print the typeable alias, got %q", out)
	}
}

// TestRecipesDiscoveryEmptyWithoutProfession: with no profession, the discovery verb reports nothing to craft.
func TestRecipesDiscoveryEmptyWithoutProfession(t *testing.T) {
	e := newCmdEnv(t)
	out, _ := e.run("recipes")
	if !strings.Contains(strings.ToLower(strings.Join(out, "\n")), "craft anything") {
		t.Fatalf("expected an empty-recipes message, got %q", out)
	}
}
