package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/double-nibble/telosmud/internal/content"
)

// content.go is the store side of the content read path: it loads the definition rows for the
// enabled packs and assembles them into content.Pack values, implementing content.Source. The
// `body` JSONB of an item/mob prototype carries the component DTOs (physical/wearable/weapon/
// container); the `body` of a zone_reset carries the ResetDTO. Stable relational columns
// (ref/name/short/long/keywords/exits) stay first-class so FK integrity and indexed lookups
// work, with the open-ended remainder in JSONB (docs/PERSISTENCE.md §1).

// protoBody is the JSONB tail shape for an item/mob prototype row: the optional component
// templates. It mirrors the component fields of content.ProtoDTO so a round-trip through the
// DB column reproduces the same prototype the embedded YAML would.
type protoBody struct {
	Physical  *content.PhysicalDTO  `json:"physical,omitempty"`
	Wearable  *content.WearableDTO  `json:"wearable,omitempty"`
	Weapon    *content.WeaponDTO    `json:"weapon,omitempty"`
	Container *content.ContainerDTO `json:"container,omitempty"`
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

	out := make([]content.Pack, 0, len(byPack))
	for _, name := range enabled {
		if pp := byPack[name]; pp != nil {
			out = append(out, *pp)
		}
	}
	return out, nil
}

func (p *Pool) loadRooms(ctx context.Context, enabled []string, zones map[string]*content.ZoneDTO) error {
	rooms := map[string]*content.RoomDTO{}
	rows, err := p.pool.Query(ctx,
		`SELECT ref, zone_ref, name, COALESCE(sector, ''), COALESCE(body->>'long', '')
		   FROM rooms WHERE pack = ANY($1) ORDER BY zone_ref, ref`, enabled)
	if err != nil {
		return fmt.Errorf("store: query rooms: %w", err)
	}
	for rows.Next() {
		var r content.RoomDTO
		var zoneRef string
		if err := rows.Scan(&r.Ref, &zoneRef, &r.Name, &r.Sector, &r.Long); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan room: %w", err)
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
