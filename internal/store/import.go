package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/double-nibble/telosmud/internal/content"
)

// ImportPack is the content WRITE path used by `make seed` (cmd/telos-seed): it loads a parsed
// content.Pack (from the same embedded YAML the tests use) into the definition rows. It is the
// "import (file->rows)" half of decision D4; export (rows->file) is a later concern. Seeding
// is idempotent per pack: it DELETEs the pack's existing rows then re-inserts, so re-running
// `make seed` is safe and a pack edit fully replaces the old content. The whole import runs in
// one transaction (PERSISTENCE.md §1: strip/replace is one transaction).
func (p *Pool) ImportPack(ctx context.Context, pk content.Pack) error {
	if pk.Pack == "" {
		return fmt.Errorf("store: import pack with empty name")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin import tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	if err := deletePack(ctx, tx, pk.Pack); err != nil {
		return err
	}
	// Insert in FK-safe phases ACROSS all zones: zones, then rooms, then exits (a cross-zone
	// exit like market.north -> darkwood:room:grove references a room in a different zone, so
	// every room must exist before any exit is inserted), then prototypes and resets.
	for _, z := range pk.Zones {
		if err := insertZone(ctx, tx, pk.Pack, z); err != nil {
			return err
		}
	}
	for _, z := range pk.Zones {
		if err := insertRooms(ctx, tx, pk.Pack, z); err != nil {
			return err
		}
	}
	for _, z := range pk.Zones {
		if err := insertExits(ctx, tx, z); err != nil {
			return err
		}
	}
	for _, z := range pk.Zones {
		if err := insertProtosAndResets(ctx, tx, pk.Pack, z); err != nil {
			return err
		}
	}
	// Pack-GLOBAL defs (Phase 5.1): zone-independent rows, inserted after the zone tree (no FK to it).
	if err := insertGlobalDefs(ctx, tx, pk); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit import: %w", err)
	}
	return nil
}

// deletePack removes every definition row belonging to a pack, in FK-safe order, so a re-seed
// fully replaces it (and `DELETE WHERE pack=...` is the bare-engine strip helper).
func deletePack(ctx context.Context, tx pgx.Tx, pack string) error {
	// Children first: exits reference rooms, resets/rooms/prototypes reference zones.
	stmts := []string{
		`DELETE FROM exits WHERE from_room IN (SELECT ref FROM rooms WHERE pack=$1)`,
		`DELETE FROM zone_resets WHERE pack=$1`,
		`DELETE FROM item_prototypes WHERE pack=$1`,
		`DELETE FROM mob_prototypes WHERE pack=$1`,
		`DELETE FROM rooms WHERE pack=$1`,
		`DELETE FROM zones WHERE pack=$1`,
		// Pack-global defs (Phase 5.1): no FK into the zone tree, so order-independent.
		`DELETE FROM attribute_defs WHERE pack=$1`,
		`DELETE FROM resource_defs WHERE pack=$1`,
		`DELETE FROM damage_type_defs WHERE pack=$1`,
		`DELETE FROM affect_defs WHERE pack=$1`,
		`DELETE FROM ability_defs WHERE pack=$1`,
		// Combat content (Phase 6.3a): the profiles and the pack's default_combat scalar. MUST be
		// stripped on a re-seed so a second `make seed`/`make up` strips-and-replaces rather than
		// colliding on a duplicate ref (the ability_defs idempotency regression — covered by
		// TestImportPackIdempotent). No FK into the zone tree, so order-independent. (The mob `living`
		// block rides mob_prototypes.body and is cleared with the mob_prototypes delete above.)
		`DELETE FROM combat_profile_defs WHERE pack=$1`,
		`DELETE FROM pack_meta WHERE pack=$1`,
		// Channels (Phase 8.3): stripped on re-seed so a second import strips-and-replaces rather than
		// colliding on a duplicate ref (the same idempotency discipline as the other def tables). No FK
		// into the zone tree, so order-independent.
		`DELETE FROM channel_defs WHERE pack=$1`,
		// Regions (Phase 10.3): stripped on re-seed, same strips-and-replaces idempotency. No FK into the
		// zone tree (a region just NAMES member zone refs as data), so order-independent.
		`DELETE FROM region_defs WHERE pack=$1`,
		// Tracks (Phase 11.2): stripped on re-seed, same strips-and-replaces idempotency. No FK into the
		// zone tree (a track names attribute refs as data), so order-independent.
		`DELETE FROM track_defs WHERE pack=$1`,
		// Bundles (Phase 11.4b): same strips-and-replaces idempotency. No FK into the zone tree.
		`DELETE FROM bundle_defs WHERE pack=$1`,
		// Loot (Phase 12.1): rarity tiers + loot tables, same strips-and-replaces idempotency.
		`DELETE FROM rarity_tier_defs WHERE pack=$1`,
		// Named affixes (#37): same strips-and-replaces idempotency.
		`DELETE FROM affix_defs WHERE pack=$1`,
		`DELETE FROM loot_table_defs WHERE pack=$1`,
		// Spawn schedules (Phase 12.4): same strips-and-replaces idempotency.
		`DELETE FROM spawn_schedule_defs WHERE pack=$1`,
		// Recipes (Phase 13.5): same strips-and-replaces idempotency.
		`DELETE FROM recipe_defs WHERE pack=$1`,
		// Wear slots (#35): same strips-and-replaces idempotency.
		`DELETE FROM wear_slot_defs WHERE pack=$1`,
		// Chargens (Phase 14.8): same strips-and-replaces idempotency.
		`DELETE FROM chargen_defs WHERE pack=$1`,
		// Display templates: same strips-and-replaces idempotency.
		`DELETE FROM display_defs WHERE pack=$1`,
		// Trust tiers (#27/#29): same strips-and-replaces idempotency. No FK into the zone tree.
		`DELETE FROM trust_tier_defs WHERE pack=$1`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s, pack); err != nil {
			return fmt.Errorf("store: delete pack rows: %w", err)
		}
	}
	return nil
}

func insertZone(ctx context.Context, tx pgx.Tx, pack string, z content.ZoneDTO) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO zones (ref, pack, name, start_room, reset_secs) VALUES ($1,$2,$3,$4,$5)`,
		z.Ref, pack, z.Name, nullStr(z.StartRoom), z.ResetSecs); err != nil {
		return fmt.Errorf("store: insert zone %s: %w", z.Ref, err)
	}
	return nil
}

// insertRooms inserts a zone's rooms. The `long` text lives in the room body JSONB (the
// content read path reads it back via body->>'long').
func insertRooms(ctx context.Context, tx pgx.Tx, pack string, z content.ZoneDTO) error {
	for _, r := range z.Rooms {
		rb := map[string]any{"long": r.Long}
		if len(r.Flags) > 0 {
			rb["flags"] = r.Flags
		}
		body, _ := json.Marshal(rb)
		var coord []byte // the [x,y,z] minimap position rides the dedicated coord JSONB column (Phase 9.3b)
		if len(r.Coord) > 0 {
			coord, _ = json.Marshal(r.Coord)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO rooms (ref, pack, zone_ref, name, sector, coord, body) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			r.Ref, pack, z.Ref, r.Name, nullStr(r.Sector), coord, body); err != nil {
			return fmt.Errorf("store: insert room %s: %w", r.Ref, err)
		}
	}
	return nil
}

// insertExits inserts a zone's exits. Called only after ALL rooms (every zone) are inserted,
// so a cross-zone to_room FK resolves.
func insertExits(ctx context.Context, tx pgx.Tx, z content.ZoneDTO) error {
	for _, r := range z.Rooms {
		for dir, to := range r.Exits {
			if _, err := tx.Exec(ctx,
				`INSERT INTO exits (from_room, dir, to_room) VALUES ($1,$2,$3)`,
				r.Ref, dir, to); err != nil {
				return fmt.Errorf("store: insert exit %s/%s: %w", r.Ref, dir, err)
			}
		}
	}
	return nil
}

func insertProtosAndResets(ctx context.Context, tx pgx.Tx, pack string, z content.ZoneDTO) error {
	if err := insertProtos(ctx, tx, "item_prototypes", pack, z.Ref, z.Items); err != nil {
		return err
	}
	if err := insertProtos(ctx, tx, "mob_prototypes", pack, z.Ref, z.Mobs); err != nil {
		return err
	}
	for i, rst := range z.Resets {
		body, _ := json.Marshal(rst)
		if _, err := tx.Exec(ctx,
			`INSERT INTO zone_resets (pack, zone_ref, seq, body) VALUES ($1,$2,$3,$4)`,
			pack, z.Ref, i, body); err != nil {
			return fmt.Errorf("store: insert reset %s#%d: %w", z.Ref, i, err)
		}
	}
	return nil
}

func insertProtos(ctx context.Context, tx pgx.Tx, table, pack, zoneRef string, protos []content.ProtoDTO) error {
	for _, d := range protos {
		body, err := json.Marshal(protoBody{
			Physical: d.Physical, Wearable: d.Wearable, Weapon: d.Weapon, Container: d.Container,
			Living: d.Living, // mob stat sheet + combat_profile ref (Phase 6.3a) rides the body JSONB
			Lua:    d.Lua,    // the prototype's trigger/scripted Lua source (Phase 7.4c) rides the body JSONB too
			// Item-economy fields (Phase 13.1/13.2): bind rule, rarity tier, tags, material spec.
			Bind: d.Bind, Tier: d.Tier, Tags: d.Tags, Material: d.Material,
			// #38 salvaging: per-item override table + un-salvageable block.
			SalvageTable: d.SalvageTable, NoSalvage: d.NoSalvage,
		})
		if err != nil {
			return fmt.Errorf("store: marshal %s %s body: %w", table, d.Ref, err)
		}
		kw := d.Keywords
		if kw == nil {
			kw = []string{}
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s (ref, pack, zone_ref, short, long, keywords, body)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`, table),
			d.Ref, pack, zoneRef, d.Short, d.Long, kw, body); err != nil {
			return fmt.Errorf("store: insert %s %s: %w", table, d.Ref, err)
		}
	}
	return nil
}

// insertGlobalDefs writes a pack's zone-independent attribute/resource/damage-type rows (Phase 5.1).
// The relational columns stay first-class; min/max (attributes), regen/depleted_threshold
// (resources), color/resist (damage types) ride the JSONB `body` — the same split the loaders read.
// default_base is the {lit}|{expr} spec marshalled straight into its own JSONB column.
func insertGlobalDefs(ctx context.Context, tx pgx.Tx, pk content.Pack) error {
	for _, a := range pk.Attributes {
		base, err := json.Marshal(a.DefaultBase)
		if err != nil {
			return fmt.Errorf("store: marshal attribute %s base: %w", a.Ref, err)
		}
		body, _ := json.Marshal(attrBody{Min: a.Min, Max: a.Max, Stat: a.Stat})
		if _, err := tx.Exec(ctx,
			`INSERT INTO attribute_defs (ref, pack, display_name, value_kind, default_base, body)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			a.Ref, pk.Pack, a.DisplayName, a.ValueKind, base, body); err != nil {
			return fmt.Errorf("store: insert attribute %s: %w", a.Ref, err)
		}
	}
	for _, r := range pk.Resources {
		body, _ := json.Marshal(resourceBody{
			Regen: r.Regen, RegenInCombat: r.RegenInCombat, DepletedThreshold: r.DepletedThreshold,
			OnEvent: r.OnEvent, OnEventLua: r.OnEventLua, OnReactionLua: r.OnReactionLua,
			OnDepleted: r.OnDepleted, PerRound: r.PerRound, Gauge: r.Gauge,
		})
		if _, err := tx.Exec(ctx,
			`INSERT INTO resource_defs (ref, pack, display_name, max_attr, vital, body)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			r.Ref, pk.Pack, r.DisplayName, nullStr(r.MaxAttr), r.Vital, body); err != nil {
			return fmt.Errorf("store: insert resource %s: %w", r.Ref, err)
		}
	}
	for _, d := range pk.DamageTypes {
		body, _ := json.Marshal(dmgBody{Color: d.Color, Resist: d.Resist})
		if _, err := tx.Exec(ctx,
			`INSERT INTO damage_type_defs (ref, pack, display_name, body) VALUES ($1,$2,$3,$4)`,
			d.Ref, pk.Pack, d.DisplayName, body); err != nil {
			return fmt.Errorf("store: insert damage type %s: %w", d.Ref, err)
		}
	}
	for _, a := range pk.Affects {
		body, err := json.Marshal(a.Body)
		if err != nil {
			return fmt.Errorf("store: marshal affect %s body: %w", a.Ref, err)
		}
		scope := a.StackScope
		if scope == "" {
			scope = "source"
		}
		stacking := a.Stacking
		if stacking == "" {
			stacking = "refresh"
		}
		maxStacks := a.MaxStacks
		if maxStacks < 1 {
			maxStacks = 1
		}
		// Scope ([G13], Phase 6.4a): "entity" (default) | "room". A first-class column like stack_scope;
		// an empty DTO value (a pre-6.4a / entity-scoped affect) normalizes to "entity" so the stored row
		// is explicit and the loader's roomScoped mapping (Scope=="room") stays correct.
		affectScope := a.Scope
		if affectScope == "" {
			affectScope = "entity"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO affect_defs (ref, pack, name, category, stacking, max_stacks, stack_scope, dispellable, scope, body)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			a.Ref, pk.Pack, a.Name, nullStr(a.Category), stacking, maxStacks, scope, a.Dispellable, affectScope, body); err != nil {
			return fmt.Errorf("store: insert affect %s: %w", a.Ref, err)
		}
	}
	// Abilities (Phase 5.3): targeting/requires/costs/on_resolve are their own JSONB columns; the
	// command verbs ride the `messages` JSONB under "words" (the 00003 schema has no words column),
	// alongside the act() emit templates. The loader (loadGlobalDefs) reads this split back verbatim.
	for _, ab := range pk.Abilities {
		targeting, _ := json.Marshal(ab.Targeting)
		requires, _ := json.Marshal(ab.Requires)
		costs, _ := json.Marshal(ab.Costs)
		onResolve, err := json.Marshal(ab.OnResolve)
		if err != nil {
			return fmt.Errorf("store: marshal ability %s on_resolve: %w", ab.Ref, err)
		}
		messages, _ := json.Marshal(abilityMessages{
			AbilityMessagesDTO: ab.Messages, Words: ab.Words,
			RequiresGrant: ab.RequiresGrant, Skill: ab.Skill, // Phase 11.4a/11.3 fields ride the messages JSONB (no column)
			OnEvent: ab.OnEvent, // [G3] event subscriptions ride the messages JSONB too (no column)
		})
		tags := ab.Tags
		if tags == nil {
			tags = []string{}
		}
		inv := ab.Invocation
		if inv == "" {
			inv = "command"
		}
		var lua any
		if ab.OnResolveLua != "" {
			lua = ab.OnResolveLua
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO ability_defs
			   (ref, pack, name, invocation, targeting, tags, requires, costs,
			    cast_time, lag, cooldown, on_resolve, on_resolve_lua, messages)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			ab.Ref, pk.Pack, ab.Name, inv, targeting, tags, requires, costs,
			ab.CastTime, ab.Lag, ab.Cooldown, onResolve, lua, messages); err != nil {
			return fmt.Errorf("store: insert ability %s: %w", ab.Ref, err)
		}
	}
	// Combat profiles (Phase 6.3a): the whole to-hit/avoidance/damage SHAPE is content, so it all
	// rides the JSONB body (combatProfileBody). ref+pack is the per-kind PK. The loader reads it back
	// into the same CombatProfileDTO the embedded YAML produces.
	for _, cp := range pk.CombatProfiles {
		body, err := json.Marshal(combatProfileBody{
			ToHit: cp.ToHit, Avoidance: cp.Avoidance, DamageBonus: cp.DamageBonus,
		})
		if err != nil {
			return fmt.Errorf("store: marshal combat profile %s body: %w", cp.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO combat_profile_defs (ref, pack, body) VALUES ($1,$2,$3)`,
			cp.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert combat profile %s: %w", cp.Ref, err)
		}
	}
	// Channels (Phase 8.3): the whole channel SHAPE (verb/color/format/access/...) rides the JSONB body
	// (channelBody). ref+pack is the per-kind PK. The loader reads it back into the same ChannelDTO the
	// embedded YAML produces, so YAML and Postgres packs register channels identically.
	for _, ch := range pk.Channels {
		body, err := json.Marshal(channelBody{
			Name: ch.Name, Words: ch.Words, Color: ch.Color, Format: ch.Format,
			Access: ch.Access, HearAccess: ch.HearAccess, DefaultOn: ch.DefaultOn, History: ch.History,
		})
		if err != nil {
			return fmt.Errorf("store: marshal channel %s body: %w", ch.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO channel_defs (ref, pack, body) VALUES ($1,$2,$3)`,
			ch.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert channel %s: %w", ch.Ref, err)
		}
	}
	// Regions (Phase 10.3): the region SHAPE (name + member zone refs) rides the JSONB body (regionBody).
	// ref+pack is the per-kind PK. The loader reads it back into the same RegionDTO the embedded YAML
	// produces, so YAML and Postgres packs define regions identically.
	for _, rg := range pk.Regions {
		body, err := json.Marshal(regionBody{Name: rg.Name, Zones: rg.Zones})
		if err != nil {
			return fmt.Errorf("store: marshal region %s body: %w", rg.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO region_defs (ref, pack, body) VALUES ($1,$2,$3)`,
			rg.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert region %s: %w", rg.Ref, err)
		}
	}
	// Tracks (Phase 11.2): the track SHAPE (progress/level attrs, thresholds, per-step grant op-lists)
	// rides the JSONB body (trackBody). ref+pack is the per-kind PK. The loader reads it back into the same
	// TrackDTO the embedded YAML produces, so YAML and Postgres packs define tracks identically.
	for _, tr := range pk.Tracks {
		body, err := json.Marshal(trackBody{
			ProgressAttr: tr.ProgressAttr, LevelAttr: tr.LevelAttr,
			Thresholds: tr.Thresholds, Steps: tr.Steps,
		})
		if err != nil {
			return fmt.Errorf("store: marshal track %s body: %w", tr.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO track_defs (ref, pack, body) VALUES ($1,$2,$3)`,
			tr.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert track %s: %w", tr.Ref, err)
		}
	}
	// Bundles (Phase 11.4b): the bundle SHAPE (kind + grant op-list) rides the JSONB body (bundleBody).
	// ref+pack is the per-kind PK. The loader reads it back into the same BundleDTO the embedded YAML
	// produces, so YAML and Postgres packs define bundles identically.
	for _, bn := range pk.Bundles {
		body, err := json.Marshal(bundleBody{Kind: bn.Kind, Uncapped: bn.Uncapped, Grants: bn.Grants})
		if err != nil {
			return fmt.Errorf("store: marshal bundle %s body: %w", bn.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO bundle_defs (ref, pack, body) VALUES ($1,$2,$3)`,
			bn.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert bundle %s: %w", bn.Ref, err)
		}
	}
	// Rarity tiers + loot tables (Phase 12.1): ref+pack PK, the shape in the JSONB body. Round-trip into
	// the same DTOs the embedded YAML produces.
	for _, rt := range pk.RarityTiers {
		body, err := json.Marshal(rarityTierBody{
			Order: rt.Order, Weight: rt.Weight, Color: rt.Color, Binds: rt.Binds,
			SalvageTable: rt.SalvageTable, SalvageSkill: rt.SalvageSkill, SalvageBonusStep: rt.SalvageBonusStep,
		})
		if err != nil {
			return fmt.Errorf("store: marshal rarity_tier %s body: %w", rt.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO rarity_tier_defs (ref, pack, body) VALUES ($1,$2,$3)`, rt.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert rarity_tier %s: %w", rt.Ref, err)
		}
	}
	// Named affixes (#37): ref+pack PK, the attr + roll range in the JSONB body.
	for _, af := range pk.Affixes {
		body, err := json.Marshal(affixBody{Attr: af.Attr, Min: af.Min, Max: af.Max})
		if err != nil {
			return fmt.Errorf("store: marshal affix %s body: %w", af.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO affix_defs (ref, pack, body) VALUES ($1,$2,$3)`, af.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert affix %s: %w", af.Ref, err)
		}
	}
	for _, lt := range pk.LootTables {
		body, err := json.Marshal(lootTableBody{Rolls: lt.Rolls, OnRoll: lt.OnRoll})
		if err != nil {
			return fmt.Errorf("store: marshal loot_table %s body: %w", lt.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO loot_table_defs (ref, pack, body) VALUES ($1,$2,$3)`, lt.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert loot_table %s: %w", lt.Ref, err)
		}
	}
	// Spawn schedules (Phase 12.4): ref+pack PK, the schedule shape in the JSONB body.
	for _, sc := range pk.SpawnSchedules {
		body, err := json.Marshal(spawnScheduleBody{
			Proto: sc.Proto, Zone: sc.Zone, Room: sc.Room,
			IntervalAfterDeathSec: sc.IntervalAfterDeathSec, OnMissed: sc.OnMissed, Announce: sc.Announce,
		})
		if err != nil {
			return fmt.Errorf("store: marshal spawn_schedule %s body: %w", sc.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO spawn_schedule_defs (ref, pack, body) VALUES ($1,$2,$3)`, sc.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert spawn_schedule %s: %w", sc.Ref, err)
		}
	}
	// Recipes (Phase 13.5): ref+pack PK, the recipe shape in the JSONB body.
	for _, rc := range pk.Recipes {
		body, err := json.Marshal(recipeBody{
			Name: rc.Name, Aliases: rc.Aliases,
			Profession: rc.Profession, Track: rc.Track, Skill: rc.Skill, MinSkill: rc.MinSkill, Station: rc.Station,
			Inputs: rc.Inputs, Output: rc.Output, QualityBase: rc.QualityBase,
		})
		if err != nil {
			return fmt.Errorf("store: marshal recipe %s body: %w", rc.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO recipe_defs (ref, pack, body) VALUES ($1,$2,$3)`, rc.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert recipe %s: %w", rc.Ref, err)
		}
	}
	// Wear slots (#35): ref+pack PK, the label/order/kind in the JSONB body.
	for _, ws := range pk.WearSlots {
		body, err := json.Marshal(wearSlotBody{Label: ws.Label, Order: ws.Order, Kind: ws.Kind})
		if err != nil {
			return fmt.Errorf("store: marshal wear_slot %s body: %w", ws.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO wear_slot_defs (ref, pack, body) VALUES ($1,$2,$3)`, ws.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert wear_slot %s: %w", ws.Ref, err)
		}
	}
	// Chargens (Phase 14.8): ref+pack PK, the step list in the JSONB body.
	for _, cg := range pk.Chargens {
		body, err := json.Marshal(chargenBody{Steps: cg.Steps})
		if err != nil {
			return fmt.Errorf("store: marshal chargen %s body: %w", cg.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO chargen_defs (ref, pack, body) VALUES ($1,$2,$3)`, cg.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert chargen %s: %w", cg.Ref, err)
		}
	}
	// Display templates: (pack, surface) PK, the Lua render body in the JSONB body.
	for _, dd := range pk.DisplayDefs {
		body, err := json.Marshal(displayDefBody{Render: dd.Render})
		if err != nil {
			return fmt.Errorf("store: marshal display_def %s body: %w", dd.Surface, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO display_defs (surface, pack, body) VALUES ($1,$2,$3)`, dd.Surface, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert display_def %s: %w", dd.Surface, err)
		}
	}
	// Trust tiers (#27/#29, Round 9 Slice 0): (pack, name) PK + first-class rank, the granted-flag list in
	// the JSONB body.
	for _, tt := range pk.TrustTiers {
		body, err := json.Marshal(trustTierBody{Flags: tt.Flags})
		if err != nil {
			return fmt.Errorf("store: marshal trust_tier %s body: %w", tt.Name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO trust_tier_defs (name, pack, rank, body) VALUES ($1,$2,$3,$4)`,
			tt.Name, pk.Pack, tt.Rank, body); err != nil {
			return fmt.Errorf("store: insert trust_tier %s: %w", tt.Name, err)
		}
	}
	// Pack-level scalars (Phase 6.3a): default_combat in the pack_meta row. Only written when set, so
	// a pack that names no player default leaves no row (the loader then leaves DefaultCombat empty).
	if pk.DefaultCombat != "" {
		body, err := json.Marshal(packMetaBody{DefaultCombat: pk.DefaultCombat})
		if err != nil {
			return fmt.Errorf("store: marshal pack_meta %s: %w", pk.Pack, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO pack_meta (pack, body) VALUES ($1,$2)`,
			pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert pack_meta %s: %w", pk.Pack, err)
		}
	}
	return nil
}

// nullStr maps "" to a SQL NULL so optional TEXT columns stay null rather than empty-string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullBytes maps an empty byte slice to a SQL NULL so an optional JSONB column stays null rather than
// erroring on an empty value.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
