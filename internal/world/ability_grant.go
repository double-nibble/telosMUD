package world

import "fmt"

// ability_grant.go — Phase-11.4a ABILITY OWNERSHIP: the per-entity granted-ability set + the grant_ability
// op (the 11.1-deferred grant op, landing with bundles). Content abilities dispatch globally by default
// (anyone may type the verb); an ability that opts INTO ownership (requires_grant) only dispatches/casts
// for an entity that has been GRANTED it — a class/race bundle or a trainer hands it out. This keeps the
// existing universal abilities unchanged (opt-in gate) while letting a class ability be class-only.
// Single-writer: every helper runs on the zone goroutine; the granted set is persisted + COW-safe.

// hasGrantedAbility reports whether entity e has been granted ability ref. Zone-goroutine read.
func hasGrantedAbility(e *Entity, ref string) bool {
	if e == nil || e.living == nil || e.living.granted == nil {
		return false
	}
	return e.living.granted[ref]
}

// grantAbility adds ability ref to entity e's granted set (COW-safe). Idempotent. Zone goroutine only.
func grantAbility(e *Entity, ref string) {
	l := mutableLiving(e) // COW: fork a proto-aliased mob's Living before mutating its granted map
	if l == nil {
		return
	}
	if l.granted == nil {
		l.granted = map[string]bool{}
	}
	l.granted[ref] = true
}

// revokeAbility removes ability ref from entity e's granted set (the inverse — leave a class / lose a
// feat). A no-op if not granted. Zone goroutine only.
func revokeAbility(e *Entity, ref string) {
	l := mutableLiving(e)
	if l == nil || l.granted == nil {
		return
	}
	delete(l.granted, ref)
}

// dumpGrantedAbilities renders the entity's granted ability refs as a fresh slice (a copy, stable-ish
// order is not required — load re-installs each). nil when none granted.
func dumpGrantedAbilities(e *Entity) []string {
	if e == nil || e.living == nil || len(e.living.granted) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.living.granted))
	for ref := range e.living.granted {
		out = append(out, ref)
	}
	return out
}

// opGrantAbility: grant_ability(target, ability) — grant an ability to the target (a class feature, a
// trained skill, a racial ability). An ownership-gated ability (requires_grant) becomes usable only after
// this. The 11.1-deferred grant op, landing now that the ownership model exists.
func opGrantAbility(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("grant_ability: no target")
	}
	if op.ability == "" {
		return fmt.Errorf("grant_ability: no ability")
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	grantAbility(c.target, op.ability)
	return nil
}

// opRevokeAbility: revoke_ability(target, ability) — the inverse grant op.
func opRevokeAbility(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("revoke_ability: no target")
	}
	if op.ability == "" {
		return fmt.Errorf("revoke_ability: no ability")
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	revokeAbility(c.target, op.ability)
	return nil
}
