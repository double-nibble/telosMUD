package world

import "fmt"

// profession.go — Phase-13.3 PROFESSIONS (docs/PHASE13-PLAN.md §13.3, settled fork #2). A profession is NOT
// a new def table: it is an ordinary BUNDLE (kind="profession", Phase 11.4) whose grant op-list hands out the
// trade's craft verbs (grant_ability) + its skill TRACK (grant_track, Phase 11.2), plus a learn_profession op
// that records MEMBERSHIP in the entity's state.professions set. Membership is what the crafting-ability
// `requires.profession` gate (ability.go checkRequires) and the D2 cap read — the abilities/skill come from
// the reused bundle+track machinery, so the only NEW state a profession needs is this thin membership set.
// Single-writer: every helper runs on the zone goroutine; the set is COW-safe (mutableLiving) + persisted
// (the granted-abilities precedent, ability_grant.go).

// craftProfessionCap is the default ceiling on learned professions (D2: "a cap on crafting professions, e.g.
// 2"). v1 applies it UNIFORMLY to every learned profession. The D2 nuances — gathering/utility professions
// unlimited (a kind split) and making the cap CONTENT-CONFIGURABLE — are a deferred follow-up
// (docs/FOLLOW-UPS.md); the cap is not on 13.3's done-when, only the membership + gate are.
const craftProfessionCap = 2

// hasProfession reports whether entity e has learned profession ref. Zone-goroutine read.
func hasProfession(e *Entity, ref string) bool {
	if e == nil || e.living == nil || e.living.professions == nil {
		return false
	}
	return e.living.professions[ref]
}

// learnProfession records profession ref in e's membership set (COW-safe). Idempotent: re-learning a known
// profession is a no-op that never counts against the cap twice. Returns false (and changes nothing) when
// adding a NEW profession would exceed craftProfessionCap. Zone goroutine only.
func learnProfession(e *Entity, ref string) bool {
	l := mutableLiving(e) // COW: fork a proto-aliased entity's Living before mutating its professions map
	if l == nil {
		return false
	}
	if l.professions[ref] { // nil-map read is false — safe
		return true
	}
	if len(l.professions) >= craftProfessionCap {
		return false
	}
	if l.professions == nil {
		l.professions = map[string]bool{}
	}
	l.professions[ref] = true
	return true
}

// dumpProfessions renders e's learned professions as a fresh slice (a copy; load re-installs each). Order is
// not significant. nil when none — the StateJSON subtree omits it (the backward-compat default).
func dumpProfessions(e *Entity) []string {
	if e == nil || e.living == nil || len(e.living.professions) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.living.professions))
	for ref := range e.living.professions {
		out = append(out, ref)
	}
	return out
}

// opLearnProfession: learn_profession(target, profession) — enroll the target in a profession (the
// membership the requires.profession gate + the D2 cap read). A profession BUNDLE lists this op alongside
// grant_ability (the craft verbs) + grant_track (the skill), so apply_bundle both hands out the trade's
// abilities/skill AND enrolls the entity in one unit. A SOFT refuse at the cap (logged, the op-list
// continues): content gates the actual learning behind a `check` when it wants a hard "you know too many
// trades already" message; the op never aborts a half-applied bundle.
func opLearnProfession(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("learn_profession: no target")
	}
	if op.profession == "" {
		return fmt.Errorf("learn_profession: no profession")
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	if !learnProfession(c.target, op.profession) {
		c.z.log.Debug("learn_profession: at cap, not enrolled",
			"entity", c.target.short, "profession", op.profession, "cap", craftProfessionCap)
	}
	return nil
}
