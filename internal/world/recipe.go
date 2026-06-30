package world

import (
	"fmt"

	"github.com/double-nibble/telosmud/internal/content"
)

// recipe.go — Phase-13.5 CRAFTING RECIPES (docs/PHASE13-PLAN.md §13.5): the data a `craft` ability runs.
// A recipe is content (recipe_defs): a profession + skill gate, an optional STATION (a room flag, D3),
// component inputs, and an item output (+ a coarse quality band). craft_recipe(recipe) validates the gates,
// consumes the inputs, and produces the output — closing the §9 material loop (salvage feeds recipes).
// Single-writer: the zone goroutine, like every effect op. The rich affix roll stays §10-deferred; output
// quality is a coarse band (QualityBase + the crafter's skill level).

// recipeDef is the runtime form of a content RecipeDTO.
type recipeDef struct {
	ref         string
	profession  string // required profession membership ("" = none)
	skill       string // skill LEVEL attribute gating + scaling ("" = no skill gate)
	minSkill    int    // minimum skill level required
	station     string // required room flag, D3 ("" = craft anywhere)
	inputs      []recipeInput
	output      recipeOutput
	qualityBase int
}

type recipeInput struct {
	item string
	qty  int
}

type recipeOutput struct {
	item string
	qty  int
	bind string // "bound" => the crafted item is soulbound on creation
}

// buildRecipeDef maps a content RecipeDTO onto the runtime recipeDef (qty defaults to 1).
func buildRecipeDef(d content.RecipeDTO) *recipeDef {
	def := &recipeDef{
		ref: d.Ref, profession: d.Profession, skill: d.Skill, minSkill: d.MinSkill,
		station: d.Station, qualityBase: d.QualityBase,
		output: recipeOutput{item: d.Output.Item, qty: max1(d.Output.Qty), bind: d.Output.Bind},
	}
	for _, in := range d.Inputs {
		def.inputs = append(def.inputs, recipeInput{item: in.Item, qty: max1(in.Qty)})
	}
	return def
}

// max1 clamps a quantity to at least 1 (an omitted/zero qty means one).
func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// opCraftRecipe: craft_recipe(recipe) — run a content recipe for the actor. It re-validates every gate
// (profession membership, skill level, station room flag — the calling ability's requires gate is the first
// line, this is the can't-bypass backstop), checks the actor holds all inputs, consumes them, and produces
// the output with a coarse skill-scaled quality. Any failed gate is a clean refuse (a player message + no
// consume); a missing recipe/input is an error.
func opCraftRecipe(c *effectCtx, op *effectOp) error {
	if c.actor == nil {
		return fmt.Errorf("craft_recipe: no actor")
	}
	if op.recipe == "" {
		return fmt.Errorf("craft_recipe: no recipe")
	}
	def := c.z.recipeDefs().get(op.recipe)
	if def == nil {
		return fmt.Errorf("craft_recipe: unknown recipe %q", op.recipe)
	}
	if !guardCrossPlayerWrite(c, c.actor) {
		return nil
	}
	// Gate: profession membership.
	if def.profession != "" && !hasProfession(c.actor, def.profession) {
		craftRefuse(c.actor, "You lack the training for that craft.")
		return nil
	}
	// Gate: skill level.
	if def.skill != "" && def.minSkill > 0 && int(attr(c.actor, def.skill)) < def.minSkill {
		craftRefuse(c.actor, "Your skill is not yet equal to that recipe.")
		return nil
	}
	// Gate: station (D3) — a required room flag on the actor's current room.
	if def.station != "" && !roomFlag(c.actor.location, def.station) {
		craftRefuse(c.actor, "You need to be at the proper station to craft that.")
		return nil
	}
	// Validate inputs BEFORE consuming any (all-or-nothing — a partial craft never destroys components).
	for _, in := range def.inputs {
		if heldQuantity(c.actor, in.item) < in.qty {
			craftRefuse(c.actor, "You don't have the components for that.")
			return nil
		}
	}
	// Consume the inputs, then produce the output.
	for _, in := range def.inputs {
		if err := consumeQuantity(c.actor, in.item, in.qty); err != nil {
			return fmt.Errorf("craft_recipe %s: %w", def.ref, err)
		}
	}
	c.z.produceRecipeOutput(c.actor, def)
	return nil
}

// craftRefuse sends a refusal line to a player actor (a mob crafter gets nothing — no session).
func craftRefuse(actor *Entity, msg string) {
	if s, ok := sessionOf(actor); ok {
		s.send(textFrame(msg))
	}
}

// consumeQuantity destroys `qty` of prototype ref from e's inventory (stack-aware, spanning stacks), the
// shared consume the craft + salvage paths reuse. The caller must have pre-validated the quantity.
func consumeQuantity(e *Entity, ref string, qty int) error {
	if heldQuantity(e, ref) < qty {
		return fmt.Errorf("consume %s: holds < %d", ref, qty)
	}
	for qty > 0 {
		it := findHeldByProto(e, ref)
		if it == nil {
			return fmt.Errorf("consume %s: ran out mid-consume", ref)
		}
		if isMaterial(it) {
			if have := itemStackCount(it); have > qty {
				setItemStackCount(it, have-qty)
				return nil
			}
			qty -= itemStackCount(it)
		} else {
			qty--
		}
		Move(it, nil)
	}
	return nil
}

// produceRecipeOutput spawns the recipe's output into the actor's inventory, stamping a coarse quality band
// (QualityBase + the crafter's skill level — a better smith makes better gear) and the optional bind
// override. A material output merges into a held stack like a pickup. Zone goroutine.
func (z *Zone) produceRecipeOutput(actor *Entity, def *recipeDef) {
	level := def.qualityBase
	if def.skill != "" {
		level += int(attr(actor, def.skill))
	}
	for i := 0; i < def.output.qty; i++ {
		item := z.spawn(ProtoRef(def.output.item))
		if item == nil {
			return
		}
		if level > 0 {
			Add(item, &Quality{Level: level, Affixes: map[string]float64{}})
		}
		if def.output.bind == "bound" {
			bindItem(item)
		}
		Move(item, actor)
		if isMaterial(item) && mergeStackInto(actor, item) {
			actor.removeContent(item)
		}
		if s, ok := sessionOf(actor); ok {
			s.send(textFrame("You craft " + itemName(item) + "."))
		}
	}
}
