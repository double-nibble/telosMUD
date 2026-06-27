package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/double-nibble/telosmud/internal/content"
)

// content.go is the store side of the content read path: it loads the definition rows for the
// enabled packs and assembles them into content.Pack values, implementing content.Source. The
// `body` JSONB of an item/mob prototype carries the component DTOs (physical/wearable/weapon/
// container); the `body` of a zone_reset carries the ResetDTO. Stable relational columns
// (ref/name/short/long/keywords/exits) stay first-class so FK integrity and indexed lookups
// work, with the open-ended remainder in JSONB (docs/PERSISTENCE.md §1).

// Compile-time assertions that *Pool serves both the bulk and single-ref content reads.
var (
	_ content.Source           = (*Pool)(nil)
	_ content.DefinitionSource = (*Pool)(nil)
)

// protoBody is the JSONB tail shape for an item/mob prototype row: the optional component
// templates. It mirrors the component fields of content.ProtoDTO so a round-trip through the
// DB column reproduces the same prototype the embedded YAML would.
type protoBody struct {
	Physical  *content.PhysicalDTO  `json:"physical,omitempty"`
	Wearable  *content.WearableDTO  `json:"wearable,omitempty"`
	Weapon    *content.WeaponDTO    `json:"weapon,omitempty"`
	Container *content.ContainerDTO `json:"container,omitempty"`
	// Living is the mob-statting block (Phase 6.3a): the per-entity attribute base overrides + the
	// combat_profile ref. It rides the SAME mob_prototypes.body JSONB as the other component
	// templates (the schema's "living/mob/AI components" tail). Without it a DB round-trip dropped
	// the goblin's stat sheet (Living went nil), so the swing pipeline had no mob numbers — the gap
	// TestStorePackRoundTrip caught. nil for every inert item (omitempty keeps their body unchanged).
	Living *content.LivingDTO `json:"living,omitempty"`
}

// LoadPacks implements content.Source: it reads every loaded definition for the enabled packs
// from Postgres and returns one content.Pack per pack name. Unknown pack names yield nothing.
func (p *Pool) LoadPacks(ctx context.Context, enabled []string) ([]content.Pack, error) {
	if len(enabled) == 0 {
		return nil, nil
	}
	byPack := map[string]*content.Pack{}
	zonesByRef := map[string]*content.ZoneDTO{}
	pack := func(name string) *content.Pack {
		if byPack[name] == nil {
			byPack[name] = &content.Pack{Pack: name}
		}
		return byPack[name]
	}

	// Zones.
	rows, err := p.pool.Query(ctx,
		`SELECT ref, pack, name, COALESCE(start_room, ''), reset_secs
		   FROM zones WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return nil, fmt.Errorf("store: query zones: %w", err)
	}
	for rows.Next() {
		var z content.ZoneDTO
		var pk string
		if err := rows.Scan(&z.Ref, &pk, &z.Name, &z.StartRoom, &z.ResetSecs); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan zone: %w", err)
		}
		pp := pack(pk)
		pp.Zones = append(pp.Zones, z)
		zonesByRef[z.Ref] = &pp.Zones[len(pp.Zones)-1]
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// Re-index zones AFTER all appends: appending to pp.Zones above may reallocate the
	// backing array, dangling any &pp.Zones[i] taken mid-loop. Rebuild the index against the
	// final slices so the child-row loaders mutate the right ZoneDTOs.
	for _, pp := range byPack {
		for i := range pp.Zones {
			zonesByRef[pp.Zones[i].Ref] = &pp.Zones[i]
		}
	}

	// Rooms (+ exits, fetched per pack and grouped onto their zone).
	if err := p.loadRooms(ctx, enabled, zonesByRef); err != nil {
		return nil, err
	}
	if err := p.loadPrototypes(ctx, enabled, zonesByRef); err != nil {
		return nil, err
	}
	if err := p.loadResets(ctx, enabled, zonesByRef); err != nil {
		return nil, err
	}
	// Pack-GLOBAL defs (Phase 5.1): zone-independent, grouped onto their pack (not a zone).
	if err := p.loadGlobalDefs(ctx, enabled, pack); err != nil {
		return nil, err
	}

	out := make([]content.Pack, 0, len(byPack))
	for _, name := range enabled {
		if pp := byPack[name]; pp != nil {
			out = append(out, *pp)
		}
	}
	return out, nil
}

// LoadDefinition implements content.DefinitionSource: the SINGLE-ROW re-read behind hot reload
// (docs/PHASE4-PLAN.md §5). It fetches exactly the (kind, ref) row within pack with an indexed PK
// lookup and assembles the same neutral DTO the bulk loader produces, so the world's mapper
// rebuilds a byte-identical prototype. Found=false (no error) means the row was deleted/renamed —
// the caller drops the prototype. pgx.ErrNoRows is the not-found signal, not an error.
func (p *Pool) LoadDefinition(ctx context.Context, kind, ref, pack string) (content.Definition, error) {
	switch kind {
	case content.KindRoom:
		return p.loadRoomDefinition(ctx, ref, pack)
	case content.KindItem:
		return p.loadProtoDefinition(ctx, "item_prototypes", kind, ref, pack)
	case content.KindMob:
		return p.loadProtoDefinition(ctx, "mob_prototypes", kind, ref, pack)
	default:
		// Unknown/unsupported kind (e.g. zone): nothing to reload as a prototype. Report
		// not-found so the caller no-ops rather than erroring.
		return content.Definition{Kind: kind, Ref: ref, Found: false}, nil
	}
}

// loadRoomDefinition fetches one room row plus its exits and returns it as a RoomDTO-bearing
// Definition. The exits are a small second query keyed by the room ref.
func (p *Pool) loadRoomDefinition(ctx context.Context, ref, pack string) (content.Definition, error) {
	var r content.RoomDTO
	var flags []byte
	err := p.pool.QueryRow(ctx,
		`SELECT ref, name, COALESCE(sector, ''), COALESCE(body->>'long', ''),
		        COALESCE(body->'flags', '[]'::jsonb)
		   FROM rooms WHERE ref = $1 AND pack = $2`, ref, pack).
		Scan(&r.Ref, &r.Name, &r.Sector, &r.Long, &flags)
	if errors.Is(err, pgx.ErrNoRows) {
		return content.Definition{Kind: content.KindRoom, Ref: ref, Found: false}, nil
	}
	if err != nil {
		return content.Definition{}, fmt.Errorf("store: load room definition %s: %w", ref, err)
	}
	if len(flags) > 0 {
		if err := json.Unmarshal(flags, &r.Flags); err != nil {
			return content.Definition{}, fmt.Errorf("store: room %s flags: %w", ref, err)
		}
	}
	r.Exits = map[string]string{}
	exRows, err := p.pool.Query(ctx, `SELECT dir, to_room FROM exits WHERE from_room = $1`, ref)
	if err != nil {
		return content.Definition{}, fmt.Errorf("store: load room exits %s: %w", ref, err)
	}
	defer exRows.Close()
	for exRows.Next() {
		var dir, to string
		if err := exRows.Scan(&dir, &to); err != nil {
			return content.Definition{}, fmt.Errorf("store: scan exit %s: %w", ref, err)
		}
		r.Exits[dir] = to
	}
	if err := exRows.Err(); err != nil {
		return content.Definition{}, err
	}
	return content.Definition{Kind: content.KindRoom, Ref: ref, Found: true, Room: r}, nil
}

// loadProtoDefinition fetches one item/mob prototype row and decodes its component body, returning
// a ProtoDTO-bearing Definition. table selects items vs mobs; kind is echoed into the result.
func (p *Pool) loadProtoDefinition(ctx context.Context, table, kind, ref, pack string) (content.Definition, error) {
	var d content.ProtoDTO
	var body []byte
	err := p.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT ref, short, long, keywords, body FROM %s WHERE ref = $1 AND pack = $2`, table),
		ref, pack).Scan(&d.Ref, &d.Short, &d.Long, &d.Keywords, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return content.Definition{Kind: kind, Ref: ref, Found: false}, nil
	}
	if err != nil {
		return content.Definition{}, fmt.Errorf("store: load %s definition %s: %w", table, ref, err)
	}
	if len(body) > 0 {
		var b protoBody
		if err := json.Unmarshal(body, &b); err != nil {
			return content.Definition{}, fmt.Errorf("store: %s %s body: %w", table, ref, err)
		}
		d.Physical, d.Wearable, d.Weapon, d.Container = b.Physical, b.Wearable, b.Weapon, b.Container
		d.Living = b.Living
	}
	return content.Definition{Kind: kind, Ref: ref, Found: true, Proto: d}, nil
}

func (p *Pool) loadRooms(ctx context.Context, enabled []string, zones map[string]*content.ZoneDTO) error {
	rooms := map[string]*content.RoomDTO{}
	rows, err := p.pool.Query(ctx,
		`SELECT ref, zone_ref, name, COALESCE(sector, ''), COALESCE(body->>'long', ''),
		        COALESCE(body->'flags', '[]'::jsonb)
		   FROM rooms WHERE pack = ANY($1) ORDER BY zone_ref, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query rooms: %w", err)
	}
	for rows.Next() {
		var r content.RoomDTO
		var zoneRef string
		var flags []byte
		if err := rows.Scan(&r.Ref, &zoneRef, &r.Name, &r.Sector, &r.Long, &flags); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan room: %w", err)
		}
		if len(flags) > 0 {
			if err := json.Unmarshal(flags, &r.Flags); err != nil {
				rows.Close()
				return fmt.Errorf("store: room %s flags: %w", r.Ref, err)
			}
		}
		r.Exits = map[string]string{}
		z := zones[zoneRef]
		if z == nil {
			continue // orphan room (zone not in the enabled set); skip
		}
		z.Rooms = append(z.Rooms, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	// Index rooms AFTER all appends (the append above reallocates z.Rooms), so the exit pass
	// attaches each exit onto the FINAL RoomDTO, not a stale copy.
	for _, z := range zones {
		for i := range z.Rooms {
			if z.Rooms[i].Exits == nil {
				z.Rooms[i].Exits = map[string]string{}
			}
			rooms[z.Rooms[i].Ref] = &z.Rooms[i]
		}
	}

	exRows, err := p.pool.Query(ctx,
		`SELECT e.from_room, e.dir, e.to_room
		   FROM exits e JOIN rooms r ON r.ref = e.from_room
		  WHERE r.pack = ANY($1)`, enabled)
	if err != nil {
		return fmt.Errorf("store: query exits: %w", err)
	}
	defer exRows.Close()
	for exRows.Next() {
		var from, dir, to string
		if err := exRows.Scan(&from, &dir, &to); err != nil {
			return fmt.Errorf("store: scan exit: %w", err)
		}
		if r := rooms[from]; r != nil {
			r.Exits[dir] = to
		}
	}
	return exRows.Err()
}

func (p *Pool) loadPrototypes(ctx context.Context, enabled []string, zones map[string]*content.ZoneDTO) error {
	// Items and mobs share the same row shape; the table arg picks which list they land in.
	load := func(table string, mob bool) error {
		rows, err := p.pool.Query(ctx, fmt.Sprintf(
			`SELECT ref, COALESCE(zone_ref, ''), short, long, keywords, body
			   FROM %s WHERE pack = ANY($1) ORDER BY zone_ref, ref`, table), enabled)
		if err != nil {
			return fmt.Errorf("store: query %s: %w", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			var d content.ProtoDTO
			var zoneRef string
			var body []byte
			if err := rows.Scan(&d.Ref, &zoneRef, &d.Short, &d.Long, &d.Keywords, &body); err != nil {
				return fmt.Errorf("store: scan %s: %w", table, err)
			}
			if len(body) > 0 {
				var b protoBody
				if err := json.Unmarshal(body, &b); err != nil {
					return fmt.Errorf("store: %s %s body: %w", table, d.Ref, err)
				}
				d.Physical, d.Wearable, d.Weapon, d.Container = b.Physical, b.Wearable, b.Weapon, b.Container
				d.Living = b.Living
			}
			z := zones[zoneRef]
			if z == nil {
				continue
			}
			if mob {
				z.Mobs = append(z.Mobs, d)
			} else {
				z.Items = append(z.Items, d)
			}
		}
		return rows.Err()
	}
	if err := load("item_prototypes", false); err != nil {
		return err
	}
	return load("mob_prototypes", true)
}

func (p *Pool) loadResets(ctx context.Context, enabled []string, zones map[string]*content.ZoneDTO) error {
	rows, err := p.pool.Query(ctx,
		`SELECT zone_ref, body FROM zone_resets WHERE pack = ANY($1) ORDER BY zone_ref, seq`, enabled)
	if err != nil {
		return fmt.Errorf("store: query resets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var zoneRef string
		var body []byte
		if err := rows.Scan(&zoneRef, &body); err != nil {
			return fmt.Errorf("store: scan reset: %w", err)
		}
		var r content.ResetDTO
		if err := json.Unmarshal(body, &r); err != nil {
			return fmt.Errorf("store: reset body: %w", err)
		}
		if z := zones[zoneRef]; z != nil {
			z.Resets = append(z.Resets, r)
		}
	}
	return rows.Err()
}

// attrBody / resourceBody / dmgBody are the JSONB-tail shapes for the pack-global def rows. They
// mirror the non-relational fields of the content DTOs so a DB round-trip reproduces the same def
// the embedded YAML would. The relational columns (ref/display_name/value_kind/...) stay first-class.
type attrBody struct {
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

type resourceBody struct {
	Regen             int            `json:"regen,omitempty"`
	DepletedThreshold int            `json:"depleted_threshold,omitempty"`
	OnEvent           map[string]any `json:"on_event,omitempty"`    // [G3] event subscriptions (6.2)
	OnDepleted        []any          `json:"on_depleted,omitempty"` // [G-D] death hook (6.3b)
}

type dmgBody struct {
	Color  string             `json:"color,omitempty"`
	Resist map[string]float64 `json:"resist,omitempty"`
}

// affectBody is the JSONB-tail shape for an affect_defs row: the whole content.AffectBodyDTO
// (duration/modifiers/prevents/tick + the reserved on_apply/on_expire/resist hooks). It is the
// same DTO the embedded YAML carries, so a DB round-trip reproduces the same affect.
type affectBody = content.AffectBodyDTO

// combatProfileBody is the JSONB-tail shape for a combat_profile_defs row (Phase 6.3a): the to-hit
// check, the ordered avoidance ladder, and the damage-bonus formula. ref+pack are first-class; the
// SHAPE of every check/formula is content, carried generically (the world mapper parses it), so the
// whole body mirrors content.CombatProfileDTO minus its Ref (which is the relational PK column).
type combatProfileBody struct {
	ToHit       any                    `json:"to_hit,omitempty"`
	Avoidance   []any                  `json:"avoidance,omitempty"`
	DamageBonus content.FormulaNodeDTO `json:"damage_bonus,omitempty"`
}

// packMetaBody is the JSONB-tail shape for a pack_meta row: a pack's global SCALARS (Phase 6.3a:
// just default_combat — the combat profile a player fights with when its prototype names none). One
// row per pack; a future pack-level scalar is a content write here, not a migration.
type packMetaBody struct {
	DefaultCombat string `json:"default_combat,omitempty"`
}

// loadGlobalDefs reads the pack-global attribute/resource/damage-type rows for the enabled packs
// and appends them onto their content.Pack. They are zone-independent, so they group by pack name
// (the `pack` helper), not onto any zone. The `default_base` of an attribute and the JSONB `body`
// of every kind round-trip through the same DTO the embedded loader produces.
func (p *Pool) loadGlobalDefs(ctx context.Context, enabled []string, pack func(string) *content.Pack) error {
	// Attributes.
	rows, err := p.pool.Query(ctx,
		`SELECT ref, pack, display_name, value_kind, COALESCE(default_base, 'null'::jsonb), body
		   FROM attribute_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query attribute_defs: %w", err)
	}
	for rows.Next() {
		var a content.AttributeDTO
		var pk string
		var base, body []byte
		if err := rows.Scan(&a.Ref, &pk, &a.DisplayName, &a.ValueKind, &base, &body); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan attribute_def: %w", err)
		}
		if err := decodeBaseSpec(base, &a.DefaultBase); err != nil {
			rows.Close()
			return fmt.Errorf("store: attribute_def %s base: %w", a.Ref, err)
		}
		if len(body) > 0 {
			var b attrBody
			if err := json.Unmarshal(body, &b); err != nil {
				rows.Close()
				return fmt.Errorf("store: attribute_def %s body: %w", a.Ref, err)
			}
			a.Min, a.Max = b.Min, b.Max
		}
		pp := pack(pk)
		pp.Attributes = append(pp.Attributes, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	// Resources.
	rRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, display_name, COALESCE(max_attr, ''), vital, body
		   FROM resource_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query resource_defs: %w", err)
	}
	for rRows.Next() {
		var r content.ResourceDTO
		var pk string
		var body []byte
		if err := rRows.Scan(&r.Ref, &pk, &r.DisplayName, &r.MaxAttr, &r.Vital, &body); err != nil {
			rRows.Close()
			return fmt.Errorf("store: scan resource_def: %w", err)
		}
		if len(body) > 0 {
			var b resourceBody
			if err := json.Unmarshal(body, &b); err != nil {
				rRows.Close()
				return fmt.Errorf("store: resource_def %s body: %w", r.Ref, err)
			}
			r.Regen, r.DepletedThreshold = b.Regen, b.DepletedThreshold
			r.OnEvent, r.OnDepleted = b.OnEvent, b.OnDepleted
		}
		pp := pack(pk)
		pp.Resources = append(pp.Resources, r)
	}
	if err := rRows.Err(); err != nil {
		return err
	}
	rRows.Close()

	// Damage types.
	dRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, display_name, body
		   FROM damage_type_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query damage_type_defs: %w", err)
	}
	for dRows.Next() {
		var d content.DamageTypeDTO
		var pk string
		var body []byte
		if err := dRows.Scan(&d.Ref, &pk, &d.DisplayName, &body); err != nil {
			dRows.Close()
			return fmt.Errorf("store: scan damage_type_def: %w", err)
		}
		if len(body) > 0 {
			var b dmgBody
			if err := json.Unmarshal(body, &b); err != nil {
				dRows.Close()
				return fmt.Errorf("store: damage_type_def %s body: %w", d.Ref, err)
			}
			d.Color, d.Resist = b.Color, b.Resist
		}
		pp := pack(pk)
		pp.DamageTypes = append(pp.DamageTypes, d)
	}
	if err := dRows.Err(); err != nil {
		return err
	}
	dRows.Close()

	// Affects (Phase 5.2 + 6.4a): the status-effect defs. The first-class columns (stacking/max_stacks/
	// stack_scope/dispellable/category/scope) plus the JSONB `body` (duration/modifiers/prevents/tick).
	// scope ([G13]) is "entity"|"room"; the world mapper reads it as roomScoped (Scope=="room").
	aRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, name, COALESCE(category, ''), stacking, max_stacks,
		        COALESCE(stack_scope, ''), dispellable, COALESCE(scope, ''), body
		   FROM affect_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query affect_defs: %w", err)
	}
	defer aRows.Close()
	for aRows.Next() {
		var af content.AffectDTO
		var pk string
		var body []byte
		if err := aRows.Scan(&af.Ref, &pk, &af.Name, &af.Category, &af.Stacking,
			&af.MaxStacks, &af.StackScope, &af.Dispellable, &af.Scope, &body); err != nil {
			return fmt.Errorf("store: scan affect_def: %w", err)
		}
		if len(body) > 0 {
			var b affectBody
			if err := json.Unmarshal(body, &b); err != nil {
				return fmt.Errorf("store: affect_def %s body: %w", af.Ref, err)
			}
			af.Body = b
		}
		pp := pack(pk)
		pp.Affects = append(pp.Affects, af)
	}
	if err := aRows.Err(); err != nil {
		return err
	}
	aRows.Close()

	// Abilities (Phase 5.3): the skill/spell definitions. The lifecycle metadata (invocation/
	// cast_time/lag/cooldown) + tags are first-class columns; targeting/requires/costs/on_resolve/
	// messages are JSONB (the same DTO sub-shapes the embedded YAML carries). The 00003 schema has no
	// first-class `words` column, so the command-invocation verbs ride the `messages` JSONB under
	// "words" (abilityMessages) — the production seed writes them there; the embedded YAML carries
	// them as a first-class field.
	abRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, name, invocation, tags, targeting, requires, costs,
		        cast_time, lag, cooldown, COALESCE(on_resolve, 'null'::jsonb),
		        COALESCE(on_resolve_lua, ''), COALESCE(messages, '{}'::jsonb)
		   FROM ability_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query ability_defs: %w", err)
	}
	defer abRows.Close()
	for abRows.Next() {
		var ab content.AbilityDTO
		var pk string
		var targeting, requires, costs, onResolve, messages []byte
		if err := abRows.Scan(&ab.Ref, &pk, &ab.Name, &ab.Invocation, &ab.Tags,
			&targeting, &requires, &costs, &ab.CastTime, &ab.Lag, &ab.Cooldown,
			&onResolve, &ab.OnResolveLua, &messages); err != nil {
			return fmt.Errorf("store: scan ability_def: %w", err)
		}
		if err := json.Unmarshal(targeting, &ab.Targeting); err != nil {
			return fmt.Errorf("store: ability_def %s targeting: %w", ab.Ref, err)
		}
		if len(requires) > 0 {
			if err := json.Unmarshal(requires, &ab.Requires); err != nil {
				return fmt.Errorf("store: ability_def %s requires: %w", ab.Ref, err)
			}
		}
		if len(costs) > 0 {
			if err := json.Unmarshal(costs, &ab.Costs); err != nil {
				return fmt.Errorf("store: ability_def %s costs: %w", ab.Ref, err)
			}
		}
		var mb abilityMessages
		if len(messages) > 0 {
			if err := json.Unmarshal(messages, &mb); err != nil {
				return fmt.Errorf("store: ability_def %s messages: %w", ab.Ref, err)
			}
		}
		ab.Messages = mb.AbilityMessagesDTO
		ab.Words = mb.Words
		// on_resolve is a raw op-list (or null); carry it as the decoded generic form the world-side
		// op-list parser consumes, exactly like the affect tick op-list.
		if len(onResolve) > 0 {
			if err := json.Unmarshal(onResolve, &ab.OnResolve); err != nil {
				return fmt.Errorf("store: ability_def %s on_resolve: %w", ab.Ref, err)
			}
		}
		pp := pack(pk)
		pp.Abilities = append(pp.Abilities, ab)
	}
	if err := abRows.Err(); err != nil {
		return err
	}
	abRows.Close()

	// Combat profiles (Phase 6.3a): ref+pack first-class, the to-hit/avoidance/damage SHAPE in the
	// JSONB body. Decoded into the same CombatProfileDTO the embedded YAML carries.
	cpRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM combat_profile_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query combat_profile_defs: %w", err)
	}
	for cpRows.Next() {
		var cp content.CombatProfileDTO
		var pk string
		var body []byte
		if err := cpRows.Scan(&cp.Ref, &pk, &body); err != nil {
			cpRows.Close()
			return fmt.Errorf("store: scan combat_profile_def: %w", err)
		}
		if len(body) > 0 {
			var b combatProfileBody
			if err := json.Unmarshal(body, &b); err != nil {
				cpRows.Close()
				return fmt.Errorf("store: combat_profile_def %s body: %w", cp.Ref, err)
			}
			cp.ToHit, cp.Avoidance, cp.DamageBonus = b.ToHit, b.Avoidance, b.DamageBonus
		}
		pp := pack(pk)
		pp.CombatProfiles = append(pp.CombatProfiles, cp)
	}
	if err := cpRows.Err(); err != nil {
		return err
	}
	cpRows.Close()

	// Pack-level scalars (Phase 6.3a): default_combat from pack_meta, onto its pack. A pack with no
	// row leaves DefaultCombat empty (the loader's "players have no combat profile" default).
	mRows, err := p.pool.Query(ctx,
		`SELECT pack, body FROM pack_meta WHERE pack = ANY($1)`, enabled)
	if err != nil {
		return fmt.Errorf("store: query pack_meta: %w", err)
	}
	defer mRows.Close()
	for mRows.Next() {
		var pk string
		var body []byte
		if err := mRows.Scan(&pk, &body); err != nil {
			return fmt.Errorf("store: scan pack_meta: %w", err)
		}
		if len(body) > 0 {
			var b packMetaBody
			if err := json.Unmarshal(body, &b); err != nil {
				return fmt.Errorf("store: pack_meta %s body: %w", pk, err)
			}
			pack(pk).DefaultCombat = b.DefaultCombat
		}
	}
	return mRows.Err()
}

// abilityMessages is the JSONB-tail shape for the ability_defs `messages` column. The 00003 schema
// has no first-class `words` column, so the command-invocation verbs ride here under "words" (the
// production seed writes them; the embedded YAML carries them as a first-class field). Everything
// else is the act() emit templates.
type abilityMessages struct {
	content.AbilityMessagesDTO
	Words []string `json:"words,omitempty"`
}

// decodeBaseSpec unmarshals an attribute's default_base JSONB into a content.BaseSpecDTO. A JSON
// `null` (no base) leaves it zero. The column is the {"lit":n} | {"expr":<ast>} object the embedded
// YAML carries, so the same DTO drives both sources.
func decodeBaseSpec(raw []byte, out *content.BaseSpecDTO) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, out)
}
