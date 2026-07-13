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
	// Lua is the prototype's optional trigger/scripted source (Phase 7.4c): the `on(event,fn)` +
	// self.state Lua that runs per spawned instance (a greeter mob; a reaction mob like the demo
	// archmage's Counterspell / warden's Shield). It rides the SAME body JSONB. Without it a DB
	// round-trip dropped a scripted prototype's Lua (a content Lua mob became a pure-data mob through
	// Postgres) — the gap the richer demo's first Lua mobs caught, exactly like the Living gap before
	// it. Empty for every pure-data prototype (omitempty keeps their body unchanged).
	Lua string `json:"lua,omitempty"`
	// Bind/Tier/Tags/Material are the item-economy fields (Phase 13.1/13.2): the binding rule
	// (bind_on_pickup/equip), the rarity tier (the no-trade threshold), the free-form tags, and the
	// stackable-material spec. They ride the SAME body JSONB as the other component templates. Without
	// them a DB round-trip dropped the sword's bind/tier/tags and a material's max_stack (a bound rare
	// became an inert tradeable through Postgres) — the gap TestStorePackRoundTrip caught, exactly like
	// the Living/Lua gaps before. Empty/nil for every prototype that declares none (omitempty keeps
	// their body unchanged).
	Bind     string               `json:"bind,omitempty"`
	Tier     string               `json:"tier,omitempty"`
	Tags     []string             `json:"tags,omitempty"`
	Material *content.MaterialDTO `json:"material,omitempty"`
	// SalvageTable/NoSalvage are the #38 salvaging rules (per-item override table + un-salvageable block).
	// They ride the same body JSONB; without them a DB round-trip would drop a quest item's no-salvage flag.
	SalvageTable string `json:"salvage_table,omitempty"`
	NoSalvage    bool   `json:"no_salvage,omitempty"`
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
	case content.KindChannel:
		return p.loadChannelDefinition(ctx, ref, pack)
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
		d.Lua = b.Lua
		d.Bind, d.Tier, d.Tags, d.Material = b.Bind, b.Tier, b.Tags, b.Material
		d.SalvageTable, d.NoSalvage = b.SalvageTable, b.NoSalvage
	}
	return content.Definition{Kind: kind, Ref: ref, Found: true, Proto: d}, nil
}

// loadChannelDefinition fetches one channel_defs row and decodes its JSONB body into a ChannelDTO-
// bearing Definition — the single-ref re-read the hot-reload applier uses for a `channel` invalidation
// (world/reload.go reloadChannel). pgx.ErrNoRows => Found=false (a deleted channel; the reloader then
// removes it from the registry). Mirrors loadProtoDefinition's shape exactly.
func (p *Pool) loadChannelDefinition(ctx context.Context, ref, pack string) (content.Definition, error) {
	var ch content.ChannelDTO
	var body []byte
	err := p.pool.QueryRow(ctx,
		`SELECT ref, body FROM channel_defs WHERE ref = $1 AND pack = $2`, ref, pack).
		Scan(&ch.Ref, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return content.Definition{Kind: content.KindChannel, Ref: ref, Found: false}, nil
	}
	if err != nil {
		return content.Definition{}, fmt.Errorf("store: load channel definition %s: %w", ref, err)
	}
	if len(body) > 0 {
		var b channelBody
		if err := json.Unmarshal(body, &b); err != nil {
			return content.Definition{}, fmt.Errorf("store: channel %s body: %w", ref, err)
		}
		ch.Name, ch.Words, ch.Color, ch.Format = b.Name, b.Words, b.Color, b.Format
		ch.Access, ch.HearAccess, ch.DefaultOn, ch.History = b.Access, b.HearAccess, b.DefaultOn, b.History
	}
	return content.Definition{Kind: content.KindChannel, Ref: ref, Found: true, Channel: ch}, nil
}

func (p *Pool) loadRooms(ctx context.Context, enabled []string, zones map[string]*content.ZoneDTO) error {
	rooms := map[string]*content.RoomDTO{}
	rows, err := p.pool.Query(ctx,
		`SELECT ref, zone_ref, name, COALESCE(sector, ''), COALESCE(body->>'long', ''),
		        COALESCE(body->'flags', '[]'::jsonb), coord
		   FROM rooms WHERE pack = ANY($1) ORDER BY zone_ref, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query rooms: %w", err)
	}
	for rows.Next() {
		var r content.RoomDTO
		var zoneRef string
		var flags, coord []byte
		if err := rows.Scan(&r.Ref, &zoneRef, &r.Name, &r.Sector, &r.Long, &flags, &coord); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan room: %w", err)
		}
		if len(flags) > 0 {
			if err := json.Unmarshal(flags, &r.Flags); err != nil {
				rows.Close()
				return fmt.Errorf("store: room %s flags: %w", r.Ref, err)
			}
		}
		if len(coord) > 0 {
			if err := json.Unmarshal(coord, &r.Coord); err != nil {
				rows.Close()
				return fmt.Errorf("store: room %s coord: %w", r.Ref, err)
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
				d.Lua = b.Lua
				d.Bind, d.Tier, d.Tags, d.Material = b.Bind, b.Tier, b.Tags, b.Material
				d.SalvageTable, d.NoSalvage = b.SalvageTable, b.NoSalvage
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
	// Stat is the player-facing-stat flag (Phase 9.2, GMCP Char.Stats). It rides the schemaless body
	// JSONB so adding it needs no migration; without persisting it a DB round-trip would drop a flagged
	// attribute's stat status (the Living/Lua field-drop class). Empty for every non-stat attribute.
	Stat bool `json:"stat,omitempty"`
}

type resourceBody struct {
	Regen             int               `json:"regen,omitempty"`
	RegenInCombat     bool              `json:"regen_in_combat,omitempty"` // keep regenerating while fighting (default false)
	DepletedThreshold int               `json:"depleted_threshold,omitempty"`
	OnEvent           map[string]any    `json:"on_event,omitempty"`        // [G3] event subscriptions (6.2)
	OnEventLua        map[string]string `json:"on_event_lua,omitempty"`    // [G3] Lua-body event subscriptions (7.4g)
	OnReactionLua     map[string]string `json:"on_reaction_lua,omitempty"` // result-altering reaction hooks (7.9)
	OnDepleted        []any             `json:"on_depleted,omitempty"`     // [G-D] death hook (6.3b)
	PerRound          bool              `json:"per_round,omitempty"`       // [G9] per-round reaction budget (6.4b)
	Gauge             bool              `json:"gauge,omitempty"`           // #50: player-facing HUD pool (Char.Vitals filter)
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

// channelBody is the JSONB-tail shape for a channel_defs row (Phase 8.3): everything that is not the
// relational ref/pack PK. It mirrors content.ChannelDTO minus its Ref, so the whole channel SHAPE
// (verb words, color/format template, access predicate, default_on, history) is content carried in the
// body — the engine names no channel.
type channelBody struct {
	Name       string                    `json:"name,omitempty"`
	Words      []string                  `json:"words,omitempty"`
	Color      string                    `json:"color,omitempty"`
	Format     string                    `json:"format,omitempty"`
	Access     content.ChannelAccessDTO  `json:"access,omitempty"`
	HearAccess *content.ChannelAccessDTO `json:"hear_access,omitempty"` // nil vs empty is load-bearing (hear=speak vs open)
	DefaultOn  bool                      `json:"default_on,omitempty"`
	History    int                       `json:"history,omitempty"`
}

// regionBody is the JSONB-tail shape for a region_defs row (Phase 10.3): everything that is not the
// relational ref/pack PK. It mirrors content.RegionDTO minus its Ref, so the whole region SHAPE (its
// display name + member zone refs) is content carried in the body — the engine names no region.
type regionBody struct {
	Name  string   `json:"name,omitempty"`
	Zones []string `json:"zones,omitempty"`
}

// trackBody is the JSONB-tail shape for a track_defs row (Phase 11.2): everything that is not the ref/pack
// PK. It mirrors content.TrackDTO minus its Ref, so the whole track SHAPE (progress attr, level attr,
// thresholds, per-step grant op-lists) is content carried in the body — the engine names no track.
type trackBody struct {
	ProgressAttr string    `json:"progress_attr,omitempty"`
	LevelAttr    string    `json:"level_attr,omitempty"`
	Thresholds   []float64 `json:"thresholds,omitempty"`
	Steps        []any     `json:"steps,omitempty"`
}

// bundleBody is the JSONB-tail shape for a bundle_defs row (Phase 11.4b): everything that is not the
// ref/pack PK. It mirrors content.BundleDTO minus its Ref, so the bundle SHAPE (its kind + grant op-list)
// is content carried in the body — the engine names no bundle.
type bundleBody struct {
	Kind     string `json:"kind,omitempty"`
	Uncapped bool   `json:"uncapped,omitempty"`
	Grants   any    `json:"grants,omitempty"`
}

// rarityTierBody / lootTableBody are the JSONB-tail shapes for the loot def rows (Phase 12.1), each
// mirroring its DTO minus the Ref — the whole shape (tier order/weight/color; the loot rolls) is content
// in the body. The engine names no tier or table.
type rarityTierBody struct {
	Order  int     `json:"order,omitempty"`
	Weight float64 `json:"weight,omitempty"`
	Color  string  `json:"color,omitempty"`
	Binds  bool    `json:"binds,omitempty"` // Phase 13.4 (D1): a binds tier's items bind on creation (the no-trade sink)
	// #38 slice B: the tier's derived salvage rule (default table + skill requirement + over-skill step).
	SalvageTable     string `json:"salvage_table,omitempty"`
	SalvageSkill     int    `json:"salvage_skill,omitempty"`
	SalvageBonusStep int    `json:"salvage_bonus_step,omitempty"`
}

type lootTableBody struct {
	Rolls  []content.LootRollDTO `json:"rolls,omitempty"`
	OnRoll string                `json:"on_roll,omitempty"` // Phase 12.1 conditional-drop Lua hatch
}

// spawnScheduleBody is the JSONB-tail shape for a spawn_schedule_defs row (Phase 12.4): everything but the
// ref/pack PK — the schedule SHAPE (proto/zone/room/interval/on_missed/announce).
type spawnScheduleBody struct {
	Proto                 string `json:"proto,omitempty"`
	Zone                  string `json:"zone,omitempty"`
	Room                  string `json:"room,omitempty"`
	IntervalAfterDeathSec int    `json:"interval_after_death_sec,omitempty"`
	OnMissed              string `json:"on_missed,omitempty"`
	Announce              string `json:"announce,omitempty"`
}

// recipeBody is the JSONB-tail shape for a recipe_defs row (Phase 13.5): everything but the ref/pack PK —
// the recipe's profession/skill gate, station flag, inputs, output, and quality band, all content.
// affixBody is the JSONB-tail shape for an affix_defs row (#37): everything but the ref/pack PK — the named
// affix's target attribute and its roll [min, max] range.
type affixBody struct {
	Attr string  `json:"attr,omitempty"`
	Min  float64 `json:"min,omitempty"`
	Max  float64 `json:"max,omitempty"`
}

// wearSlotBody is the JSONB-tail shape for a wear_slot_defs row (#35): everything but the ref/pack PK — the
// slot's display label, its display/selection order, and its equip-verb kind (worn/wield/hold).
type wearSlotBody struct {
	Label string `json:"label,omitempty"`
	Order int    `json:"order,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

type recipeBody struct {
	Name        string                   `json:"name,omitempty"`    // #34 discovery display name
	Aliases     []string                 `json:"aliases,omitempty"` // #34 `craft <name>` short names
	Profession  string                   `json:"profession,omitempty"`
	Track       string                   `json:"track,omitempty"`
	Skill       string                   `json:"skill,omitempty"`
	MinSkill    int                      `json:"min_skill,omitempty"`
	Station     string                   `json:"station,omitempty"`
	Inputs      []content.RecipeInputDTO `json:"inputs,omitempty"`
	Output      content.RecipeOutputDTO  `json:"output,omitempty"`
	QualityBase int                      `json:"quality_base,omitempty"`
}

// chargenBody is the JSONB-tail shape for a chargen_defs row (Phase 14.8): the ordered step list, everything
// but the ref/pack PK. The steps are pure content the website renders + validates.
type chargenBody struct {
	Steps []content.ChargenStepDTO `json:"steps,omitempty"`
}

// helpBody is the JSONB-tail shape for a help_defs row (#64): everything but the ref/pack PK — the topic's
// title, category, keyword aliases, body text, and see-also list.
type helpBody struct {
	Title    string   `json:"title,omitempty"`
	Category string   `json:"category,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
	Body     string   `json:"body,omitempty"`
	SeeAlso  []string `json:"see_also,omitempty"`
	MinRank  int      `json:"min_rank,omitempty"` // staff-only visibility gate (#351); 0 = world-readable
}

// displayDefBody is the JSONB-tail shape for a display_defs row: the Lua render body, everything but the
// (pack, surface) PK.
type displayDefBody struct {
	Render string `json:"render,omitempty"`
}

// toggleBody is the JSONB-tail shape for a toggle_defs row (#358): the player-toggle SHAPE (display name,
// verb words, default state, description), everything but the ref/pack PK.
type toggleBody struct {
	Name      string   `json:"name,omitempty"`
	Words     []string `json:"words,omitempty"`
	DefaultOn bool     `json:"default_on,omitempty"`
	Desc      string   `json:"desc,omitempty"`
}

// trustTierBody is the JSONB-tail shape for a trust_tier_defs row (#27/#29, Round 9 Slice 0): the granted
// reserved-flag list, everything but the (pack, name) PK and the first-class rank column.
type trustTierBody struct {
	Flags []string `json:"flags,omitempty"`
}

// commandBody is the JSONB-tail shape for a command_defs row (#20, Phase 7.4e): the alias list + the Lua
// handler body, everything but the (pack, verb) PK.
type commandBody struct {
	Aliases []string `json:"aliases,omitempty"`
	Lua     string   `json:"lua,omitempty"`
}

// formulaBody is the JSONB-tail shape for a formula_defs row (#20, Phase 7.4f): the Lua formula body,
// everything but the (pack, name) PK.
type formulaBody struct {
	Lua string `json:"lua,omitempty"`
}

// packMetaBody is the JSONB-tail shape for a pack_meta row: a pack's global SCALARS (Phase 6.3a:
// default_combat — the combat profile a player fights with when its prototype names none; #20: pvp_lua —
// the pack PvP-policy hook, Phase 7.4f). One row per pack; a future pack-level scalar is a content write
// here, not a migration.
type packMetaBody struct {
	DefaultCombat string `json:"default_combat,omitempty"`
	PvpLua        string `json:"pvp_lua,omitempty"`
	WorldScript   string `json:"world_script,omitempty"` // #47: the world-director Lua signal-handler body
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
			a.Min, a.Max, a.Stat = b.Min, b.Max, b.Stat
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
			r.RegenInCombat = b.RegenInCombat
			r.OnEvent, r.OnDepleted = b.OnEvent, b.OnDepleted
			r.OnEventLua, r.OnReactionLua = b.OnEventLua, b.OnReactionLua
			r.PerRound = b.PerRound
			r.Gauge = b.Gauge
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
		ab.RequiresGrant = mb.RequiresGrant // Phase 11.4a ownership flag, ridden in the messages JSONB
		ab.Skill = mb.Skill                 // Phase 11.3 skill-track tag, ridden in the messages JSONB
		ab.OnEvent = mb.OnEvent             // [G3] event subscriptions, ridden in the messages JSONB
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

	// Channels (Phase 8.3): ref+pack first-class, the channel SHAPE (verb/color/format/access/...) in
	// the JSONB body. Decoded into the same ChannelDTO the embedded YAML carries, so the world side
	// registers them identically whether the pack came from YAML or Postgres.
	chRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM channel_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query channel_defs: %w", err)
	}
	for chRows.Next() {
		var ch content.ChannelDTO
		var pk string
		var body []byte
		if err := chRows.Scan(&ch.Ref, &pk, &body); err != nil {
			chRows.Close()
			return fmt.Errorf("store: scan channel_def: %w", err)
		}
		if len(body) > 0 {
			var b channelBody
			if err := json.Unmarshal(body, &b); err != nil {
				chRows.Close()
				return fmt.Errorf("store: channel_def %s body: %w", ch.Ref, err)
			}
			ch.Name, ch.Words, ch.Color, ch.Format = b.Name, b.Words, b.Color, b.Format
			ch.Access, ch.HearAccess, ch.DefaultOn, ch.History = b.Access, b.HearAccess, b.DefaultOn, b.History
		}
		pp := pack(pk)
		pp.Channels = append(pp.Channels, ch)
	}
	if err := chRows.Err(); err != nil {
		return err
	}
	chRows.Close()

	// Regions (Phase 10.3): ref+pack first-class, the region SHAPE (name + member zone refs) in the JSONB
	// body. Decoded into the same RegionDTO the embedded YAML carries, so the director/zone wiring sees
	// regions identically whether the pack came from YAML or Postgres.
	rgRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM region_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query region_defs: %w", err)
	}
	for rgRows.Next() {
		var rg content.RegionDTO
		var pk string
		var body []byte
		if err := rgRows.Scan(&rg.Ref, &pk, &body); err != nil {
			rgRows.Close()
			return fmt.Errorf("store: scan region_def: %w", err)
		}
		if len(body) > 0 {
			var b regionBody
			if err := json.Unmarshal(body, &b); err != nil {
				rgRows.Close()
				return fmt.Errorf("store: region_def %s body: %w", rg.Ref, err)
			}
			rg.Name, rg.Zones = b.Name, b.Zones
		}
		pp := pack(pk)
		pp.Regions = append(pp.Regions, rg)
	}
	if err := rgRows.Err(); err != nil {
		return err
	}
	rgRows.Close()

	// Tracks (Phase 11.2): ref+pack first-class, the track SHAPE (progress/level attrs, thresholds, per-
	// step grant op-lists) in the JSONB body. Decoded into the same TrackDTO the embedded YAML carries, so
	// the world side registers them identically whether the pack came from YAML or Postgres.
	trRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM track_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query track_defs: %w", err)
	}
	for trRows.Next() {
		var tr content.TrackDTO
		var pk string
		var body []byte
		if err := trRows.Scan(&tr.Ref, &pk, &body); err != nil {
			trRows.Close()
			return fmt.Errorf("store: scan track_def: %w", err)
		}
		if len(body) > 0 {
			var b trackBody
			if err := json.Unmarshal(body, &b); err != nil {
				trRows.Close()
				return fmt.Errorf("store: track_def %s body: %w", tr.Ref, err)
			}
			tr.ProgressAttr, tr.LevelAttr, tr.Thresholds, tr.Steps = b.ProgressAttr, b.LevelAttr, b.Thresholds, b.Steps
		}
		pp := pack(pk)
		pp.Tracks = append(pp.Tracks, tr)
	}
	if err := trRows.Err(); err != nil {
		return err
	}
	trRows.Close()

	// Bundles (Phase 11.4b): ref+pack first-class, the bundle SHAPE (kind + grant op-list) in the JSONB
	// body. Decoded into the same BundleDTO the embedded YAML carries, so the world side registers them
	// identically whether the pack came from YAML or Postgres.
	bnRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM bundle_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query bundle_defs: %w", err)
	}
	for bnRows.Next() {
		var bn content.BundleDTO
		var pk string
		var body []byte
		if err := bnRows.Scan(&bn.Ref, &pk, &body); err != nil {
			bnRows.Close()
			return fmt.Errorf("store: scan bundle_def: %w", err)
		}
		if len(body) > 0 {
			var b bundleBody
			if err := json.Unmarshal(body, &b); err != nil {
				bnRows.Close()
				return fmt.Errorf("store: bundle_def %s body: %w", bn.Ref, err)
			}
			bn.Kind, bn.Uncapped, bn.Grants = b.Kind, b.Uncapped, b.Grants
		}
		pp := pack(pk)
		pp.Bundles = append(pp.Bundles, bn)
	}
	if err := bnRows.Err(); err != nil {
		return err
	}
	bnRows.Close()

	// Rarity tiers (Phase 12.1): ref+pack first-class, the tier shape (order/weight/color) in the body.
	rtRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM rarity_tier_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query rarity_tier_defs: %w", err)
	}
	for rtRows.Next() {
		var rt content.RarityTierDTO
		var pk string
		var body []byte
		if err := rtRows.Scan(&rt.Ref, &pk, &body); err != nil {
			rtRows.Close()
			return fmt.Errorf("store: scan rarity_tier_def: %w", err)
		}
		if len(body) > 0 {
			var b rarityTierBody
			if err := json.Unmarshal(body, &b); err != nil {
				rtRows.Close()
				return fmt.Errorf("store: rarity_tier_def %s body: %w", rt.Ref, err)
			}
			rt.Order, rt.Weight, rt.Color, rt.Binds = b.Order, b.Weight, b.Color, b.Binds
			rt.SalvageTable, rt.SalvageSkill, rt.SalvageBonusStep = b.SalvageTable, b.SalvageSkill, b.SalvageBonusStep
		}
		pack(pk).RarityTiers = append(pack(pk).RarityTiers, rt)
	}
	if err := rtRows.Err(); err != nil {
		return err
	}
	rtRows.Close()

	// Named affixes (#37): ref+pack first-class, the attr + roll range in the JSONB body.
	afRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM affix_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query affix_defs: %w", err)
	}
	for afRows.Next() {
		var af content.AffixDefDTO
		var pk string
		var body []byte
		if err := afRows.Scan(&af.Ref, &pk, &body); err != nil {
			afRows.Close()
			return fmt.Errorf("store: scan affix_def: %w", err)
		}
		if len(body) > 0 {
			var b affixBody
			if err := json.Unmarshal(body, &b); err != nil {
				afRows.Close()
				return fmt.Errorf("store: affix_def %s body: %w", af.Ref, err)
			}
			af.Attr, af.Min, af.Max = b.Attr, b.Min, b.Max
		}
		pack(pk).Affixes = append(pack(pk).Affixes, af)
	}
	if err := afRows.Err(); err != nil {
		return err
	}
	afRows.Close()

	// Loot tables (Phase 12.1): ref+pack first-class, the rolls in the JSONB body.
	ltRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM loot_table_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query loot_table_defs: %w", err)
	}
	for ltRows.Next() {
		var lt content.LootTableDTO
		var pk string
		var body []byte
		if err := ltRows.Scan(&lt.Ref, &pk, &body); err != nil {
			ltRows.Close()
			return fmt.Errorf("store: scan loot_table_def: %w", err)
		}
		if len(body) > 0 {
			var b lootTableBody
			if err := json.Unmarshal(body, &b); err != nil {
				ltRows.Close()
				return fmt.Errorf("store: loot_table_def %s body: %w", lt.Ref, err)
			}
			lt.Rolls = b.Rolls
			lt.OnRoll = b.OnRoll
		}
		pack(pk).LootTables = append(pack(pk).LootTables, lt)
	}
	if err := ltRows.Err(); err != nil {
		return err
	}
	ltRows.Close()

	// Spawn schedules (Phase 12.4): ref+pack first-class, the schedule shape in the JSONB body.
	scRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM spawn_schedule_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query spawn_schedule_defs: %w", err)
	}
	for scRows.Next() {
		var sc content.SpawnScheduleDTO
		var pk string
		var body []byte
		if err := scRows.Scan(&sc.Ref, &pk, &body); err != nil {
			scRows.Close()
			return fmt.Errorf("store: scan spawn_schedule_def: %w", err)
		}
		if len(body) > 0 {
			var b spawnScheduleBody
			if err := json.Unmarshal(body, &b); err != nil {
				scRows.Close()
				return fmt.Errorf("store: spawn_schedule_def %s body: %w", sc.Ref, err)
			}
			sc.Proto, sc.Zone, sc.Room = b.Proto, b.Zone, b.Room
			sc.IntervalAfterDeathSec, sc.OnMissed, sc.Announce = b.IntervalAfterDeathSec, b.OnMissed, b.Announce
		}
		pack(pk).SpawnSchedules = append(pack(pk).SpawnSchedules, sc)
	}
	if err := scRows.Err(); err != nil {
		return err
	}
	scRows.Close()

	// Recipes (Phase 13.5): ref+pack first-class, the recipe shape in the JSONB body.
	rcRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM recipe_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query recipe_defs: %w", err)
	}
	for rcRows.Next() {
		var rc content.RecipeDTO
		var pk string
		var body []byte
		if err := rcRows.Scan(&rc.Ref, &pk, &body); err != nil {
			rcRows.Close()
			return fmt.Errorf("store: scan recipe_def: %w", err)
		}
		if len(body) > 0 {
			var b recipeBody
			if err := json.Unmarshal(body, &b); err != nil {
				rcRows.Close()
				return fmt.Errorf("store: recipe_def %s body: %w", rc.Ref, err)
			}
			rc.Name, rc.Aliases = b.Name, b.Aliases
			rc.Profession, rc.Track, rc.Skill, rc.MinSkill, rc.Station = b.Profession, b.Track, b.Skill, b.MinSkill, b.Station
			rc.Inputs, rc.Output, rc.QualityBase = b.Inputs, b.Output, b.QualityBase
		}
		pack(pk).Recipes = append(pack(pk).Recipes, rc)
	}
	if err := rcRows.Err(); err != nil {
		return err
	}
	rcRows.Close()

	// Wear slots (#35): ref+pack first-class, the label/order/kind in the JSONB body.
	wsRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM wear_slot_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query wear_slot_defs: %w", err)
	}
	for wsRows.Next() {
		var ws content.WearSlotDTO
		var pk string
		var body []byte
		if err := wsRows.Scan(&ws.Ref, &pk, &body); err != nil {
			wsRows.Close()
			return fmt.Errorf("store: scan wear_slot_def: %w", err)
		}
		if len(body) > 0 {
			var b wearSlotBody
			if err := json.Unmarshal(body, &b); err != nil {
				wsRows.Close()
				return fmt.Errorf("store: wear_slot_def %s body: %w", ws.Ref, err)
			}
			ws.Label, ws.Order, ws.Kind = b.Label, b.Order, b.Kind
		}
		pack(pk).WearSlots = append(pack(pk).WearSlots, ws)
	}
	if err := wsRows.Err(); err != nil {
		return err
	}
	wsRows.Close()

	// Chargens (Phase 14.8): ref+pack first-class, the step list in the JSONB body.
	cgRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM chargen_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query chargen_defs: %w", err)
	}
	for cgRows.Next() {
		var cg content.ChargenDTO
		var pk string
		var body []byte
		if err := cgRows.Scan(&cg.Ref, &pk, &body); err != nil {
			cgRows.Close()
			return fmt.Errorf("store: scan chargen_def: %w", err)
		}
		if len(body) > 0 {
			var b chargenBody
			if err := json.Unmarshal(body, &b); err != nil {
				cgRows.Close()
				return fmt.Errorf("store: chargen_def %s body: %w", cg.Ref, err)
			}
			cg.Steps = b.Steps
		}
		pack(pk).Chargens = append(pack(pk).Chargens, cg)
	}
	if err := cgRows.Err(); err != nil {
		return err
	}
	cgRows.Close()

	// Help topics (#64): ref+pack first-class, the title/category/keywords/body/see_also in the JSONB body.
	hdRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM help_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query help_defs: %w", err)
	}
	for hdRows.Next() {
		var hd content.HelpDTO
		var pk string
		var body []byte
		if err := hdRows.Scan(&hd.Ref, &pk, &body); err != nil {
			hdRows.Close()
			return fmt.Errorf("store: scan help_def: %w", err)
		}
		if len(body) > 0 {
			var b helpBody
			if err := json.Unmarshal(body, &b); err != nil {
				hdRows.Close()
				return fmt.Errorf("store: help_def %s body: %w", hd.Ref, err)
			}
			hd.Title, hd.Category, hd.Keywords = b.Title, b.Category, b.Keywords
			hd.Body, hd.SeeAlso, hd.MinRank = b.Body, b.SeeAlso, b.MinRank
		}
		pack(pk).HelpDefs = append(pack(pk).HelpDefs, hd)
	}
	if err := hdRows.Err(); err != nil {
		return err
	}
	hdRows.Close()

	// Player toggles (#358): ref+pack first-class, the name/words/default_on/desc in the JSONB body.
	tgRows, err := p.pool.Query(ctx,
		`SELECT ref, pack, body FROM toggle_defs WHERE pack = ANY($1) ORDER BY pack, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query toggle_defs: %w", err)
	}
	for tgRows.Next() {
		var tg content.ToggleDTO
		var pk string
		var body []byte
		if err := tgRows.Scan(&tg.Ref, &pk, &body); err != nil {
			tgRows.Close()
			return fmt.Errorf("store: scan toggle_def: %w", err)
		}
		if len(body) > 0 {
			var b toggleBody
			if err := json.Unmarshal(body, &b); err != nil {
				tgRows.Close()
				return fmt.Errorf("store: toggle_def %s body: %w", tg.Ref, err)
			}
			tg.Name, tg.Words, tg.DefaultOn, tg.Desc = b.Name, b.Words, b.DefaultOn, b.Desc
		}
		pack(pk).ToggleDefs = append(pack(pk).ToggleDefs, tg)
	}
	if err := tgRows.Err(); err != nil {
		return err
	}
	tgRows.Close()

	// Display templates: (pack, surface) first-class, the Lua render body in the JSONB body. Ordered by
	// (pack, surface) for deterministic load; the loader's per-pack accumulation applies last-write-wins.
	ddRows, err := p.pool.Query(ctx,
		`SELECT surface, pack, body FROM display_defs WHERE pack = ANY($1) ORDER BY pack, surface`, enabled)
	if err != nil {
		return fmt.Errorf("store: query display_defs: %w", err)
	}
	for ddRows.Next() {
		var dd content.DisplayDefDTO
		var pk string
		var body []byte
		if err := ddRows.Scan(&dd.Surface, &pk, &body); err != nil {
			ddRows.Close()
			return fmt.Errorf("store: scan display_def: %w", err)
		}
		if len(body) > 0 {
			var b displayDefBody
			if err := json.Unmarshal(body, &b); err != nil {
				ddRows.Close()
				return fmt.Errorf("store: display_def %s body: %w", dd.Surface, err)
			}
			dd.Render = b.Render
		}
		pack(pk).DisplayDefs = append(pack(pk).DisplayDefs, dd)
	}
	if err := ddRows.Err(); err != nil {
		return err
	}
	ddRows.Close()

	// Trust tiers (#27/#29, Round 9 Slice 0): (pack, name) first-class + rank column, the granted-flag list
	// in the JSONB body. Ordered by (pack, rank, name) for deterministic load; the loader's per-pack
	// accumulation applies last-write-wins by name.
	ttRows, err := p.pool.Query(ctx,
		`SELECT name, pack, rank, body FROM trust_tier_defs WHERE pack = ANY($1) ORDER BY pack, rank, name`, enabled)
	if err != nil {
		return fmt.Errorf("store: query trust_tier_defs: %w", err)
	}
	for ttRows.Next() {
		var tt content.TrustTierDTO
		var pk string
		var body []byte
		if err := ttRows.Scan(&tt.Name, &pk, &tt.Rank, &body); err != nil {
			ttRows.Close()
			return fmt.Errorf("store: scan trust_tier_def: %w", err)
		}
		if len(body) > 0 {
			var b trustTierBody
			if err := json.Unmarshal(body, &b); err != nil {
				ttRows.Close()
				return fmt.Errorf("store: trust_tier_def %s body: %w", tt.Name, err)
			}
			tt.Flags = b.Flags
		}
		pack(pk).TrustTiers = append(pack(pk).TrustTiers, tt)
	}
	if err := ttRows.Err(); err != nil {
		return err
	}
	ttRows.Close()

	// Custom Lua verbs (#20, Phase 7.4e): (pack, verb) first-class, the alias list + Lua handler in the JSONB
	// body. Ordered by (pack, verb) for deterministic load; the loader accumulates verbs across packs.
	cmdRows, err := p.pool.Query(ctx,
		`SELECT verb, pack, body FROM command_defs WHERE pack = ANY($1) ORDER BY pack, verb`, enabled)
	if err != nil {
		return fmt.Errorf("store: query command_defs: %w", err)
	}
	for cmdRows.Next() {
		var cmd content.CommandDTO
		var pk string
		var body []byte
		if err := cmdRows.Scan(&cmd.Verb, &pk, &body); err != nil {
			cmdRows.Close()
			return fmt.Errorf("store: scan command_def: %w", err)
		}
		if len(body) > 0 {
			var b commandBody
			if err := json.Unmarshal(body, &b); err != nil {
				cmdRows.Close()
				return fmt.Errorf("store: command_def %s body: %w", cmd.Verb, err)
			}
			cmd.Aliases = b.Aliases
			cmd.Lua = b.Lua
		}
		pack(pk).Commands = append(pack(pk).Commands, cmd)
	}
	if err := cmdRows.Err(); err != nil {
		return err
	}
	cmdRows.Close()

	// Ruleset-formula overrides (#20, Phase 7.4f): (pack, name) first-class, the Lua formula body in the JSONB
	// body. Ordered by (pack, name) for deterministic load; reconstructed into each pack's Formulas map.
	fRows, err := p.pool.Query(ctx,
		`SELECT name, pack, body FROM formula_defs WHERE pack = ANY($1) ORDER BY pack, name`, enabled)
	if err != nil {
		return fmt.Errorf("store: query formula_defs: %w", err)
	}
	for fRows.Next() {
		var name, pk string
		var body []byte
		if err := fRows.Scan(&name, &pk, &body); err != nil {
			fRows.Close()
			return fmt.Errorf("store: scan formula_def: %w", err)
		}
		var b formulaBody
		if len(body) > 0 {
			if err := json.Unmarshal(body, &b); err != nil {
				fRows.Close()
				return fmt.Errorf("store: formula_def %s body: %w", name, err)
			}
		}
		if pack(pk).Formulas == nil {
			pack(pk).Formulas = map[string]string{}
		}
		pack(pk).Formulas[name] = b.Lua
	}
	if err := fRows.Err(); err != nil {
		return err
	}
	fRows.Close()

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
			pack(pk).PvpLua = b.PvpLua
			pack(pk).WorldScript = b.WorldScript
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
	// RequiresGrant (Phase 11.4a) + Skill (Phase 11.3) are top-level AbilityDTO fields with no column of
	// their own; they ride THIS messages JSONB wrapper exactly like Words, so a DB round-trip preserves them
	// without a schema migration. The store path dropped both until the first demo ability set them (the
	// Phase-13.3 craft verb) — the gap TestStorePackRoundTrip then caught. Empty/false for an ability that
	// sets neither (omitempty keeps every existing ability's body byte-identical).
	RequiresGrant bool   `json:"requires_grant,omitempty"`
	Skill         string `json:"skill,omitempty"`
	// OnEvent ([G3], Phase 6.2) is a top-level AbilityDTO field with no first-class column; it rides THIS
	// messages JSONB wrapper like Words/Skill/RequiresGrant so a DB round-trip preserves an ability's event
	// subscriptions without a schema migration. omitempty keeps every existing ability's body byte-identical.
	OnEvent map[string]any `json:"on_event,omitempty"`
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
