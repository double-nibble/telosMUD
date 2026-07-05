package world

import (
	"math/rand"
	"testing"
)

// salvage_test.go — Phase 13.4: deconstruction. The done-when: an OWNER disenchants a BOUND source into a
// tradeable mid-tier component (unbound) + a bound top-tier essence (tier-dependent binding, D1), the source
// is consumed, and the yield is a deterministic weighted roll under a seed.

func TestSalvageBoundSourceTierBinding(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity

	// A BOUND steel longsword in hand (a soulbound epic the owner wants to break down).
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	bindItem(sword)
	Move(sword, actor)
	if !isBound(sword) {
		t.Fatal("precondition: the source sword should be bound")
	}

	c := &effectCtx{
		z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral,
		rng: rand.New(rand.NewSource(1)),
	}
	op := &effectOp{kind: "salvage_item", item: "midgaard:obj:sword", table: "disenchant_arms"}
	if err := opSalvageItem(c, op); err != nil {
		t.Fatal(err)
	}

	// The source is consumed even though it was BOUND (owner deconstruction, §1).
	if findHeldByProto(actor, "midgaard:obj:sword") != nil {
		t.Fatal("salvage must consume the bound source item")
	}
	// A tradeable mid-tier component: leather, UNBOUND (feeds the market).
	leather := findHeldByProto(actor, "midgaard:obj:leather")
	if leather == nil {
		t.Fatal("salvage should yield a leather component")
	}
	if isBound(leather) {
		t.Fatal("a mid-tier (uncommon) component must stay UNBOUND/tradeable")
	}
	// A bound top-tier essence (the epic tier's binds flag, D1).
	essence := findHeldByProto(actor, "midgaard:obj:essence")
	if essence == nil {
		t.Fatal("salvage should yield an arcane essence")
	}
	if !isBound(essence) {
		t.Fatal("the top-tier (epic) essence must be BOUND on creation (the no-trade sink, D1)")
	}
}

// TestDisenchantRefusedWithoutProfession: the verb is gated on requires.profession — a non-member is refused
// and the source is untouched.
func TestDisenchantRefusedWithoutProfession(t *testing.T) {
	e := newCmdEnv(t)
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	bindItem(sword)
	Move(sword, e.actor.entity)

	aout, _ := e.run("disenchant")
	if !has(aout, "lack the training") {
		t.Fatalf("disenchant without the profession should be refused; got %v", aout)
	}
	if findHeldByProto(e.actor.entity, "midgaard:obj:sword") == nil {
		t.Fatal("a refused disenchant must not consume the source")
	}
}

// TestDisenchantVerbAfterLearning: once leatherworking is learned, the OBJECT-TARGETED disenchant verb (#38)
// resolves the held item by keyword, consuming the sword and yielding the components (the end-to-end path).
func TestDisenchantVerbAfterLearning(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	bindItem(sword)
	Move(sword, actor)

	applyBundleTo(e.z, actor, "leatherworking")
	e.run("disenchant sword") // #38: name the held item to disenchant

	if findHeldByProto(actor, "midgaard:obj:sword") != nil {
		t.Fatal("disenchant should consume the resolved sword")
	}
	if findHeldByProto(actor, "midgaard:obj:essence") == nil {
		t.Fatal("disenchant should yield an arcane essence")
	}
}

// TestDisenchantNoArgRefuses: the object-targeted verb with no item names nothing — a clean refuse, no consume.
func TestDisenchantNoArgRefuses(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	Move(sword, actor)
	applyBundleTo(e.z, actor, "leatherworking")

	aout, _ := e.run("disenchant")
	if !has(aout, "aren't carrying that") {
		t.Fatalf("bare disenchant should refuse (name no item); got %v", aout)
	}
	if findHeldByProto(actor, "midgaard:obj:sword") == nil {
		t.Fatal("a refused disenchant must not consume anything")
	}
}

// TestDisenchantTagGateRefusesUntagged: the demo verb gates on `tag: salvageable`; an item lacking the tag is
// refused (and untouched), even held by a trained crafter.
func TestDisenchantTagGateRefusesUntagged(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	applyBundleTo(e.z, actor, "leatherworking")
	// A plain torch (no `salvageable` tag) in hand.
	torch := addTestItem(e.z, actor, "a pine torch", []string{"torch"})
	_ = torch

	aout, _ := e.run("disenchant torch")
	if !has(aout, "can't salvage that") {
		t.Fatalf("an untagged item should be refused by the tag gate; got %v", aout)
	}
	if len(actor.contents) == 0 {
		t.Fatal("a tag-refused disenchant must not consume the item")
	}
}

// TestDisenchantBlockedItemRefused: a per-item un-salvageable (no_salvage) block refuses the verb outright,
// ahead of the tag gate.
func TestDisenchantBlockedItemRefused(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	applyBundleTo(e.z, actor, "leatherworking")
	// A salvageable-tagged but explicitly-blocked relic.
	relic := addTestItem(e.z, actor, "a mithril relic", []string{"relic"},
		&ItemMeta{tags: []string{"salvageable"}, noSalvage: true})
	_ = relic

	aout, _ := e.run("disenchant relic")
	if !has(aout, "cannot be salvaged") {
		t.Fatalf("a no_salvage item should be refused; got %v", aout)
	}
	if len(actor.contents) == 0 {
		t.Fatal("a blocked disenchant must not consume the item")
	}
}

// TestDisenchantWornItemRefused: salvage is destructive, so a WORN/wielded source is refused up front (the
// player must `remove` it first — the same guard drop honors, #36) and is NOT consumed.
func TestDisenchantWornItemRefused(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	applyBundleTo(e.z, actor, "leatherworking")
	sword := addTestItem(e.z, actor, "a steel sword", []string{"sword"},
		wearableFor(WearLocWield), &Weapon{diceNum: 1, diceSize: 8, damageType: "slash"},
		&ItemMeta{tags: []string{"salvageable"}})
	e.run("wield sword")

	aout, _ := e.run("disenchant sword")
	if !has(aout, "must remove it") {
		t.Fatalf("disenchanting a wielded item should be refused; got %v", aout)
	}
	if wr, _ := Get[*Wearer](actor); wr.slotOf(sword) == WearLocNone {
		t.Fatal("a refused disenchant must not have unequipped/consumed the worn sword")
	}
}

// TestDisenchantKeptItemRefused: a keep-flagged source is refused (unkeep first) and not consumed.
func TestDisenchantKeptItemRefused(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	applyBundleTo(e.z, actor, "leatherworking")
	relic := addTestItem(e.z, actor, "a prized relic", []string{"relic"},
		&ItemMeta{tags: []string{"salvageable"}})
	keepItem(relic)

	aout, _ := e.run("disenchant relic")
	if !has(aout, "marked keep") {
		t.Fatalf("disenchanting a kept item should be refused; got %v", aout)
	}
	if findHeldByProto(actor, string(relic.proto)) == nil && relic.location != actor {
		t.Fatal("a refused disenchant must not consume the kept item")
	}
}

// TestSalvagePerItemOverrideTable: an item's own salvage_table wins over the op's default table.
func TestSalvagePerItemOverrideTable(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	// An item that overrides the salvage table to disenchant_arms (which yields leather + essence).
	widget := addTestItem(e.z, actor, "a broken widget", []string{"widget"},
		&ItemMeta{tags: []string{"salvageable"}, salvageTable: "disenchant_arms"})

	c := &effectCtx{
		z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral,
		arg: "widget", rng: rand.New(rand.NewSource(1)),
	}
	// The op names NO fixed item (object-target) and a BOGUS default table; the per-item override is used.
	op := &effectOp{kind: "salvage_item", table: "no_such_default", tag: "salvageable"}
	if err := opSalvageItem(c, op); err != nil {
		t.Fatalf("override salvage: %v", err)
	}
	if widget.location == actor { // still held => not consumed
		t.Fatal("the override salvage should consume the widget")
	}
	if findHeldByProto(actor, "midgaard:obj:essence") == nil {
		t.Fatal("the per-item override table (disenchant_arms) should have yielded an essence")
	}
}

// TestSalvageUnknownTableErrors: a salvage op with no resolvable table errors (and does not consume the
// source before it checks).
func TestSalvageUnknownTableErrors(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	// An UNTIERED item (a torch), so the op's (bogus) table is the only source — a rare/tiered item would
	// derive its tier's salvage_table instead (#38 slice B) and never reach the unknown-table path.
	torch := e.z.spawn(ProtoRef("midgaard:obj:torch"))
	Move(torch, actor)

	c := &effectCtx{z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral}
	op := &effectOp{kind: "salvage_item", item: "midgaard:obj:torch", table: "nonexistent"}
	if err := opSalvageItem(c, op); err == nil {
		t.Fatal("salvage_item with an unknown table should error")
	}
	if findHeldByProto(actor, "midgaard:obj:torch") == nil {
		t.Fatal("a salvage that errors on the table must not have consumed the source")
	}
}

// TestSalvageDerivesTableFromTier: with no per-item override and no op table, the yield derives from the
// item's rarity TIER (the demo rare tier -> disenchant_arms) — the #38 slice-B derivation.
func TestSalvageDerivesTableFromTier(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword")) // tier: rare, tagged salvageable
	Move(sword, actor)

	c := &effectCtx{
		z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral,
		arg: "sword", rng: rand.New(rand.NewSource(1)),
	}
	// No fixed item, NO table, NO override — the table must derive from the rare tier's salvage_table.
	op := &effectOp{kind: "salvage_item", tag: "salvageable", skill: "leatherworking"}
	if err := opSalvageItem(c, op); err != nil {
		t.Fatalf("derived salvage: %v", err)
	}
	if findHeldByProto(actor, "midgaard:obj:sword") != nil {
		t.Fatal("the tier-derived salvage should have consumed the sword")
	}
	if findHeldByProto(actor, "midgaard:obj:essence") == nil {
		t.Fatal("the rare tier's derived disenchant_arms table should yield an essence")
	}
}

// TestSalvageSkillGateRefusesLowSkill: the skill requirement scales with the item's LEVEL; below it the actor
// is refused and the item is untouched. Above it, the salvage proceeds.
func TestSalvageSkillGateRefusesLowSkill(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	// A rare sword rolled to LEVEL 5 => min skill 5 (rare base 0 + level 5).
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	Add(sword, &Quality{Level: 5, Affixes: map[string]float64{}})
	Move(sword, actor)

	newCtx := func() *effectCtx {
		return &effectCtx{
			z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral,
			arg: "sword", rng: rand.New(rand.NewSource(1)),
		}
	}
	op := &effectOp{kind: "salvage_item", tag: "salvageable", skill: "leatherworking"}

	// Skill 0 < required 5 => refused, sword untouched.
	setAttrBase(actor, "leatherworking", 0)
	if err := opSalvageItem(newCtx(), op); err != nil {
		t.Fatalf("low-skill salvage: %v", err)
	}
	if findHeldByProto(actor, "midgaard:obj:sword") == nil {
		t.Fatal("a below-requirement salvage must not consume the item")
	}

	// Skill 5 >= required 5 => proceeds.
	setAttrBase(actor, "leatherworking", 5)
	if err := opSalvageItem(newCtx(), op); err != nil {
		t.Fatalf("at-requirement salvage: %v", err)
	}
	if findHeldByProto(actor, "midgaard:obj:sword") != nil {
		t.Fatal("an at-requirement salvage should consume the item")
	}
}

// TestSalvageOverSkillBonus: far exceeding the skill requirement yields BONUS rolls of the table's CHANCE
// filler (a common torch), while the GUARANTEED components (the tradeable leather + the bound epic essence
// SINK) are minted exactly ONCE — over-skill rewards filler without N-multiplying the scarce/bound sink.
func TestSalvageOverSkillBonus(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	setAttrBase(actor, "leatherworking", 30) // deep over-skill => the bonus cap applies

	sword := e.z.spawn(ProtoRef("midgaard:obj:sword")) // rare, level 0 => min skill 0
	Move(sword, actor)

	c := &effectCtx{
		z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral,
		arg: "sword", rng: rand.New(rand.NewSource(1)),
	}
	op := &effectOp{kind: "salvage_item", tag: "salvageable", skill: "leatherworking"}
	if err := opSalvageItem(c, op); err != nil {
		t.Fatalf("over-skill salvage: %v", err)
	}
	// The GUARANTEED bound essence sink is minted once — NOT multiplied by over-skill.
	if got := heldQuantity(actor, "midgaard:obj:essence"); got != 1 {
		t.Fatalf("over-skill essence = %d, want 1 (a guaranteed bound sink must not multiply)", got)
	}
	// The GUARANTEED leather is likewise once.
	if got := heldQuantity(actor, "midgaard:obj:leather"); got != 1 {
		t.Fatalf("over-skill leather = %d, want 1 (guaranteed, not multiplied)", got)
	}
	// The CHANCE filler (torch) IS multiplied: base 1 + the capped bonus passes.
	if got := heldQuantity(actor, "midgaard:obj:torch"); got != 1+maxSalvageBonus {
		t.Fatalf("over-skill filler torches = %d, want %d (base 1 + %d capped bonus rolls)", got, 1+maxSalvageBonus, maxSalvageBonus)
	}
}

// TestSalvageRefuseDoesNotAdvanceSkill: a skill-gated REFUSE suppresses the ability's OnSkillUse, so a player
// can't train the salvaging skill by spamming a disenchant they're too unskilled to complete (#38 review #1).
// A SUCCESSFUL salvage leaves the hook enabled.
func TestSalvageRefuseDoesNotAdvanceSkill(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	Add(sword, &Quality{Level: 5, Affixes: map[string]float64{}}) // rare level 5 => min skill 5
	Move(sword, actor)

	op := &effectOp{kind: "salvage_item", tag: "salvageable", skill: "leatherworking"}

	// Below the requirement: refused, and OnSkillUse is suppressed.
	setAttrBase(actor, "leatherworking", 0)
	cRefuse := &effectCtx{z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral, arg: "sword"}
	if err := opSalvageItem(cRefuse, op); err != nil {
		t.Fatalf("gated salvage: %v", err)
	}
	if !cRefuse.suppressSkillUse {
		t.Fatal("a skill-gated refuse must suppress OnSkillUse (no training past your own gate)")
	}

	// At/above the requirement: succeeds, and the hook is NOT suppressed.
	setAttrBase(actor, "leatherworking", 5)
	cOK := &effectCtx{z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral, arg: "sword", rng: rand.New(rand.NewSource(1))}
	if err := opSalvageItem(cOK, op); err != nil {
		t.Fatalf("successful salvage: %v", err)
	}
	if cOK.suppressSkillUse {
		t.Fatal("a successful salvage should allow OnSkillUse to fire")
	}
}
