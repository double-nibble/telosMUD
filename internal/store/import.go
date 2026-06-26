package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/double-nibble/telosmud/internal/content"
)

// import.go is the content WRITE path used by `make seed` (cmd/telos-seed): it loads a parsed
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
		body, _ := json.Marshal(map[string]string{"long": r.Long})
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

// nullStr maps "" to a SQL NULL so optional TEXT columns stay null rather than empty-string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
