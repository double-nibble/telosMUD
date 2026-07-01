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

// defaultProfessionCap is the ceiling on learned CAPPED (crafting) professions when content does not define
// the cap attribute (D2: "a cap on crafting professions, e.g. 2"). The cap is CONTENT-CONFIGURABLE via the
// professionCapAttr attribute (so a class/feat can raise it) and only CAPPED professions count — a profession
// bundle marked `uncapped` (gathering/utility) is unlimited (docs/REMAINING.md §4).
const defaultProfessionCap = 2

// professionCapAttr is the content attribute that holds a character's learned-CAPPED-profession ceiling. When
// unset (attr == 0) the engine uses defaultProfessionCap, so a pack that defines nothing keeps the old
// behavior; a pack can register it (and a bundle can modify_attribute_base it) to make the cap vary.
const professionCapAttr = "max_professions"

// hasProfession reports whether entity e has learned profession ref. Zone-goroutine read.
func hasProfession(e *Entity, ref string) bool {
	if e == nil || e.living == nil || e.living.professions == nil {
		return false
	}
	return e.living.professions[ref]
}

// professionIsCapped reports whether profession ref counts against the learned-profession cap. A profession
// bundle (ref == the profession membership ref by convention) marked `uncapped` is a gathering/utility trade
// and does NOT count; anything else — including a profession with no matching bundle def — counts (the safe
// default: an unknown profession is treated as capped so it can't dodge the ceiling). Zone-goroutine read.
func (z *Zone) professionIsCapped(ref string) bool {
	if def := z.bundleDefs().get(ref); def != nil && def.uncapped {
		return false
	}
	return true
}

// professionCap resolves e's ceiling on CAPPED professions: the content attribute professionCapAttr when set
// (>0), else defaultProfessionCap. Zone-goroutine read.
func (z *Zone) professionCap(e *Entity) int {
	if v := int(attr(e, professionCapAttr)); v > 0 {
		return v
	}
	return defaultProfessionCap
}

// cappedProfessionCount counts how many of e's currently-learned professions count against the cap (i.e.
// excludes uncapped gathering/utility trades). Zone-goroutine read.
func (z *Zone) cappedProfessionCount(e *Entity) int {
	if e == nil || e.living == nil {
		return 0
	}
	n := 0
	for ref := range e.living.professions {
		if z.professionIsCapped(ref) {
			n++
		}
	}
	return n
}

// learnProfession records profession ref in e's membership set (COW-safe). Idempotent: re-learning a known
// profession is a no-op. Returns false (and changes nothing) when adding a NEW CAPPED profession would exceed
// the character's cap (professionCap); an UNCAPPED (gathering/utility) profession is never blocked. Load
// restores a saved set directly (character.go), bypassing this — a saved set already passed. Zone goroutine only.
func (z *Zone) learnProfession(e *Entity, ref string) bool {
	l := mutableLiving(e) // COW: fork a proto-aliased entity's Living before mutating its professions map
	if l == nil {
		return false
	}
	if l.professions[ref] { // nil-map read is false — safe
		return true
	}
	if z.professionIsCapped(ref) && z.cappedProfessionCount(e) >= z.professionCap(e) {
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
	if !c.z.learnProfession(c.target, op.profession) {
		c.z.log.Debug("learn_profession: at cap, not enrolled",
			"entity", c.target.short, "profession", op.profession, "cap", c.z.professionCap(c.target))
	}
	return nil
}
