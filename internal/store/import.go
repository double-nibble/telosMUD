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
		if _, err := tx.Exec(ctx,
			`INSERT INTO rooms (ref, pack, zone_ref, name, sector, body) VALUES ($1,$2,$3,$4,$5,$6)`,
			r.Ref, pack, z.Ref, r.Name, nullStr(r.Sector), body); err != nil {
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
		body, _ := json.Marshal(attrBody{Min: a.Min, Max: a.Max})
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
			OnEvent: r.OnEvent, OnDepleted: r.OnDepleted, PerRound: r.PerRound,
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
			Access: ch.Access, DefaultOn: ch.DefaultOn, History: ch.History,
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
