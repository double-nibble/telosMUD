package world

import (
	"reflect"

	"github.com/double-nibble/telosmud/internal/content"
)

// The DTO -> component mapper (docs/PHASE4-PLAN.md D5). The content package owns the on-disk
// transfer structs (content.*DTO); THIS file owns the explicit translation onto the runtime
// *Room/*Physical/*Wearable/*Weapon/*Container component structs. Keeping the mapper here
// (and the json tags only on the DTOs) means the world component layout is never frozen to a
// persistence format, and the world package is the sole place a *Prototype is constructed —
// the loader calls protoCache.define (build.go), and define's component set comes from here.
//
// These builders mirror the old defineTorch/defineHelmet/defineSword/defineChest exactly:
// each adds only the components the DTO carries (a nil DTO pointer => component absent), so a
// prototype's component set is byte-identical to the hand-authored one. The parity test
// (content_parity_test.go) is the guard.

// roomComponents builds the component template for a room prototype: a *Room whose exits map
// is populated from the DTO at authoring time (immutable thereafter; an instance that
// re-routes an exit COWs via mutableRoom). Mirrors the old defineRoom.
func roomComponents(r content.RoomDTO) componentSet {
	exits := make(map[string]ProtoRef, len(r.Exits))
	for dir, to := range r.Exits {
		exits[dir] = ProtoRef(to)
	}
	room := &Room{exits: exits, sector: r.Sector}
	return componentSet{reflect.TypeFor[*Room](): room}
}

// protoComponents builds the component template for an item/mob prototype from the present
// DTO sub-structs. Only non-nil components are added, exactly as the old define* helpers did.
func protoComponents(p content.ProtoDTO) componentSet {
	comps := componentSet{}
	if d := p.Physical; d != nil {
		comps[reflect.TypeFor[*Physical]()] = &Physical{
			weight: d.Weight, size: d.Size, material: d.Material,
		}
	}
	if d := p.Wearable; d != nil {
		comps[reflect.TypeFor[*Wearable]()] = wearableFromNames(d.Locations)
	}
	if d := p.Weapon; d != nil {
		comps[reflect.TypeFor[*Weapon]()] = &Weapon{
			diceNum: d.DiceNum, diceSize: d.DiceSize, damageType: d.DamageType,
			class: d.Class, attackVerb: d.AttackVerb,
		}
	}
	if d := p.Container; d != nil {
		comps[reflect.TypeFor[*Container]()] = &Container{
			capacity: d.Capacity, weightLimit: d.WeightLimit,
			closed: d.Closed, locked: d.Locked, keyRef: ProtoRef(d.KeyRef),
		}
	}
	return comps
}

// wearLocByName resolves a content wear-location NAME to the internal WearLoc slot. The names
// are the human labels (the inverse of wearLocName), so content authors never see the enum.
var wearLocByName = map[string]WearLoc{
	"head":    WearLocHead,
	"body":    WearLocBody,
	"hands":   WearLocHands,
	"feet":    WearLocFeet,
	"wield":   WearLocWield,
	"wielded": WearLocWield, // accept the display label too
	"hold":    WearLocHold,
	"held":    WearLocHold,
}

// wearableFromNames builds a *Wearable advertising exactly the named slots. An unknown name
// is ignored (content-lint would flag it); the demo uses only "head" and "wield".
func wearableFromNames(names []string) *Wearable {
	var locs []WearLoc
	for _, n := range names {
		if loc, ok := wearLocByName[n]; ok {
			locs = append(locs, loc)
		}
	}
	return wearableFor(locs...)
}
