package world

import (
	"context"
	"encoding/json"
	"sort"
)

// character.go is the persistence DTO boundary (docs/PHASE4-PLAN.md D5) and the CharacterStore
// contract for slice 4.2: the load/save of a player character + its location and carried items.
// It is the at-rest twin of buildSnapshot (the in-flight handoff form, handoff.go): the `state`
// JSON shape here is the SAME logical shape as the PlayerSnapshot, kept convergent so a handoff
// and a later save stay monotonic and a different shard can rehydrate a player it never saw from
// the checkpoint (the Phase 10 crash-failover primitive, docs/PLACEMENT.md §5-6).
//
// Like content_map.go, the on-disk JSON tags live ONLY on these transfer structs, never on the
// world component structs — so the runtime layout (hot pointers, COW) is never frozen to a
// persistence format. dumpCharacter (zone goroutine) maps the live entity -> CharSnapshot;
// loadCharacter (zone goroutine) maps CharSnapshot -> a freshly rehydrated entity.

// CharSnapshot is the durable form of a character: the engine-universal relational columns
// (PersistID, name, zone/room) plus the optimistic-concurrency version, plus the opaque content
// state (carried items/equipment, and, in later phases, attributes/resources/affects). It is
// produced on the zone goroutine by dumpCharacter and consumed off-goroutine by the saver, then
// re-applied on the zone goroutine by loadCharacter on the next login. State is the marshalled
// `characters.state` JSONB (== the PlayerSnapshot subtree); the relational fields are the
// `characters` columns the store CASes on.
type CharSnapshot struct {
	PID          PersistID // characters.id (UUID); "" for a not-yet-created character
	Name         string    // characters.name (CITEXT UNIQUE); the routing/login key
	ZoneRef      string    // characters.zone_ref: which zone to rehydrate into
	RoomRef      string    // characters.room_ref: which room within it
	StateVersion uint64    // characters.state_version: the value this snapshot was dumped AT
	State        StateJSON // the characters.state JSONB content subtree
}

// StateJSON is the content-defined `state` subtree (docs/PERSISTENCE.md §3). For the demo a
// character carries only the applied-input high-water mark and its inventory/equipment; the
// attributes/resources/affects/skills/flags subtrees are reserved (Phase 5+ fills them, Phase 4
// round-trips them opaquely). Its JSON shape is intentionally the PlayerSnapshot shape so the
// row, the Redis checkpoint, and the handoff snapshot all share one serialization.
type StateJSON struct {
	AppliedSeq uint64              `json:"applied_seq"`
	Inventory  []ItemJSON          `json:"inventory,omitempty"`
	Equipment  map[string]ItemJSON `json:"equipment,omitempty"` // wear-slot name -> worn item

	// Attributes is the per-entity attribute BASE OVERRIDES only (Phase 5.1, docs/PHASE5-PLAN.md
	// §3): race/class/level/point-buy bases keyed by attribute ref. DERIVED values are NEVER stored
	// — they are recomputed from bases + mods + defs on load — so a content rebalance (changing a
	// max_hp formula) takes effect on every character without a data migration. A pre-5.1 save (or a
	// contentless character) has none; loadCharacter handles that sanely.
	Attributes map[string]float64 `json:"attributes,omitempty"`
	// Resources is each pool's CURRENT only (max is the derived attr). loadCharacter installs these
	// clamped to the live derived max. Reserved subtrees (skills/flags) arrive in 5.3.
	Resources map[string]ResourceJSON `json:"resources,omitempty"`
	// Affects are the active status effects (Phase 5.2, docs/PHASE5-PLAN.md §3): each with its
	// REMAINING duration (in pulses — already heartbeat-denominated so it is conserved across the
	// save/load seam), magnitude, and stack count. loadCharacter re-attaches each via the runtime
	// attach path with remaining FROM THE SNAPSHOT — not the def's full duration — so a reload never
	// resets durations, double-ticks, or re-fires on_apply. The applier (source) is NOT persisted: a
	// reloaded affect is keyed as self-applied (its mechanical effect is identical). A pre-5.2 save
	// (no affects array) loads sanely with none. NOT carried on the cross-shard handoff snapshot
	// (buildSnapshot) — that is the same deferred set as inventory/stats; 5.2 does save/load only.
	Affects []AffectJSON `json:"affects,omitempty"`
	// Flags is the entity's open-set named-flag set (Phase 5.3, flags.go): per-entity booleans like the
	// "pvp" consent flag the PvP gate reads. Stored as the set of SET flag names. loadCharacter
	// re-installs each. A pre-5.3 save (no flags) loads with none (the safe default — no PvP consent).
	Flags []string `json:"flags,omitempty"`
	// Cooldowns is each armed ability cooldown's REMAINING pulses at dump time ([G8] / P6-D8, Phase
	// 6.3a), keyed by ability ref. loadCharacter re-arms each via pulse.after(remaining) on the
	// DESTINATION zone goroutine (never a cross-goroutine timer write — the Phase 5.2 lesson). A logout
	// mid-cooldown does NOT refresh it (remaining is conserved, like affect durations). Transient combat
	// state (Fighting/target/threat) is NOT persisted — combat drops cleanly on a crash/handoff. A
	// pre-6.3 save (no cooldowns) loads with none.
	Cooldowns map[string]int `json:"cooldowns,omitempty"`
}

// AffectJSON is the durable form of one active affect: the def ref, the remaining duration in PULSES,
// the magnitude, and the stack count. The §3 PERSISTENCE shape ([{id, remaining, mag, stacks}]).
type AffectJSON struct {
	ID        string  `json:"id"`               // the affect_def ref
	Remaining int     `json:"remaining"`        // pulses left (authoritative on load; never reset to full)
	Mag       float64 `json:"mag,omitempty"`    // applied magnitude (1 if absent)
	Stacks    int     `json:"stacks,omitempty"` // stack count (1 if absent)
}

// ResourceJSON is the durable form of one resource pool: the current value only (Cur). The max is a
// derived attribute, recomputed on load, never stored — so gear/affect changes to the cap are always
// reflected. Mirrors the §3 PERSISTENCE shape ({hp:{cur:N}}).
type ResourceJSON struct {
	Cur int `json:"cur"`
}

// ItemJSON is the flyweight item form (docs/PERSISTENCE.md §4, common.v1.Item): a prototype ref
// plus the COW delta, with nested contents for containers. Slice 4.2 persists the prototype ref
// and the container nesting; the delta (enchant/durability/quantity) rides in Delta as raw JSON
// and round-trips opaquely until per-item COW persistence lands. Keeping the same field names as
// the proto keeps the save form and the handoff form one shape.
type ItemJSON struct {
	ProtoRef string `json:"proto_ref"`
	// Delta is nil today (no per-item COW persistence yet). INVARIANT for when it is populated:
	// it MUST be an owned copy (marshalled bytes), never a json.RawMessage aliasing a live
	// component's COW backing buffer — dumpCharacter runs on the zone goroutine but the saver
	// reads the snapshot off-goroutine, so an aliased Delta would race the zone (see saver.go).
	Delta    json.RawMessage `json:"delta,omitempty"`
	Contents []ItemJSON      `json:"contents,omitempty"`
}

// CharacterStore is the durable character record (docs/PERSISTENCE.md §2). It is deliberately
// small: load by name (the engine-universal key), create a fresh row (minting the PersistID),
// and save with a state_version compare-and-set. Two implementations exist — a pgx store against
// the `characters` table (internal/store) and an in-memory store for tests — so the durability
// ladder logic is unit-testable with no live Postgres. The whole store is OPTIONAL: a nil store
// means ephemeral characters (today's behavior), so every storeless test stays green.
//
// All methods do blocking I/O and run OFF the zone goroutine (the login read in server.go, the
// async saver goroutine) — never on the actor loop.
type CharacterStore interface {
	// LoadCharacter returns the durable snapshot for name, or found=false when the name has
	// no row yet (a brand-new character). An error is an infrastructure failure (the caller
	// degrades, never crashes).
	LoadCharacter(ctx context.Context, name string) (snap CharSnapshot, found bool, err error)

	// CreateCharacter inserts a fresh row for name at the given start location, minting and
	// returning the PersistID (UUID). state_version starts at 0. It is the brand-new-character
	// path; an existing name is a conflict the caller treats as "load instead".
	CreateCharacter(ctx context.Context, name, zoneRef, roomRef string) (PersistID, error)

	// SaveCharacter writes snap with optimistic concurrency: it applies only when the stored
	// state_version equals snap.StateVersion (the value the snapshot was dumped at), then bumps
	// it. It returns the NEW state_version on success; ok=false (no error) means a stale writer
	// lost the CAS — the caller reconciles (re-dump at the current version), it never forces
	// the write. This is the state_version backstop behind the directory epoch (§7).
	SaveCharacter(ctx context.Context, snap CharSnapshot) (newVersion uint64, ok bool, err error)
}

// Checkpointer is the optional Redis tier of the durability ladder (docs/PERSISTENCE.md §6): a
// frequent (~10s), cheap write-back mirror whose only job is to shrink the crash data-loss
// window. It is keyed by character name so ANY shard can read the freshest checkpoint on login
// (the crash-rehydrate-by-name primitive) and pick whichever of {checkpoint, Postgres} carries
// the higher state_version. nil disables the Redis tier (Postgres-only ladder); a Redis hiccup
// is never fatal — the load falls back to Postgres, the save falls back to the next flush.
type Checkpointer interface {
	// Checkpoint writes the latest snapshot for snap.Name (overwriting the prior one). Off the
	// zone goroutine (the saver).
	Checkpoint(ctx context.Context, snap CharSnapshot) error
	// LoadCheckpoint returns the last checkpoint for name, or found=false if none. Off-goroutine
	// (the login read in server.go).
	LoadCheckpoint(ctx context.Context, name string) (snap CharSnapshot, found bool, err error)
}

// dumpCharacter serializes a live player session into its durable CharSnapshot. It runs ON the
// zone goroutine (called from leave/quit and the pulse cadence), reading only zone-owned state —
// exactly the same single-writer safety as buildSnapshot. It NEVER does I/O: the blocking write
// is the async saver's job; this is the pure produce step. The returned snapshot carries the
// session's current state_version so the saver's CAS guards against a concurrent (zombie) owner.
func dumpCharacter(s *session) CharSnapshot {
	e := s.entity
	var pid PersistID
	if e.pid != nil {
		pid = *e.pid
	}
	st := StateJSON{
		AppliedSeq: s.appliedSeq,
		Inventory:  dumpInventory(e),
		Equipment:  dumpEquipment(e),
		Attributes: dumpAttributes(e),
		Resources:  dumpResources(e),
		Affects:    dumpAffects(e),
		Flags:      dumpFlags(e),
		Cooldowns:  dumpCooldowns(e),
	}
	return CharSnapshot{
		PID:          pid,
		Name:         s.character,
		ZoneRef:      s.entity.zone.id,
		RoomRef:      string(roomRef(e.location)),
		StateVersion: s.stateVersion,
		State:        st,
	}
}

// dumpAttributes renders the entity's per-attribute BASE OVERRIDES (Living.attrBase) — bases only,
// never the derived/resolved values (those recompute on load, §3). Empty when the entity carries no
// overrides (a contentless or freshly-spawned character). Runs on the zone goroutine; reads
// zone-owned state, copied into a fresh map so the saver never aliases live entity state.
func dumpAttributes(e *Entity) map[string]float64 {
	if e.living == nil || len(e.living.attrBase) == 0 {
		return nil
	}
	out := make(map[string]float64, len(e.living.attrBase))
	for k, v := range e.living.attrBase {
		out[k] = v
	}
	return out
}

// dumpResources renders each resource pool's CURRENT only (max is the derived attr, recomputed on
// load). It uses the CLAMPED live current (resourceCurrent) so a pool whose max gear/affects lowered
// is never saved over-cap. Empty when the entity holds no resource currents.
func dumpResources(e *Entity) map[string]ResourceJSON {
	if e.living == nil || len(e.living.resCur) == 0 {
		return nil
	}
	out := make(map[string]ResourceJSON, len(e.living.resCur))
	for name := range e.living.resCur {
		out[name] = ResourceJSON{Cur: resourceCurrent(e, name)}
	}
	return out
}

// dumpAffects renders the entity's active status effects (Phase 5.2, §3): each with its REMAINING
// pulses, magnitude, and stack count. Empty when the entity carries no Affected component or no live
// affects. Runs on the zone goroutine; the remaining values are the live (decrementing) ones, so a
// save mid-affect resumes at the right point. A copy is built so the saver never aliases live state.
func dumpAffects(e *Entity) []AffectJSON {
	a, ok := Get[*Affected](e)
	if !ok || len(a.list) == 0 {
		return nil
	}
	out := make([]AffectJSON, 0, len(a.list))
	for _, inst := range a.list {
		out = append(out, AffectJSON{
			ID:        inst.def.ref,
			Remaining: inst.remaining,
			Mag:       inst.magnitude,
			Stacks:    inst.stacks,
		})
	}
	return out
}

// dumpCooldowns renders each armed ability cooldown as its REMAINING pulses ([G8] / P6-D8): the stored
// elapses-at pulse minus the current zone pulse, clamped non-negative. Remaining (not the absolute
// pulse) is the durable form so it is conserved across the save/load seam exactly like an affect's
// remaining duration — a reload re-arms from remaining, never refreshing to the full cooldown. An
// already-elapsed entry (remaining 0) is dropped. Empty when the entity holds no live cooldowns. Runs
// on the zone goroutine; reads zone-owned state into a fresh map so the saver never aliases live state.
func dumpCooldowns(e *Entity) map[string]int {
	if e == nil || e.living == nil || len(e.living.cooldowns) == 0 || e.zone == nil {
		return nil
	}
	now := e.zone.pulses.pulse
	var out map[string]int
	for ref, at := range e.living.cooldowns {
		if at <= now {
			continue // already elapsed (the clear callback just hasn't run yet)
		}
		if out == nil {
			out = map[string]int{}
		}
		out[ref] = int(at - now) //nolint:gosec // TODO(world-engineer): remaining-cooldown delta is bounded; add a guard
	}
	return out
}

// dumpFlags renders the entity's SET named flags (Living.flags) as a stable sorted slice so the
// on-disk form is deterministic. Empty when the entity carries no flags. Runs on the zone goroutine;
// reads zone-owned state into a fresh slice so the saver never aliases live entity state.
func dumpFlags(e *Entity) []string {
	if e.living == nil || len(e.living.flags) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.living.flags))
	for name := range e.living.flags {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// dumpInventory walks the player's carried items (e.contents) and renders each as an ItemJSON —
// the prototype ref plus nested container contents. Worn items stay in contents (equipped is a
// STATE over a carried item, components.go Wearer), so they are dumped under equipment AND would
// otherwise appear here; we exclude worn items from the inventory list so a load re-equips them
// from equipment alone and never double-adds. Only prototype-backed items are persistable (a
// player's own entity has no prototype and is never an item); a contents entry with no prototype
// is skipped defensively.
func dumpInventory(e *Entity) []ItemJSON {
	worn := wornSet(e)
	var out []ItemJSON
	for _, item := range e.contents {
		if item.proto == "" || worn[item] {
			continue
		}
		out = append(out, dumpItem(item))
	}
	return out
}

// dumpEquipment renders the player's worn items keyed by their wear-slot NAME (the content label,
// wearLocName) so the on-disk form never exposes the internal WearLoc enum (mirrors the DTO
// boundary in content_map.go). Empty when the player wears nothing.
func dumpEquipment(e *Entity) map[string]ItemJSON {
	wr, ok := Get[*Wearer](e)
	if !ok || len(wr.worn) == 0 {
		return nil
	}
	out := make(map[string]ItemJSON, len(wr.worn))
	for loc, item := range wr.worn {
		name := wearLocName[loc]
		if name == "" {
			continue
		}
		out[name] = dumpItem(item)
	}
	return out
}

// dumpItem renders one item entity as an ItemJSON, recursing into a container's contents. The COW
// delta is not yet captured field-by-field (slice 4.2 round-trips the prototype + nesting; the
// typed delta is a later concern, PHASE4-PLAN.md §7) so Delta stays nil for now — the field is
// present so the shape matches the proto and a later slice fills it without a format change.
func dumpItem(item *Entity) ItemJSON {
	out := ItemJSON{ProtoRef: string(item.proto)}
	for _, child := range item.contents {
		if child.proto == "" {
			continue
		}
		out.Contents = append(out.Contents, dumpItem(child))
	}
	return out
}

// wornSet returns the set of item entities currently equipped on e, so dumpInventory can exclude
// them (they are dumped under equipment). Empty when the entity has no Wearer or wears nothing.
func wornSet(e *Entity) map[*Entity]bool {
	wr, ok := Get[*Wearer](e)
	if !ok || len(wr.worn) == 0 {
		return nil
	}
	set := make(map[*Entity]bool, len(wr.worn))
	for _, item := range wr.worn {
		set[item] = true
	}
	return set
}

// equipmentItems returns the entity's worn items as a slice (any wear order), for the Lua
// handle layer's h:equipment() traversal. A non-wearer / empty wearer returns nil. Read-only.
func equipmentItems(e *Entity) []*Entity {
	wr, ok := Get[*Wearer](e)
	if !ok || len(wr.worn) == 0 {
		return nil
	}
	items := make([]*Entity, 0, len(wr.worn))
	for _, item := range wr.worn {
		if item != nil {
			items = append(items, item)
		}
	}
	return items
}

// loadCharacter applies a durable CharSnapshot onto a freshly-built player entity, rehydrating
// its PersistID, its room, and its carried/worn items. It runs ON the zone goroutine (called from
// attach's fresh-login branch, after newPlayerEntity), so every mutation is single-writer. The
// entity is NOT yet placed in a room here — attach/join does the Move into the room — so this
// only sets identity + version + spawns the inventory tree into the player's contents.
//
// Rehydrating an item is a plain spawn from its prototype ref (the flyweight source) into the
// player's contents; the COW delta is round-tripped opaquely in a later slice. An item whose
// prototype is unknown (content stripped/renamed since the save) is skipped with a warning, not a
// crash — the character still loads, just lighter.
func loadCharacter(z *Zone, s *session, snap CharSnapshot) {
	e := s.entity
	if snap.PID != "" {
		pid := snap.PID
		e.pid = &pid // PersistID becomes REAL here (identity.go)
	}
	s.stateVersion = snap.StateVersion
	// appliedSeq is the SESSION-scoped input-dedup fence (the high-water mark over THIS gate
	// session's input seq numbers), NOT a durable property of the character. A fresh login — and a
	// crash-rehydrate-by-name — is a NEW gate session whose input seq restarts at 1, so the fence MUST
	// start at 0. Restoring the saved value here would drop the returning player's first N inputs as
	// phantom replays (a silent mute until their seq climbs past the stale mark). The handoff and
	// link-dead RESUME paths keep the SAME session and preserve appliedSeq separately (zone.go attach /
	// world.go redirect resumeSeq) — only this fresh-login rehydrate resets it. The durable AppliedSeq
	// is still persisted (dumpCharacter) for diagnostics + the in-flight handoff snapshot shape.
	s.appliedSeq = 0

	// Rehydrate content-defined stats (Phase 5.1, §3). Install attribute BASE OVERRIDES FIRST so the
	// derivation has them before any resource max is computed; setAttrBase dirties the cache. Then
	// set each resource CURRENT clamped to its derived max (setResourceCurrent). DERIVED attribute
	// values are recomputed lazily on the next attr() — never loaded — so a contentless / pre-5.1
	// save (no attributes/resources subtrees) just installs nothing and the entity reads 0/full.
	if e.living != nil {
		for ref, base := range snap.State.Attributes {
			setAttrBase(e, ref, base)
		}
		for ref, r := range snap.State.Resources {
			setResourceCurrent(e, ref, r.Cur) // clamps to the live derived max
		}
		// Re-attach active affects (Phase 5.2, §3) via the runtime attach path with remaining FROM THE
		// SNAPSHOT (reattach=true) — re-registering the per-entity tick and re-seeding the prevents +
		// modifier contributions WITHOUT resetting duration, double-ticking, or re-firing on_apply. An
		// unknown ref (content stripped/renamed) is skipped by attach with a debug log, never a crash.
		for _, af := range snap.State.Affects {
			applyAffect(e, af.ID, attachOpts{
				duration: af.Remaining, magnitude: af.Mag, stacks: af.Stacks, reattach: true,
			})
		}
		// Re-install the entity's named flags (Phase 5.3, flags.go) — e.g. a player's "pvp" consent.
		for _, name := range snap.State.Flags {
			setFlag(e, name, true)
		}
		// Re-arm ability cooldowns ([G8] / P6-D8, Phase 6.3a) from their REMAINING pulses — on THIS
		// (destination) zone goroutine, so the re-armed clear callback is registered on the zone that
		// owns the entity (never a cross-goroutine timer write, the Phase 5.2 lesson). A logout mid-
		// cooldown resumes at the saved remaining; an elapsed (<=0) entry is skipped.
		for ref, remaining := range snap.State.Cooldowns {
			if remaining <= 0 {
				continue
			}
			z.rearmCooldown(s, ref, remaining)
		}
	}

	// Rehydrate carried items into the player's contents (inventory).
	for _, it := range snap.State.Inventory {
		loadItem(z, e, it)
	}
	// Rehydrate worn items: spawn each, place it in contents (equipped is a state over a carried
	// item), and record it in the Wearer slot by its content label.
	if len(snap.State.Equipment) > 0 {
		wr := actorWearer(e)
		for slotName, it := range snap.State.Equipment {
			loc, ok := wearLocByName[slotName]
			if !ok {
				z.log.Warn("character load: unknown wear slot, item dropped", "player", s.character, "slot", slotName)
				continue
			}
			item := loadItem(z, e, it)
			if item != nil {
				wr.worn[loc] = item
			}
		}
	}
	z.log.Debug("character loaded", "player", s.character, "pid", snap.PID,
		"state_version", snap.StateVersion, "applied", s.appliedSeq,
		"inventory", len(snap.State.Inventory), "equipment", len(snap.State.Equipment))
}

// loadCharacterSnapshot reads the freshest durable snapshot for a character name, picking the
// higher-state_version of {Postgres row, Redis checkpoint} — the durability-ladder freshness check
// (docs/PHASE4-PLAN.md §4 read path). It runs OFF the zone goroutine (server.go Connect), so the
// blocking reads never stall the actor loop. found=false means no row AND no checkpoint exists (a
// brand-new name) OR no store is configured (ephemeral) — the caller then creates-or-stays-blank.
//
// Crash-failover (docs/PLACEMENT.md §5-6): keying the load by NAME (not by any shard-local handle)
// is what lets ANY shard rehydrate a player it never saw — the checkpoint may have been written by
// a now-dead original owner. Picking the higher state_version means a fresh ~10s Redis checkpoint
// beats a stale ~60s Postgres flush, shrinking the crash data-loss window to the checkpoint cadence.
func (s *Shard) loadCharacterSnapshot(ctx context.Context, name string) (CharSnapshot, bool) {
	sv := s.saver
	if sv == nil || (sv.store == nil && sv.ckpt == nil) {
		return CharSnapshot{}, false // ephemeral: no durable tier
	}
	var best CharSnapshot
	var found bool

	if sv.store != nil {
		if snap, ok, err := sv.store.LoadCharacter(ctx, name); err != nil {
			s.saver.log.Debug("character load (postgres) failed; trying checkpoint", "name", name, "err", err)
		} else if ok {
			best, found = snap, true
		}
	}
	if sv.ckpt != nil {
		if snap, ok, err := sv.ckpt.LoadCheckpoint(ctx, name); err != nil {
			s.saver.log.Debug("checkpoint load failed (non-fatal)", "name", name, "err", err)
		} else if ok && (!found || snap.StateVersion > best.StateVersion) {
			// The checkpoint is fresher (or Postgres had no row): prefer it. Carry the Postgres
			// PID forward if the checkpoint lacks one (a checkpoint written before the row's
			// create returned), so the rehydrated entity keeps its durable identity.
			if snap.PID == "" {
				snap.PID = best.PID
			}
			best, found = snap, true
		}
	}
	if found {
		s.saver.log.Debug("character snapshot loaded", "name", name,
			"state_version", best.StateVersion, "room", best.RoomRef)
	}
	return best, found
}

// loadItem spawns one persisted item from its prototype ref into parent's contents, recursing
// into container contents. Returns the spawned entity (nil if the prototype is unknown). The
// spawn is byte-for-byte the same flyweight COW instance the world already builds; persistence
// only chooses WHAT to spawn and WHERE to put it.
func loadItem(z *Zone, parent *Entity, it ItemJSON) *Entity {
	item := z.spawn(ProtoRef(it.ProtoRef))
	if item == nil {
		z.log.Warn("character load: unknown item prototype, skipped", "proto", it.ProtoRef)
		return nil
	}
	Move(item, parent)
	for _, child := range it.Contents {
		loadItem(z, item, child)
	}
	return item
}
