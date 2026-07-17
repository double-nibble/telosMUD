package world

import (
	"fmt"

	"github.com/double-nibble/telosmud/internal/content"
)

// track.go — the Phase-11.2 ADVANCEMENT-TRACK machinery (docs/PHASE11-PLAN.md §11.2, gap [G6a]): the
// union abstraction for every progression mode. A track is content (track_defs): a progress attribute,
// ascending thresholds that mark each step, and a per-step grant op-list. An entity that HAS a track
// stores its current step in instance state (Living.tracks, persisted). The machinery is two ops:
//
//   - grant_track(track)      — add the track to an entity (multiclass / join-a-guild / a chargen bundle).
//   - advance_track(track, by) — feed progress: raise the progress attribute, then apply the grant op-list
//                                of every newly-crossed step. The "which event feeds the track" of the gap
//                                analysis is just which handler calls advance_track (OnKill→xp, OnSkillUse→
//                                a skill track, a trainer ability, a spend-points ability — all content).
//
// `level` is never an engine concept: a track's optional level_attr just NAMES which attribute (if any) a
// step raises, so 11.3 can fire OnLevel vs OnTrackStep; a use-based track has no level_attr at all.
// Single-writer: every op runs on the zone goroutine.

// trackDef is the runtime form of a content TrackDTO: the progress attr, the optional level attr, the
// ascending thresholds, and the parsed per-step grant op-lists (steps[i] runs when step i+1 is reached).
type trackDef struct {
	ref          string
	progressAttr string
	levelAttr    string // "" => not a level track (use-based / level-less)
	thresholds   []float64
	steps        [][]effectOp
}

// buildTrackDef maps a content.TrackDTO onto the runtime trackDef, parsing each step's grant op-list. A
// parse error returns the partially-built def + the error (registered with whatever parsed — content-lint
// is the real gate, mirroring buildAbilityDef).
func buildTrackDef(t content.TrackDTO) (*trackDef, error) {
	def := &trackDef{
		ref:          t.Ref,
		progressAttr: t.ProgressAttr,
		levelAttr:    t.LevelAttr,
		thresholds:   append([]float64(nil), t.Thresholds...),
	}
	for i, raw := range t.Steps {
		ops, err := parseOpList(raw)
		if err != nil {
			return def, fmt.Errorf("track %s step %d: %w", t.Ref, i+1, err)
		}
		def.steps = append(def.steps, ops)
	}
	return def, nil
}

// --- entity track state (Living.tracks) --------------------------------------------------------

// trackStep returns entity e's current step on track ref (0 = not granted / step 0). Zone-goroutine read.
func trackStep(e *Entity, ref string) int {
	if e == nil || e.living == nil || e.living.tracks == nil {
		return 0
	}
	return e.living.tracks[ref]
}

// hasTrack reports whether entity e has been granted track ref (present in its track set, any step).
func hasTrack(e *Entity, ref string) bool {
	if e == nil || e.living == nil || e.living.tracks == nil {
		return false
	}
	_, ok := e.living.tracks[ref]
	return ok
}

// setTrackStep records entity e's current step on track ref (COW-safe). Single-writer: zone goroutine.
func setTrackStep(e *Entity, ref string, step int) {
	l := mutableLiving(e) // COW: fork a proto-aliased mob's Living before mutating its tracks map
	if l == nil {
		return
	}
	if l.tracks == nil {
		l.tracks = map[string]int{}
	}
	l.tracks[ref] = step
}

// --- the ops -----------------------------------------------------------------------------------

// opGrantTrack: grant_track(target, track) — add a track to the target at its starting step (0). Idempotent:
// re-granting an already-held track is a no-op (it does not reset progress — multiclass / a reload-safe
// bundle re-apply must not wipe a level). This is the Phase-11.1-deferred grant op, landing with tracks.
func opGrantTrack(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("grant_track: no target")
	}
	if op.track == "" {
		return fmt.Errorf("grant_track: no track")
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	if hasTrack(c.target, op.track) {
		return nil // already granted: never reset progress
	}
	setTrackStep(c.target, op.track, 0)
	return nil
}

// opAdvanceTrack: advance_track(target, track, amount) — feed `amount` progress into the track. It raises
// the track's progress attribute base by amount, then applies the grant op-list of EVERY step whose
// threshold the new progress crosses (so a big XP award can jump several levels at once), advancing the
// entity's stored step. Auto-grants the track on first advance (a use-based skill track need not be
// explicitly granted). Each step's grants run ONCE — the stored step is the high-water, so a reload (which
// restores the step, not re-runs advance) never re-applies a grant.
func opAdvanceTrack(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("advance_track: no target")
	}
	if op.track == "" {
		return fmt.Errorf("advance_track: no track")
	}
	def := c.z.trackDefs().get(op.track)
	if def == nil {
		return fmt.Errorf("advance_track: unknown track %q", op.track)
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	// Raise the progress attribute base by amount (accumulating, seeded from the def default on first
	// touch — the 11.1 modify_attribute_base semantics).
	if def.progressAttr != "" {
		setAttrBase(c.target, def.progressAttr, attrBaseValue(c.target, def.progressAttr)+op.amount)
	}
	// Apply every newly-crossed step's grants. The current progress is the live derived value (mods
	// included; for a plain xp attribute that is the base). The stored step is the high-water.
	progress := attr(c.target, def.progressAttr)
	step := trackStep(c.target, op.track)
	subject := c.target
	for step < len(def.thresholds) && progress >= def.thresholds[step] {
		crossed := def.thresholds[step] // the threshold this iteration crosses (before step++ advances it)
		step++
		if grants := def.steps[step-1]; len(grants) > 0 {
			runOps(c, grants) // the step grant op-list runs on the same ctx (c.target = the advancing entity)
		}
		// Fire the progression events about the advancing entity (Phase 11.3), threading this cascade's
		// budget. Content reacts with flavor/unlocks; the engine itself reads no "level". OnLevel fires
		// only for a LEVEL track (level_attr set); every track fires OnTrackStep.
		c.z.fireEvent(c, evOnTrackStep, subject, nil, float64(step))
		if def.levelAttr != "" {
			c.z.fireEvent(c, evOnLevel, subject, nil, float64(step))
		}
		// Durable audit (#350): record each NEWLY-crossed step exactly once. dedup_key is "<track>:<step>"
		// (the stored high-water step), so a re-advance that crosses no new step never reaches here and a
		// replay of an already-recorded step dedups on the unique index. A mob subject / not-yet-saved
		// player / storeless shard is a no-op (the helper guards).
		c.z.auditTrackStep(subject, op.track, step, crossed)
	}
	setTrackStep(c.target, op.track, step)
	return nil
}
