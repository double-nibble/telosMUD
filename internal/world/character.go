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

	// PendingChargen is the not-yet-applied character-generation RESULT (Phase 14.8, Model A): set by the
	// store from the characters.chargen column when a freshly content-built character logs in for the FIRST
	// time. The world applies it on first spawn (applyPendingChargen: set the point-buy attribute bases, then
	// run each chosen bundle's grants), and the next save clears the column. nil for every returning
	// character (the common case). It is NOT part of State (so it never rides the handoff snapshot).
	PendingChargen *ChargenResult
}

// ChargenResult is the output of a content chargen flow that telos-account recorded for a new character: the
// chosen bundle refs (race/class/…) + the chosen attribute base values (point-buy). The world applies it once,
// on first spawn. It is a small, account-owned shape (NOT the world's StateJSON), kept in its own
// characters.chargen column so telos-account never has to know the PlayerSnapshot serialization.
type ChargenResult struct {
	Bundles []string           `json:"bundles,omitempty"`
	Attrs   map[string]float64 `json:"attrs,omitempty"`
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
	// (no affects array) loads sanely with none. As of the cross-shard full-state-carry fix this IS
	// carried on the handoff snapshot too (handoff.go state_json) and re-attached on the destination
	// via the shared applyStateComponents — same shape, remaining durations conserved (a frozen
	// source does not tick).
	Affects []AffectJSON `json:"affects,omitempty"`
	// Flags is the entity's open-set named-flag set (Phase 5.3, flags.go): per-entity booleans like the
	// "pvp" consent flag the PvP gate reads. Stored as the set of SET flag names. loadCharacter
	// re-installs each. A pre-5.3 save (no flags) loads with none (the safe default — no PvP consent).
	Flags []string `json:"flags,omitempty"`
	// Tracks is the entity's per-advancement-track CURRENT STEP (Phase 11.2, track.go), keyed by track
	// ref. It is the high-water of progression: a step's grant op-list ran when the step was reached and
	// wrote its results into the OTHER persisted subtrees (Attributes base overrides, Flags, …), so a
	// reload restores both the step AND its already-applied grants — the grant is never re-run (the
	// exactly-once-across-reload guarantee). A pre-11.2 save (no tracks) loads with none. DATA ONLY
	// (ref->int).
	Tracks map[string]int `json:"tracks,omitempty"`
	// Abilities is the entity's set of GRANTED ability refs (Phase 11.4a, ability_grant.go): the abilities
	// a class/race bundle or a trainer handed it. Re-installed on load so a granted (ownership-gated)
	// ability stays usable across a relogin. A pre-11.4 save (no abilities) loads with none — an entity
	// keeps whatever un-gated abilities content makes universal. DATA ONLY (a list of refs).
	Abilities []string `json:"abilities,omitempty"`
	// Professions is the entity's set of LEARNED profession refs (Phase 13.3, profession.go): the trades it
	// has enrolled in, re-installed on load so the crafting-ability requires.profession gate + the D2 cap
	// survive a relogin. A pre-13.3 save (no professions) loads with none. DATA ONLY (a list of refs) — the
	// abilities-subtree precedent; the verbs+skill a profession granted ride the EXISTING abilities/tracks
	// subtrees, so a profession needs only this thin membership list persisted.
	Professions []string `json:"professions,omitempty"`
	// LootPity is the per-pity-key consecutive-miss count (Phase 12.2, loot.go): bad-luck protection for
	// loot chance rolls, riding the durability ladder so a player's "I'm due a drop" progress survives a
	// relogin. A pre-12.2 save (no loot_pity) loads with none. DATA ONLY (key->int).
	LootPity map[string]int `json:"loot_pity,omitempty"`
	// Cooldowns is each armed ability cooldown's REMAINING pulses at dump time ([G8] / P6-D8, Phase
	// 6.3a), keyed by ability ref. loadCharacter re-arms each via pulse.after(remaining) on the
	// DESTINATION zone goroutine (never a cross-goroutine timer write — the Phase 5.2 lesson). A logout
	// mid-cooldown does NOT refresh it (remaining is conserved, like affect durations). Transient combat
	// state (Fighting/target/threat) is NOT persisted — combat drops cleanly on a crash/handoff. A
	// pre-6.3 save (no cooldowns) loads with none.
	Cooldowns map[string]int `json:"cooldowns,omitempty"`
	// Script is the PLAYER's data-only Lua self.state subtree (Phase 7.6, P7-D5, T10): the durable
	// form of the per-instance entityScripts[rid].state table (a quest counter, "have I greeted you",
	// …), marshalled through the DATA-ONLY allowlist + caps (luastate.go) — numbers/strings/bools/
	// nested tables of those ONLY, no functions/handles/userdata. dumpCharacter marshals it; load
	// re-hydrates a PLAIN table (never executes code, never resurrects a handle). A pre-7.6 save (no
	// Script) loads with an empty state — the established backward-compat default. MOB/item self.state
	// is RUNTIME-ONLY (flyweight respawn; dropped on death) — only a PLAYER's persists (the 7.6
	// boundary; a unique-persistent-mob mechanism is a later phase's concern).
	Script json.RawMessage `json:"script,omitempty"`
	// Tells is the per-player DELIVERED-CURSOR for durable tells (Phase 8.5, OQ-4, P8-A5): the last
	// tell sequence DELIVERED from each sender, keyed by sender (author) id. It is the consumer-side
	// consumer-side idempotency layer atop JetStream's at-least-once delivery: the world drains the
	// durable dtell stream and renders a message only when its Seq is strictly GREATER than the stored
	// cursor for its author, then advances the cursor. JetStream's own dedup window covers recent
	// redeliveries; this DURABLE cursor covers redeliveries AFTER the window (a consumer restart minutes
	// later). The guarantee is RENDER-AT-LEAST-ONCE / USUALLY-EXACTLY-ONCE / NEVER-LOST: a tell is never
	// lost, and renders exactly once in steady state; the one exception is a crash in the narrow window
	// between the gate emit and the next PERSISTENCE of the advanced cursor (this field rides the save
	// cadence), which can re-render that one tell ONCE on restart before the re-advanced cursor
	// re-suppresses it — bounded, never a loop or a storm; not a literal exactly-once durable guarantee.
	// It is DATA ONLY (a map of id->uint64, the Script-subtree
	// precedent) — no code, no handles — and is size-guarded (tellCursorMaxSenders) like the Lua state
	// subtree. A pre-8.5 save (no Tells) loads with an empty cursor (the backward-compat default), so a
	// returning player's first post-upgrade tells deliver from seq 0 — never a silent drop.
	Tells *TellCursorJSON `json:"tells,omitempty"`
	// Comms is the player's data-only comms-state subtree (Phase 8.6, P8-D7): the per-channel
	// enabled/disabled OVERRIDES (vs each channel's default_on), the ignore list, and the AFK
	// flag + message. It is the durable home of the receiver-side comms preferences the gate
	// enforces (the hear-set + the ignore funnel) — the Script/Tells additive-JSONB precedent.
	// dumpCommsState marshals it; loadCommsState rehydrates it. It is DATA ONLY (bools/strings/
	// lists of ids) — no code, no handles — and size-guarded (commsIgnoreMaxIDs, commsChanMaxRefs)
	// like the Lua state + tell-cursor subtrees. A pre-8.6 save (nil Comms) loads with defaults:
	// every channel at its default_on, no ignores, not AFK — the established backward-compat
	// default, so a returning player keeps the pre-8.6 behavior.
	Comms *CommsStateJSON `json:"comms,omitempty"`
}

// CommsStateJSON is the durable form of a player's receiver-side comms preferences (Phase 8.6,
// P8-D7). It is data only — the engine names no channel; the refs here are content channel refs and
// the ids are player ids. A zero value (all maps/lists empty, AFK false) is the default state and is
// OMITTED from the save (the omitempty + the nil-return from dumpCommsState), so a player who has
// never toggled anything writes no comms subtree at all.
type CommsStateJSON struct {
	// Channels is the per-channel ENABLED OVERRIDE keyed by channel ref: a present entry forces a
	// channel on (true) or off (false) regardless of its default_on; an ABSENT entry means "use the
	// channel's default_on". Storing only overrides (not the full enabled set) keeps a hot-reload of a
	// channel's default_on meaningful for players who never touched it — the Attributes-base-overrides
	// precedent (store the delta from the content default, never the derived value).
	Channels map[string]bool `json:"channels,omitempty"`
	// Ignore is the set of sender (author) ids this player has ignored — applied authoritatively at the
	// RECEIVER gate (the single ignore funnel, P8-A6). A sorted, de-duplicated slice (a set on the wire).
	Ignore []string `json:"ignore,omitempty"`
	// AFK / AFKMsg are the away flag and the optional auto-reply message. AFK is surfaced in `who` (the
	// 8.4 presence flag) and triggers a one-line auto-reply to a tell sender ("X is AFK: <msg>"); it
	// clears on the player's next input.
	AFK    bool   `json:"afk,omitempty"`
	AFKMsg string `json:"afk_msg,omitempty"`
}

// commsIgnoreMaxIDs / commsChanMaxRefs cap the comms-state subtree's two open-ended collections (the
// size guard, the tellCursorMaxSenders / Lua-state-cap precedent): a player cannot grow the durable
// state without bound by ignoring an unbounded number of senders or toggling unbounded refs. At cap the
// oldest entries are dropped (ignore is a set, channel overrides are bounded by the loaded channel_defs
// anyway, so the cap is a defensive ceiling, rarely hit).
const (
	commsIgnoreMaxIDs = 1024
	commsChanMaxRefs  = 256
)

// marshalCommsState / unmarshalCommsState keep the comms-state JSON encode/decode in one place so the
// durable form (StateJSON.Comms) and the handoff-carry form (handoff.go CommsState) are byte-identical.
func marshalCommsState(c *CommsStateJSON) (string, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

func unmarshalCommsState(raw string) (*CommsStateJSON, error) {
	var c CommsStateJSON
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// TellCursorJSON is the durable per-player delivered-cursor (Phase 8.5, OQ-4): Delivered[authorID] is
// the highest tell Seq from that author this player has already had rendered. The render gate is
// strictly-greater (msg.Seq > Delivered[author]) so a redelivery (Seq <= stored) is suppressed.
type TellCursorJSON struct {
	Delivered map[string]uint64 `json:"delivered,omitempty"`
}

// tellCursorMaxSenders caps how many distinct senders a player's delivered-cursor tracks (the size
// guard, the Lua-state-cap precedent): a player tracking an unbounded number of senders would grow
// the state subtree without bound. When at cap, dumpTellCursor keeps the most-recently-advanced
// senders (the cursor is a dedup optimization, not a ledger — a dropped old sender at worst risks one
// re-render of a very old tell that has itself almost certainly aged out of the stream).
const tellCursorMaxSenders = 1024

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
	st := dumpStateComponents(e)
	st.AppliedSeq = s.appliedSeq
	st.Tells = dumpTellCursor(s) // Phase 8.5 — the durable-tell delivered-cursor (OQ-4)
	st.Comms = dumpCommsState(s) // Phase 8.6 — the receiver-side comms-state subtree (P8-D7)
	return CharSnapshot{
		PID:          pid,
		Name:         s.character,
		ZoneRef:      s.entity.zone.id,
		RoomRef:      string(roomRef(e.location)),
		StateVersion: s.stateVersion,
		State:        st,
	}
}

// dumpStateComponents marshals the ENTITY-scoped content subtree of a character — the
// components that live on the *Entity (inventory, equipment, attribute bases, resource currents,
// affects, flags, cooldowns, the data-only self.state) — into a StateJSON. It deliberately OMITS
// the SESSION-scoped subtrees (AppliedSeq, the tell delivered-cursor, the comms-state) because
// those are not entity state; the caller layers those in (dumpCharacter) or excludes them (the
// handoff carry, where comms rides its own dedicated snapshot field and the tell cursor is out of
// scope). This is the single component-dump path shared by the durable save (dumpCharacter) and
// the cross-shard handoff carry (dumpStateJSON) — there is NO parallel serialization. Runs on the
// zone goroutine; every component dumper copies into fresh storage so the saver/snapshot never
// aliases live entity state (the character.go:196-201 owned-bytes invariant).
func dumpStateComponents(e *Entity) StateJSON {
	return StateJSON{
		Inventory:   dumpInventory(e),
		Equipment:   dumpEquipment(e),
		Attributes:  dumpAttributes(e),
		Resources:   dumpResources(e),
		Affects:     dumpAffects(e),
		Flags:       dumpFlags(e),
		Tracks:      dumpTracks(e),           // Phase 11.2 — per-track current step
		Abilities:   dumpGrantedAbilities(e), // Phase 11.4a — granted ability refs
		Professions: dumpProfessions(e),      // Phase 13.3 — learned profession refs
		LootPity:    dumpLootPity(e),         // Phase 12.2 — per-key consecutive-miss counts
		Cooldowns:   dumpCooldowns(e),
		Script:      dumpScriptState(e), // Phase 7.6 — the player's data-only self.state subtree
	}
}

// dumpTracks renders the entity's per-advancement-track current step as a fresh map (a copy, so the
// snapshot never aliases live instance state). nil when no track has been granted.
func dumpTracks(e *Entity) map[string]int {
	if e == nil || e.living == nil || len(e.living.tracks) == 0 {
		return nil
	}
	out := make(map[string]int, len(e.living.tracks))
	for ref, step := range e.living.tracks {
		out[ref] = step
	}
	return out
}

// dumpStateJSON marshals the cross-shard handoff state carry (the full-state-carry fix) to the
// JSON string carried on the handoff snapshot (handoff.go state_json, field 15), or "" when the
// player carries no entity state (a bare/contentless player — the destination then resolves from
// defaults, identical to the pre-carry snapshot). It reuses dumpStateComponents so the handoff
// carry and the durable save are byte-identical for the entity subtree (one shape, no parallel
// serialization). It EXCLUDES:
//   - the comms subtree: field 14 (comms_state) is the SINGLE authority for comms on the handoff
//     (no double-writer);
//   - the tell delivered-cursor: a session-scoped cursor, out of scope for this carry (FOLLOW-UPS);
//   - AppliedSeq: the linchpin is seeded from the dedicated snapshot field (field 12), NEVER routed
//     through the embedded state (a fresh login restarts the fence; a handoff keeps the session).
//
// Runs on the zone goroutine at the freeze point, so the read is race-free and the affect/cooldown
// remaining values are conserved (a frozen source does not tick).
func dumpStateJSON(s *session) string {
	st := dumpStateComponents(s.entity)
	if st.empty() {
		return "" // bare/contentless player: nothing to carry (defaults on the destination)
	}
	b, err := json.Marshal(st)
	if err != nil {
		s.entity.zone.log.Error("handoff state carry marshal failed (carrying empty)",
			"player", s.character, "err", err.Error())
		return ""
	}
	return string(b)
}

// empty reports whether the entity-scoped state carry has nothing to carry, so dumpStateJSON omits
// the field entirely for a bare player (parity with the all-default comms "" case). AppliedSeq/Tells/
// Comms are not set on the carry form, so they do not affect this.
func (st StateJSON) empty() bool {
	return len(st.Inventory) == 0 && len(st.Equipment) == 0 && len(st.Attributes) == 0 &&
		len(st.Resources) == 0 && len(st.Affects) == 0 && len(st.Flags) == 0 &&
		len(st.Tracks) == 0 && len(st.Abilities) == 0 && len(st.LootPity) == 0 &&
		len(st.Cooldowns) == 0 && len(st.Script) == 0
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
		out[ref] = pulsesToInt(at - now) // at > now (guarded above): a small, bounded remaining-cooldown delta
	}
	return out
}

// dumpScriptState marshals the PLAYER's data-only Lua self.state subtree (Phase 7.6, luastate.go)
// into durable JSON. It reads the per-instance entityScripts[rid].state table and runs it through
// the data-only allowlist + caps. Returns nil when the entity has no script state (the common
// case — a player with no quest/script state, and the backward-compat default). A marshaller
// REJECTION (a handle/function in self.state, or an over-cap subtree) is logged LOUDLY (naming the
// bad key) and the Script subtree is OMITTED from this save — the rest of the character's state
// still persists, but the bad script state is NOT silently persisted as garbage; the loud log is
// the author-facing surface. Runs on the zone goroutine.
func dumpScriptState(e *Entity) json.RawMessage {
	if e == nil || e.zone == nil || e.zone.lua == nil {
		return nil
	}
	es := e.zone.lua.entityScripts[e.rid]
	if es == nil || es.state == nil {
		return nil // no script state for this entity
	}
	b, err := marshalLuaState(es.state)
	if err != nil {
		e.zone.log.Error("self.state save REJECTED (not persisted; fix the script state)",
			"rid", e.rid, "err", err.Error())
		return nil
	}
	return b
}

// loadScriptState re-hydrates a player's persisted self.state JSON (Phase 7.6) into a PLAIN Lua
// table installed as the entity's self.state (entityScripts[rid].state). Empty/nil bytes install a
// fresh empty state (pre-7.6 / no-script-state default). A malformed blob degrades to an empty
// state with a loud log. Never executes code, never resurrects a handle (the persisted form has
// none). Runs on the zone goroutine.
func loadScriptState(z *Zone, e *Entity, b json.RawMessage) {
	if z == nil || z.lua == nil || e == nil {
		return
	}
	if len(b) == 0 {
		return // no script state to install (the entity's state, if accessed, is created empty)
	}
	t, err := z.lua.unmarshalLuaState(b)
	if err != nil {
		z.log.Error("self.state load degraded to empty (malformed persisted state)", "rid", e.rid, "err", err.Error())
		return
	}
	z.lua.setStateTable(e, t)
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
		// #27/#28: the RESERVED trust flags (holylight/builder/admin) are NEVER persisted — they are derived
		// from the account tier at login (applyTierFlags), so they can't be injected via the state/handoff
		// snapshot (an UNAUTHENTICATED channel) and restored through the trusted setFlag path, bypassing the
		// content op guard. Keeping them off the persistence boundary is the structural fix (security-audit H-1).
		if reservedFlag(name) {
			continue
		}
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

// dumpEquipment renders the player's worn items keyed by their wear-slot REF (#35 — the stable content slot
// id, WearLoc). The ref is the durable key (a slot's LABEL may be re-authored; its ref is its identity), and
// the load path resolves legacy label keys back for backward compatibility. Empty when the player wears nothing.
func dumpEquipment(e *Entity) map[string]ItemJSON {
	wr, ok := Get[*Wearer](e)
	if !ok || len(wr.worn) == 0 {
		return nil
	}
	out := make(map[string]ItemJSON, len(wr.worn))
	for loc, item := range wr.worn {
		if loc == WearLocNone {
			continue
		}
		out[string(loc)] = dumpItem(item)
	}
	return out
}

// dumpItem renders one item entity as an ItemJSON, recursing into a container's contents. The COW
// delta is not yet captured field-by-field (slice 4.2 round-trips the prototype + nesting; the
// typed delta is a later concern, PHASE4-PLAN.md §7) so Delta stays nil for now — the field is
// present so the shape matches the proto and a later slice fills it without a format change.
func dumpItem(item *Entity) ItemJSON {
	out := ItemJSON{ProtoRef: string(item.proto)}
	out.Delta = dumpItemDelta(item) // Phase 12.3/13.1: per-instance delta (quality + bound state)
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
	// the handoff snapshot's AppliedSeq) — only this fresh-login rehydrate resets it. The durable
	// AppliedSeq is still persisted (dumpCharacter) for the in-flight handoff snapshot shape.
	s.appliedSeq = 0

	// Re-install the entity-scoped content subtree (items/attributes/resources/affects/flags/cooldowns/
	// self.state) via the SHARED applier — the SAME path the cross-shard handoff rehydrate uses, so a
	// fresh login and a handoff arrive at byte-identical entity state. AppliedSeq is NOT part of this
	// applier (see applyStateComponents): the fresh-login reset above stays the ONLY writer of it on
	// this path.
	_ = applyStateComponents(z, s, snap.State) // fresh login: dropped-item count surfaced only on the handoff path

	// Rehydrate the durable-tell delivered-cursor (Phase 8.5, OQ-4) so a redelivery after this login
	// renders ONCE. A pre-8.5 save (nil Tells) installs an empty cursor (the backward-compat default).
	// SESSION-scoped (not in applyStateComponents) and NOT carried on the handoff (out of scope).
	loadTellCursor(s, snap.State.Tells)

	// Rehydrate the receiver-side comms-state subtree (Phase 8.6, P8-D7): channel toggles, ignore list,
	// AFK. A pre-8.6 save (nil Comms) installs all-default state (every channel at its default_on, no
	// ignores, not AFK). loadCharacter runs on the zone goroutine that now owns the session; the caller
	// (re)publishes the effective hear-set to the gate AFTER load (zone.go join/attach). SESSION-scoped
	// (not in applyStateComponents); on the handoff path comms rides its own dedicated snapshot field.
	loadCommsState(s, snap.State.Comms)

	z.log.Debug("character loaded", "player", s.character, "pid", snap.PID,
		"state_version", snap.StateVersion, "applied", s.appliedSeq,
		"inventory", len(snap.State.Inventory), "equipment", len(snap.State.Equipment))
}

// applyStateComponents re-installs the ENTITY-scoped content subtree of a StateJSON onto a player's
// live entity (s.entity), on the zone goroutine. It is the shared applier called by BOTH the
// fresh-login rehydrate (loadCharacter) AND the cross-shard handoff rehydrate (zone.go prepare),
// so a player's inventory/equipment/attributes/resources/affects/flags/cooldowns/self.state arrive
// identically by either route.
//
// CRITICAL: it does NOT touch s.appliedSeq. The fresh-login path forces appliedSeq=0 (a new gate
// session restarts the input-dedup fence) BEFORE calling this; the handoff path seeds appliedSeq
// from the dedicated snapshot field (it keeps the SAME gate session). Routing appliedSeq through
// here would break one of the two — so it stays OUT, owned by each caller. Likewise the comms and
// tell-cursor subtrees are SESSION-scoped and applied by the caller, not here.
//
// Stats install order matters and is the WHOLE point of the sequencing here: the resource-CURRENT
// install must happen LAST, after every input to the derived resource MAX has landed, so the
// current is clamped to the genuinely-final max. The order is:
//  1. attribute BASE OVERRIDES (the raw bases the derivation starts from);
//  2. affects (reattach=true, no duration reset / on_apply re-fire) — these contribute attribute
//     MODIFIERS that can RAISE (or lower) a resource's max attribute (a +con / +max_hp buff);
//  3. flags; cooldowns re-armed on THIS zone goroutine (never a cross-goroutine timer write — the
//     Phase 5.2 lesson); the data-only self.state;
//  4. worn EQUIPMENT — gear is also a modifier source (attributes.go §1.1), so a max-raising piece
//     of gear must be installed before the clamp too;
//  5. ONLY NOW resource CURRENTS, clamped to the now-final derived max.
//
// Clamping resources earlier (the latent bug fixed here) would clamp a wounded current DOWN to a
// pre-affect/pre-gear max and the read-side clamp (resourceCurrent) only ever clamps DOWN, so it
// could never restore the lost headroom — a buffed/geared wounded player would arrive under-healed.
// markAttrsDirty (fired by every base/affect/gear install) means the max is recomputed lazily on
// the clamp's read, so the ordering is what makes it correct, not any explicit recompute. The
// affect/equipment appliers do NOT read the resource current (verified: affect attach only edits
// the modifier maps + dirties the cache; gear is a modSource), so installing currents last is safe.
// A contentless / pre-feature subtree installs nothing.
//
// It returns the number of carried item prototypes the destination could NOT spawn (unknown on
// this shard's enabled-pack set). For a fresh login this is ~always 0 (the saver wrote from the
// same content); for a CROSS-SHARD handoff it CAN be non-zero (the destination may enable a
// different pack set than the source), which is a data-loss window save/load does not have — the
// handoff caller surfaces it loudly + as a one-line player notice (see zone.go prepare).
func applyStateComponents(z *Zone, s *session, st StateJSON) (droppedItems int) {
	e := s.entity
	if e.living != nil {
		for ref, base := range st.Attributes {
			setAttrBase(e, ref, base)
		}
		// Re-attach active affects (Phase 5.2, §3) via the runtime attach path with remaining FROM THE
		// SNAPSHOT (reattach=true) — re-registering the per-entity tick and re-seeding the prevents +
		// modifier contributions WITHOUT resetting duration, double-ticking, or re-firing on_apply. An
		// unknown ref (content stripped/renamed) is skipped by attach with a debug log, never a crash.
		// These can RAISE a resource's max attribute, so they MUST land before the resource clamp below.
		for _, af := range st.Affects {
			applyAffect(e, af.ID, attachOpts{
				duration: af.Remaining, magnitude: af.Mag, stacks: af.Stacks, reattach: true,
			}, nil) // a persistence reattach is a root (and skips on_apply/the bus fire anyway)
		}
		// Re-install the entity's named flags (Phase 5.3, flags.go) — e.g. a player's "pvp" consent. The
		// RESERVED trust flags (holylight/builder/admin) are SKIPPED (security-audit H-1): they are never
		// persisted (dumpFlags) and must never be installed from a state/handoff snapshot — this restore runs
		// via the TRUSTED setFlag, bypassing the content op guard, and the handoff snapshot is unauthenticated.
		// They are derived solely from the verified account tier at login (applyTierFlags). Defense-in-depth:
		// even a forged/legacy snapshot carrying one is ignored here.
		for _, name := range st.Flags {
			if reservedFlag(name) {
				continue
			}
			setFlag(e, name, true)
		}
		// Re-install the entity's advancement-track steps (Phase 11.2, track.go). The step is the high-
		// water — its grants already wrote into the attribute/flag/resource subtrees restored above, so we
		// restore the STEP only and never re-run a grant (the exactly-once-across-reload guarantee).
		for ref, step := range st.Tracks {
			setTrackStep(e, ref, step)
		}
		// Re-install the entity's granted abilities (Phase 11.4a) so an ownership-gated ability stays
		// usable across a relogin.
		for _, ref := range st.Abilities {
			grantAbility(e, ref)
		}
		// Re-install learned professions (Phase 13.3) so the requires.profession gate + the D2 cap survive a
		// relogin. learnProfession enforces the cap, but a saved set already passed it; restore directly into
		// the membership map so a later cap reduction never silently drops an already-earned trade on load.
		for _, ref := range st.Professions {
			if l := mutableLiving(e); l != nil {
				if l.professions == nil {
					l.professions = map[string]bool{}
				}
				l.professions[ref] = true
			}
		}
		// Re-install the entity's loot-pity counters (Phase 12.2) so bad-luck protection survives a relogin.
		for key, misses := range st.LootPity {
			setLootPityMisses(e, key, misses)
		}
		// Re-arm ability cooldowns ([G8] / P6-D8, Phase 6.3a) from their REMAINING pulses — on THIS
		// (destination) zone goroutine, so the re-armed clear callback is registered on the zone that
		// owns the entity (never a cross-goroutine timer write, the Phase 5.2 lesson). A logout mid-
		// cooldown resumes at the saved remaining; an elapsed (<=0) entry is skipped.
		for ref, remaining := range st.Cooldowns {
			if remaining <= 0 {
				continue
			}
			z.rearmCooldown(s, ref, remaining)
		}
	}

	// Rehydrate the PLAYER's data-only self.state subtree (Phase 7.6, luastate.go) into a PLAIN Lua
	// table on the zone's LState — never executes code, never resurrects a handle. A pre-7.6 save (no
	// Script) installs an empty state (the backward-compat default). A malformed blob degrades to an
	// empty state with a loud log, never a crash.
	loadScriptState(z, e, st.Script)

	// Rehydrate carried items into the player's contents (inventory).
	for _, it := range st.Inventory {
		_, dropped := loadItem(z, e, it, 0)
		droppedItems += dropped
	}
	// Rehydrate worn items: spawn each, place it in contents (equipped is a state over a carried
	// item), and record it in the Wearer slot by its content label. Gear is a modifier source, so a
	// max-raising piece must be worn BEFORE the resource clamp below.
	if len(st.Equipment) > 0 {
		wr := actorWearer(e)
		vocab := z.wearSlots()
		for slotName, it := range st.Equipment {
			loc, ok := vocab.resolveKey(slotName) // #35: ref, or a legacy label key, resolved to a content slot
			if !ok {
				z.log.Warn("character load: unknown wear slot, item dropped", "player", s.character, "slot", slotName)
				droppedItems++
				continue
			}
			item, dropped := loadItem(z, e, it, 0)
			droppedItems += dropped
			if item != nil {
				wr.worn[loc] = item
			}
		}
		// #35: register + sum the worn affix bonus now, so the resource clamp below sees the gear-boosted
		// derived max (the ordering the comment above depends on).
		applyWornMods(e, wr)
	}

	// FINALLY install resource CURRENTS, clamped to the now-final derived max (after attributes +
	// affects + gear). This is the ordering fix: a wounded current is conserved against the genuinely
	// final, possibly-boosted max, never clamped down to a pre-affect/pre-gear max it can't recover.
	if e.living != nil {
		for ref, r := range st.Resources {
			setResourceCurrent(e, ref, r.Cur) // clamps to the live (final) derived max
		}
	}
	return droppedItems
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

// maxItemNestDepth bounds how deep a persisted/carried inventory tree is rehydrated. Container
// nesting is the one open-ended dimension of the StateJSON/handoff carry that is NOT otherwise
// size-guarded (the script/comms/tell subtrees have their own caps), so a pathological or
// adversarial snapshot could otherwise size the Prepare payload (and the rehydrate recursion)
// without bound. A real bag-in-a-bag-in-a-bag is only a few levels; this ceiling is generous and
// only ever trims a degenerate tree (logged loudly). FOLLOW-UPS notes a total node/byte cap as
// the deeper guard.
const maxItemNestDepth = 16

// maxCarryItemNodes bounds the TOTAL item-node count (inventory + worn + all nested container contents) a
// cross-shard handoff may carry — the WIDTH guard that complements maxItemNestDepth's depth guard. A
// wide-but-shallow adversarial tree would otherwise be bounded only by the gRPC message size, still large
// enough to stall the destination zone goroutine on rehydrate (spawn-bomb). A generous ceiling for a real
// inventory; a carry past it is rejected at Prepare.
const maxCarryItemNodes = 512

// maxCarryStateBytes bounds the marshalled StateJson a cross-shard handoff may carry — the total-payload
// guard checked before unmarshal, so a forged/oversized carry (the handoff is unauthenticated, §5) can't
// force a huge allocation on the destination. Generous for a real player's full state; gRPC's own message
// limit is far too loose to protect the zone goroutine.
const maxCarryStateBytes = 256 * 1024

// maxDurableStateBytes mirrors maxCarryStateBytes on the DURABLE save path (docs/REMAINING.md §1, symmetry).
// Unlike the handoff carry — which arrives over the network and is REJECTED past the cap — the durable state
// is the player's OWN engine-produced state, so exceeding this is a loud operator WARN (an unbounded-growth
// bug, e.g. a builder self.state that accumulates), NOT a dropped save: losing a legitimate save is worse
// than persisting a large row. Checked off the zone goroutine in the saver.
const maxDurableStateBytes = maxCarryStateBytes

// carryItemAudit walks a carried inventory tree (inventory + worn + nested contents) ONCE and returns both
// the prototype refs this shard cannot spawn (missing — a pack mismatch) and the total item-node count (for
// the width cap). A cross-shard Prepare rejects on either, before committing. A uniform-pack fleet with a
// normal inventory returns (nil, small). The depth-cap-truncated tail is not counted or flagged missing.
func (z *Zone) carryItemAudit(st StateJSON) (missing []string, nodes int) {
	var walk func(its []ItemJSON, depth int)
	walk = func(its []ItemJSON, depth int) {
		if depth > maxItemNestDepth {
			return
		}
		for _, it := range its {
			nodes++
			if z.protos.get(ProtoRef(it.ProtoRef)) == nil {
				missing = append(missing, it.ProtoRef)
			}
			walk(it.Contents, depth+1)
		}
	}
	walk(st.Inventory, 0)
	for _, it := range st.Equipment {
		nodes++
		if z.protos.get(ProtoRef(it.ProtoRef)) == nil {
			missing = append(missing, it.ProtoRef)
		}
		walk(it.Contents, 1)
	}
	return missing, nodes
}

// loadItem spawns one persisted item from its prototype ref into parent's contents, recursing
// into container contents. Returns the spawned entity (nil if the prototype is unknown) and the
// count of item prototypes that could NOT be spawned (unknown on this shard) anywhere in this
// subtree — so the cross-shard handoff rehydrate can surface a data-loss notice. The spawn is
// byte-for-byte the same flyweight COW instance the world already builds; persistence only
// chooses WHAT to spawn and WHERE to put it. depth is the current nesting level (0 at the root of
// a carried item); a subtree deeper than maxItemNestDepth is truncated with a loud log.
func loadItem(z *Zone, parent *Entity, it ItemJSON, depth int) (*Entity, int) {
	if depth > maxItemNestDepth {
		z.log.Warn("character load: item nesting exceeds cap, deeper contents dropped",
			"proto", it.ProtoRef, "depth", depth, "cap", maxItemNestDepth)
		return nil, 1
	}
	item := z.spawn(ProtoRef(it.ProtoRef))
	if item == nil {
		// Unknown prototype on THIS shard. For a fresh login this is content stripped/renamed; for a
		// cross-shard handoff it is a destination enabled-pack mismatch (a genuine data-loss window) —
		// the loud Warn here is the operator surface; the caller adds the player notice.
		z.log.Warn("character load: unknown item prototype, skipped", "proto", it.ProtoRef, "depth", depth)
		return nil, 1
	}
	loadItemDelta(item, it.Delta) // Phase 12.3/13.1: re-attach the per-instance delta (quality + bound)
	Move(item, parent)
	dropped := 0
	for _, child := range it.Contents {
		_, d := loadItem(z, item, child, depth+1)
		dropped += d
	}
	return item, dropped
}
