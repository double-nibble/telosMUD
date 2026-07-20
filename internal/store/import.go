package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/double-nibble/telosmud/internal/content"
)

// VersionMeta is the published-version identity ImportVersion stamps into content_version (#212
// slice 4). ContentSHA is the immutable git commit (the true identity); ManifestVersion is the human
// tag; ContentHash corroborates the tree.
type VersionMeta struct {
	ContentSHA      string
	ManifestVersion string
	ContentHash     string
}

// ContentVersionInfo is the content version a database currently serves (the content_version
// singleton plus the live pack registry set).
type ContentVersionInfo struct {
	Version         uint64   // the monotonic logical content version (0 => never imported)
	ContentSHA      string   // immutable git SHA of the served tree
	ManifestVersion string   // human manifest tag
	ContentHash     string   // corroborating content hash
	Packs           []string // the live registry pack set (sorted)
}

// ImportPack is the content WRITE path used by `make seed` (cmd/telos-seed): it loads a parsed
// content.Pack (from the same embedded YAML the tests use) into the definition rows. It is the
// "import (file->rows)" half of decision D4; export (rows->file) is a later concern. Seeding
// is idempotent per pack: it DELETEs the pack's existing rows then re-inserts, so re-running
// `make seed` is safe and a pack edit fully replaces the old content. The whole import runs in
// one transaction (PERSISTENCE.md §1: strip/replace is one transaction).
func (p *Pool) ImportPack(ctx context.Context, pk content.Pack) error {
	return p.ImportPacks(ctx, []content.Pack{pk})
}

// ImportPacks imports EVERY pack in a single transaction (#212 slice 3), so a multi-pack version
// import (cmd/telos-pull) is all-or-nothing: a failure on any pack rolls back the WHOLE batch,
// never leaving Postgres at a torn half-version that a concurrent content read could serve. Each
// pack strips-and-replaces its OWN rows (deletePack WHERE pack=$1) and is idempotent.
//
// CONSTRAINTS the caller must honor — most zone/room/prototype/def tables key rows by `ref` ALONE
// (globally unique), NOT by (ref, pack):
//   - Pack refs must be globally DISJOINT across the batch. A ref shared by two packs collides on the
//     shared-ref PK and fails the whole import (fail-safe: atomic, never a torn state). Pack-namespaced
//     refs give this naturally.
//   - Pack names must be UNIQUE in the batch (enforced below): two same-named entries would otherwise
//     have the second's deletePack drop the first's just-inserted rows silently.
//   - Exits are phased per-pack, so a cross-PACK exit (pack A room -> pack B room) is not supported;
//     packs are self-contained worlds.
func (p *Pool) ImportPacks(ctx context.Context, packs []content.Pack) error {
	seen := make(map[string]bool, len(packs))
	for _, pk := range packs {
		if pk.Pack == "" {
			return fmt.Errorf("store: import pack with empty name")
		}
		if seen[pk.Pack] {
			return fmt.Errorf("store: duplicate pack %q in import batch", pk.Pack)
		}
		seen[pk.Pack] = true
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin import tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// Seed→pull cutover guard (#366), the REVERSE direction: pull `reference`, then re-run `make seed`, and
	// `demo` collides the other way. `allowed` is this batch's own names — a re-seed of the same pack
	// strip-replaces as it always has, but colliding with a pack somebody else owns is refused with the
	// remedy rather than a raw duplicate-key SQLSTATE. See foreignrefs.go.
	if err := assertNoForeignRefsTx(ctx, tx, packs, packNames(packs)); err != nil {
		return err
	}
	// LIMIT, so the guard is not over-trusted: two packs IN THE SAME BATCH sharing a ref are both in
	// `allowed`, so the check passes them and the insert still dies on the shared-ref PK with a raw 23505.
	// That is a content-authoring error rather than the cross-import cutover collision #366 is about, and
	// the doc comment above already names it as a supported failure — but it is the same failure CLASS, so
	// do not read a clean run here as "no ref can collide".
	for _, pk := range packs {
		if err := importPackTx(ctx, tx, pk); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit import: %w", err)
	}
	return nil
}

// packNames is the pack-name set of a batch, i.e. the packs an import legitimately owns and may overwrite.
func packNames(packs []content.Pack) []string {
	out := make([]string, 0, len(packs))
	for i := range packs {
		out = append(out, packs[i].Pack)
	}
	return out
}

// ImportVersion imports a PUBLISHED content version atomically (#212 slice 4): in ONE transaction it
// prunes packs the new version drops (present in the registry, absent from packs), strip-replaces each
// named pack, overwrites the pack registry to exactly this version's packs, and bumps the monotonic
// content_version singleton — returning the minted version and the pruned pack names.
//
// The version is minted as GREATEST(version+1, now_nanos): nanos-scale (never collides with a
// wall-clock fallback and never wedges the reconcile guard) and monotonic. The critical section is
// serialized by taking a FOR UPDATE lock on the singleton FIRST, so two concurrent importers (telos-
// pull + the director) can never interleave their prune/import/registry writes — the whole section is
// atomic, not just the scalar bump. Because everything is one tx, a crash never leaves rows without a
// consistent version marker.
//
// Re-importing byte-identical content (same ContentSHA) is IDEMPOTENT: it returns the current version
// without bumping or rewriting, so a leader-failover redelivery of the same published version does not
// inflate the version or force a spurious fleet-wide reconcile (the key slice-4 invariant).
//
// pruned is returned (sorted) so the CALLER can enforce the "don't hot-strip a live-hosted pack"
// policy (a rolling-reboot concern): this method performs the DB prune; it cannot know the fleet's
// hosted zones, so the director gates that decision (slice 4 PR E).
//
// changed reports whether this call actually re-imported (true) or short-circuited on an unchanged SHA
// (false). A caller SHOULD skip the hot-reload broadcast when !changed: re-broadcasting an already-
// applied version is a fleet-wide prototype + Lua re-swap for no benefit (the zone-shape guard drops the
// reconcile, but the per-ref path is NOT version-guarded) — a needless signal storm on a redelivery.
func (p *Pool) ImportVersion(ctx context.Context, packs []content.Pack, meta VersionMeta) (version uint64, pruned []string, changed bool, err error) {
	seen := make(map[string]bool, len(packs))
	newSet := make(map[string]bool, len(packs))
	for _, pk := range packs {
		if pk.Pack == "" {
			return 0, nil, false, fmt.Errorf("store: import pack with empty name")
		}
		if seen[pk.Pack] {
			return 0, nil, false, fmt.Errorf("store: duplicate pack %q in import batch", pk.Pack)
		}
		seen[pk.Pack] = true
		newSet[pk.Pack] = true
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, nil, false, fmt.Errorf("store: begin import tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// Lock the singleton FIRST — this serializes the ENTIRE prune→import→bump→registry section against
	// a concurrent importer (not just the scalar bump), and reads the current identity for the
	// idempotency short-circuit. Self-heal a missing seed row (a hand-mangled DB).
	var curVersion int64
	var curSHA string
	switch err := tx.QueryRow(ctx,
		`SELECT version, content_sha FROM content_version WHERE id = 1 FOR UPDATE`).Scan(&curVersion, &curSHA); {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `INSERT INTO content_version (id) VALUES (1)`); err != nil {
			return 0, nil, false, fmt.Errorf("store: seed content_version: %w", err)
		}
		curVersion, curSHA = 0, ""
	case err != nil:
		return 0, nil, false, fmt.Errorf("store: lock content_version: %w", err)
	}

	// Idempotency: byte-identical content (same non-empty SHA) is already fully imported (imports are
	// atomic), so re-importing is a no-op — commit to release the lock and return the current version.
	if meta.ContentSHA != "" && meta.ContentSHA == curSHA {
		if err := tx.Commit(ctx); err != nil {
			return 0, nil, false, fmt.Errorf("store: commit (idempotent re-import): %w", err)
		}
		return uint64(curVersion), nil, false, nil //nolint:gosec // G115: version >= 0, from a bounded nanos column
	}

	// Prune packs the prior version had that this one drops (the registry is the diff source). NOTE:
	// deletePack strips a pack's rooms, but a SURVIVING pack's exit whose to_room points INTO a pruned
	// pack's room would FK-fail the rooms DELETE — which rolls the WHOLE import back (fail-safe, never
	// torn), since cross-pack exits are unsupported (packs are self-contained worlds, see importPackTx).
	oldSet, err := registryPacksTx(ctx, tx)
	if err != nil {
		return 0, nil, false, err
	}
	for _, pack := range oldSet {
		if !newSet[pack] {
			if derr := deletePack(ctx, tx, pack); derr != nil {
				return 0, nil, false, derr
			}
			pruned = append(pruned, pack)
		}
	}
	sort.Strings(pruned)

	// Seed→pull cutover guard (#366). AFTER the prune (so a pack this version legitimately drops has
	// already gone and cannot false-positive) and BEFORE the inserts (so the operator gets an actionable
	// refusal instead of a raw duplicate-key SQLSTATE). Inside the singleton lock held above, so it cannot
	// race a concurrent importer. See foreignrefs.go.
	if err := assertNoForeignRefsTx(ctx, tx, packs, packNames(packs)); err != nil {
		return 0, nil, false, err
	}

	// Strip-replace each named pack (same per-pack tx body as ImportPacks).
	for _, pk := range packs {
		if err := importPackTx(ctx, tx, pk); err != nil {
			return 0, nil, false, err
		}
	}

	// Bump the monotonic version + stamp the version identity (under the lock held above).
	now := time.Now().UnixNano()
	var ver int64
	if err := tx.QueryRow(ctx,
		`UPDATE content_version
		    SET version = GREATEST(version + 1, $1::bigint),
		        content_sha = $2, manifest_version = $3, content_hash = $4, imported_at = now()
		  WHERE id = 1
		RETURNING version`,
		now, meta.ContentSHA, meta.ManifestVersion, meta.ContentHash).Scan(&ver); err != nil {
		return 0, nil, false, fmt.Errorf("store: bump content_version: %w", err)
	}

	// Overwrite the registry to exactly this version's packs.
	if _, err := tx.Exec(ctx, `DELETE FROM content_pack_registry`); err != nil {
		return 0, nil, false, fmt.Errorf("store: clear pack registry: %w", err)
	}
	for _, pk := range packs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO content_pack_registry (pack, version) VALUES ($1, $2)`, pk.Pack, ver); err != nil {
			return 0, nil, false, fmt.Errorf("store: register pack %s: %w", pk.Pack, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, nil, false, fmt.Errorf("store: commit import version: %w", err)
	}
	// ver is GREATEST(version+1, now_nanos) >= 1, so the widening is always safe.
	return uint64(ver), pruned, true, nil //nolint:gosec // G115: ver >= 1 (a positive nanos-scale value), never negative
}

// ErrNoContentVersion is returned by CurrentContentVersion when the database has no content_version row — a
// FRESH DB that was never pulled/imported. It is a distinct, EXPECTED bootstrap state, not a read failure, so
// a caller that must fail closed on a genuine registry-read error (telos-account, #246) can still bootstrap on
// a fresh DB by treating this sentinel as "use the demo/override default".
var ErrNoContentVersion = errors.New("store: no content version registered (fresh database)")

// CurrentContentVersion reads the content version this database currently serves (the singleton stamp
// + the live pack registry set) as ONE consistent snapshot — a single query joining the singleton to
// the registry, so an import committing mid-read can't return a version stamp that disagrees with the
// pack set. A fresh database (never imported) returns ErrNoContentVersion (NOT a generic error), so a
// caller can distinguish "fresh DB, demo is correct" from a real read failure.
func (p *Pool) CurrentContentVersion(ctx context.Context) (ContentVersionInfo, error) {
	info := ContentVersionInfo{Packs: []string{}}
	var ver int64
	if err := p.pool.QueryRow(ctx,
		`SELECT cv.version, cv.content_sha, cv.manifest_version, cv.content_hash,
		        COALESCE(array_agg(r.pack ORDER BY r.pack) FILTER (WHERE r.pack IS NOT NULL), '{}')
		   FROM content_version cv
		   LEFT JOIN content_pack_registry r ON TRUE
		  WHERE cv.id = 1
		  GROUP BY cv.version, cv.content_sha, cv.manifest_version, cv.content_hash`).
		Scan(&ver, &info.ContentSHA, &info.ManifestVersion, &info.ContentHash, &info.Packs); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No content_version row: a FRESH DB that was never pulled. This is a normal bootstrap state, NOT a
			// read failure — the caller uses the demo/override default and should trust it. Distinguished from a
			// real read error (below) via the ErrNoContentVersion sentinel so a caller (telos-account, #246) can
			// fail closed on a genuine error while still bootstrapping on a fresh DB.
			return ContentVersionInfo{Packs: []string{}}, ErrNoContentVersion
		}
		return ContentVersionInfo{}, fmt.Errorf("store: read content_version: %w", err)
	}
	info.Version = uint64(ver) //nolint:gosec // G115: version >= 0 from a bounded nanos column
	if info.Packs == nil {
		info.Packs = []string{}
	}
	return info, nil
}

// ContentVersion reads just the current monotonic content version (the content_version singleton),
// for the reconcile-on-join check (#212 slice 4 PR D): a shard compares it against the version it
// last applied to detect that it missed a pull during a bus gap. A fresh DB returns 0.
func (p *Pool) ContentVersion(ctx context.Context) (uint64, error) {
	var ver int64
	if err := p.pool.QueryRow(ctx, `SELECT version FROM content_version WHERE id = 1`).Scan(&ver); err != nil {
		return 0, fmt.Errorf("store: read content version: %w", err)
	}
	return uint64(ver), nil //nolint:gosec // G115: version >= 0 from a bounded nanos column
}

// BumpContentVersion ATOMICALLY increments and returns the monotonic content version — the durable mint a
// shard-local `reload` uses instead of stamping wall-clock nanos (#232). A single atomic `version = version
// + 1` on the singleton is monotonic FLEET-WIDE with no per-shard clock, so it closes the two wall-clock
// residuals #222 left: (1) a clock-AHEAD shard can no longer stamp a far-future version that silently drops
// a LATER director pull's zone-shape reconcile (both now advance the same PG counter); (2) the bump is
// visible to reconcile-on-join, so a shard that MISSED the reload's bus message sees the advanced version
// on (re)join and re-materializes, rather than reading an unchanged version and concluding it is up to date.
//
// It bumps ONLY version — content_sha / manifest_version / the pack registry are untouched, because a
// reload re-materializes the SAME already-imported rows (no content change); the version is the logical
// "served epoch" marker (#209), not the published-content identity. The row-level UPDATE lock serializes it
// against a concurrent BumpContentVersion and against ImportVersion's `SELECT ... FOR UPDATE`, so a reload
// racing a pull converges monotonically (neither loses an increment). Self-heals a missing singleton row.
//
// Because a reload does NOT emit the version-complete sentinel, it does not advance any shard's
// appliedContentVersion — so a reload leaves even the ISSUING shard's applied version one behind the bumped
// content_version, triggering at most ONE idempotent, level-triggered reconcile-on-join on each shard's
// next bus reconnect. That redundant-but-safe re-apply is exactly what keeps residual #2 closed (a missed
// reload is indistinguishable from an applied one at rejoin, so re-materializing is the safe choice).
func (p *Pool) BumpContentVersion(ctx context.Context) (uint64, error) {
	var ver int64
	if err := p.pool.QueryRow(ctx,
		`INSERT INTO content_version (id, version) VALUES (1, 1)
		 ON CONFLICT (id) DO UPDATE SET version = content_version.version + 1
		 RETURNING version`).Scan(&ver); err != nil {
		return 0, fmt.Errorf("store: bump content version: %w", err)
	}
	return uint64(ver), nil //nolint:gosec // G115: version >= 1 after a bump, never negative
}

// PackZones returns the zone refs a pack owns (the zones rows WHERE pack=$1), sorted. The director's
// live-hosted-pack prune guard (#212 slice 4 PR E2) maps a would-be-pruned pack to the zones whose live
// hosting it must check before allowing the strip — so a pull never hot-removes content players are
// currently standing in. A pack with no zones (a shared-defs-only pack) returns an empty list.
func (p *Pool) PackZones(ctx context.Context, pack string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT ref FROM zones WHERE pack = $1 ORDER BY ref`, pack)
	if err != nil {
		return nil, fmt.Errorf("store: read pack zones: %w", err)
	}
	defer rows.Close()
	var zones []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("store: scan pack zone: %w", err)
		}
		zones = append(zones, ref)
	}
	return zones, rows.Err()
}

// registryPacksTx reads the registry pack set within a transaction (the pre-prune old set).
func registryPacksTx(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT pack FROM content_pack_registry ORDER BY pack`)
	if err != nil {
		return nil, fmt.Errorf("store: read pack registry: %w", err)
	}
	defer rows.Close()
	var packs []string
	for rows.Next() {
		var pack string
		if err := rows.Scan(&pack); err != nil {
			return nil, fmt.Errorf("store: scan pack registry: %w", err)
		}
		packs = append(packs, pack)
	}
	return packs, rows.Err()
}

// importPackTx strips + re-inserts one pack's rows within an existing transaction. It carries no
// Begin/Commit so ImportPacks can run many packs atomically in one tx.
func importPackTx(ctx context.Context, tx pgx.Tx, pk content.Pack) error {
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
	return insertGlobalDefs(ctx, tx, pk)
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
		// Help topics (#64): same strips-and-replaces idempotency. No FK into the zone tree.
		`DELETE FROM help_defs WHERE pack=$1`,
		// Player toggles (#358): same strips-and-replaces idempotency. No FK into the zone tree.
		`DELETE FROM toggle_defs WHERE pack=$1`,
		// Display templates: same strips-and-replaces idempotency.
		`DELETE FROM display_defs WHERE pack=$1`,
		// Trust tiers (#27/#29): same strips-and-replaces idempotency. No FK into the zone tree.
		`DELETE FROM trust_tier_defs WHERE pack=$1`,
		// Custom Lua verbs + ruleset formulas (#20): same strips-and-replaces idempotency. No FK into the zone tree.
		`DELETE FROM command_defs WHERE pack=$1`,
		`DELETE FROM formula_defs WHERE pack=$1`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s, pack); err != nil {
			return fmt.Errorf("store: delete pack rows: %w", err)
		}
	}
	return nil
}

// insertZone writes one zone row. The stable columns (ref/name/start_room/reset_secs) are first-class; the
// open-ended remainder rides the `body` JSONB, exactly as the prototype and room rows do.
//
// `instanceable` (#72) is the FIRST occupant of that zone body, and it has to be carried here or the DB
// round-trip silently drops it: the flag would parse from YAML, survive an in-memory load, and then come back
// FALSE through Postgres — turning the instance opt-in off for the whole fleet the moment content is served
// from the store instead of the embed. That is the field-drop trap this repo has shipped repeatedly (Round 35's
// `primary` on resourceBody, and Track 11 before it). The direction of the drop here is fail-CLOSED (a zone
// that opted in stops being mintable, rather than one that did not opting in), so it degrades to "the dungeon
// door stops working" rather than to a security hole — but it is still a silent, environment-dependent
// divergence between the embedded and stored content, which is exactly what the round-trip test exists to catch.
func insertZone(ctx context.Context, tx pgx.Tx, pack string, z content.ZoneDTO) error {
	body, err := json.Marshal(zoneBody{Instanceable: z.Instanceable})
	if err != nil {
		return fmt.Errorf("store: marshal zone body %s: %w", z.Ref, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO zones (ref, pack, name, start_room, reset_secs, body) VALUES ($1,$2,$3,$4,$5,$6)`,
		z.Ref, pack, z.Name, nullStr(z.StartRoom), z.ResetSecs, body); err != nil {
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
		// #435 instance entrances ride the body JSONB, NOT the exits table: an exit row's to_room carries a
		// foreign key into rooms(ref), and an entrance names a ZONE, so it could never satisfy it. Anything
		// added here must also be read back in BOTH loadRoomDefinition and loadRooms — a field written and
		// not read survives the YAML tree loader and dies only on the store round trip, which is why every
		// world test would still pass. RoomDTO.Lua is dropped by this path today for exactly that reason.
		if len(r.InstanceEntrances) > 0 {
			rb["instance_entrances"] = r.InstanceEntrances
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
			OnDepleted: r.OnDepleted, PerRound: r.PerRound, Gauge: r.Gauge, Primary: r.Primary,
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
	// rides the JSONB body (combatProfileBody). ref is the PK (GLOBAL across packs — see foreignrefs.go). The loader reads it back
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
	// (channelBody). ref is the PK (GLOBAL across packs — see foreignrefs.go). The loader reads it back into the same ChannelDTO the
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
	// ref is the PK (GLOBAL across packs — see foreignrefs.go). The loader reads it back into the same RegionDTO the embedded YAML
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
	// rides the JSONB body (trackBody). ref is the PK (GLOBAL across packs — see foreignrefs.go). The loader reads it back into the same
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
	// ref is the PK (GLOBAL across packs — see foreignrefs.go). The loader reads it back into the same BundleDTO the embedded YAML
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
	// Rarity tiers + loot tables (Phase 12.1): bare `ref` PK, GLOBAL across packs (see foreignrefs.go), the shape in the JSONB body. Round-trip into
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
	// Named affixes (#37): (pack, ref) PK, so it cannot collide across packs, the attr + roll range in the JSONB body.
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
	// Spawn schedules (Phase 12.4): bare `ref` PK, GLOBAL across packs (see foreignrefs.go), the schedule shape in the JSONB body.
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
	// Recipes (Phase 13.5): bare `ref` PK, GLOBAL across packs (see foreignrefs.go), the recipe shape in the JSONB body.
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
	// Wear slots (#35): (pack, ref) PK, so it cannot collide across packs, the label/order/kind in the JSONB body.
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
	// Chargens (Phase 14.8): bare `ref` PK, GLOBAL across packs (see foreignrefs.go), the step list in the JSONB body.
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
	// Help topics (#64): bare `ref` PK, GLOBAL across packs (see foreignrefs.go), the title/category/keywords/body/see_also in the JSONB body.
	for _, hd := range pk.HelpDefs {
		body, err := json.Marshal(helpBody{
			Title: hd.Title, Category: hd.Category, Keywords: hd.Keywords,
			Body: hd.Body, SeeAlso: hd.SeeAlso, MinRank: hd.MinRank,
		})
		if err != nil {
			return fmt.Errorf("store: marshal help_def %s body: %w", hd.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO help_defs (ref, pack, body) VALUES ($1,$2,$3)`, hd.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert help_def %s: %w", hd.Ref, err)
		}
	}
	// Player toggles (#358): bare `ref` PK, GLOBAL across packs (see foreignrefs.go), the name/words/default_on/desc in the JSONB body.
	for _, tg := range pk.ToggleDefs {
		body, err := json.Marshal(toggleBody{
			Name: tg.Name, Words: tg.Words, DefaultOn: tg.DefaultOn, Desc: tg.Desc,
		})
		if err != nil {
			return fmt.Errorf("store: marshal toggle_def %s body: %w", tg.Ref, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO toggle_defs (ref, pack, body) VALUES ($1,$2,$3)`, tg.Ref, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert toggle_def %s: %w", tg.Ref, err)
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
	// Custom Lua verbs (#20, Phase 7.4e): (pack, verb) PK, the alias list + Lua handler in the JSONB body.
	for _, cmd := range pk.Commands {
		body, err := json.Marshal(commandBody{Aliases: cmd.Aliases, Lua: cmd.Lua})
		if err != nil {
			return fmt.Errorf("store: marshal command %s body: %w", cmd.Verb, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO command_defs (verb, pack, body) VALUES ($1,$2,$3)`, cmd.Verb, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert command %s: %w", cmd.Verb, err)
		}
	}
	// Ruleset-formula overrides (#20, Phase 7.4f): (pack, name) PK, the Lua formula body in the JSONB body.
	for name, lua := range pk.Formulas {
		body, err := json.Marshal(formulaBody{Lua: lua})
		if err != nil {
			return fmt.Errorf("store: marshal formula %s body: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO formula_defs (name, pack, body) VALUES ($1,$2,$3)`, name, pk.Pack, body); err != nil {
			return fmt.Errorf("store: insert formula %s: %w", name, err)
		}
	}
	// Pack-level scalars (Phase 6.3a: default_combat; #20/Phase 7.4f: pvp_lua; #47: world_script) in the
	// pack_meta row. Only written when a scalar is set, so a pack that names none leaves no row (the loader
	// then leaves them empty).
	if pk.DefaultCombat != "" || pk.PvpLua != "" || pk.WorldScript != "" {
		body, err := json.Marshal(packMetaBody{DefaultCombat: pk.DefaultCombat, PvpLua: pk.PvpLua, WorldScript: pk.WorldScript})
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
