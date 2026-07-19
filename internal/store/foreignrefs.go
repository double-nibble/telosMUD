package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/double-nibble/telosmud/internal/content"
)

// foreignrefs.go — the seed→pull cutover guard (#366).
//
// # The failure it replaces
//
// Definition refs are GLOBAL, not per-pack: `zones.ref`, `rooms.ref`, `item_prototypes.ref` and most
// *_defs tables are `ref TEXT PRIMARY KEY`, with `pack` a plain column. Two packs shipping the same ref
// therefore collide at the database level.
//
// That is normally invisible, because ImportVersion prunes the packs a version drops before importing the
// ones it adds — but the prune diff is driven by `content_pack_registry`, and telos-seed does NOT register:
// it calls ImportPacks, which never touches content_version or the registry. So after `make seed`, the
// `demo` pack's rows exist while the registry is empty. Switching that deployment to telos-pull then hits:
//
//	import version: store: insert zone midgaard: duplicate key value violates unique constraint "zones_pkey" (SQLSTATE 23505)
//
// A raw SQLSTATE with no indication of what owns the ref, what to do about it, or that the whole import
// rolled back. It cost a hand-written SQL purge during the staging cutover.
//
// # Why this is a check and not a bigger fix
//
// Deliberately NOT "prune anything unregistered": a seed-imported pack with genuinely disjoint refs is
// harmless — the world's enabled set comes from the registry, so those rows are inert — and stripping it
// automatically would be a destructive behavior change to environments that work today. The check fires
// only on an ACTUAL ref collision, and the remedy is an explicit operator command.
//
// # Why it lives inside the import transaction
//
// It is the only place it can be authoritative. ImportVersion holds `SELECT ... FOR UPDATE` on the
// content_version singleton for the whole prune→import→bump→registry section, so a check inside that lock
// cannot race a concurrent importer. A pre-flight in contentpull.Pull would read outside the lock and race
// exactly the way the prune guard's own header already admits prunePreview does — there is no reason to add
// a second instance of a known-racy pattern. It also covers every caller for free, which matters because
// the DIRECTOR-coordinated pull hits the same collision as the CLI, and a fix in one would not cover both.
//
// It runs AFTER the prune loop: a pack legitimately being dropped by this version has already had its rows
// deleted, so it can never false-positive.

// ErrForeignPackRefs is returned when an import would overwrite refs currently owned by a pack that is not
// part of the incoming set. Callers branch on it with errors.As to render the remedy.
type ErrForeignPackRefs struct {
	// Packs owns at least one colliding ref, sorted.
	Packs []string
	// Sample is one representative collision, for the message ("zone \"midgaard\"").
	Sample string
	// Total is how many colliding refs were found across every table.
	Total int
	// Registered is the subset of Packs that are part of the INSTALLED content version. It decides which
	// remedy the message names: an unregistered owner is a purgeable leftover, a registered one is live
	// content that PurgePack will (correctly) refuse to touch.
	Registered []string
}

// Error renders the refusal AND the correct remedy, which differs by whether the colliding pack is part of
// the installed content version.
//
// This distinction is load-bearing, not cosmetic. An earlier version asserted unconditionally that the
// owner was "not in the content registry — almost certainly a telos-seed import" and told the operator to
// run `--purge-pack`. That inference holds for ImportVersion (a registered pack is either in the incoming
// set or was already pruned by the time this runs) but NOT for ImportPacks, which does neither — so
// re-running `make seed` against a pulled, REGISTERED pack produced a message that asserted something
// false, misdiagnosed the cause, and printed a command that PurgePack is guaranteed to refuse. A remedy
// that dead-ends is worse than no remedy: it costs the operator the time to try it.
func (e *ErrForeignPackRefs) Error() string {
	head := fmt.Sprintf(
		"refusing import: %d ref(s) this import would insert are already owned by pack(s) [%s], which are not "+
			"in the incoming set (e.g. %s). Definition refs are global, so importing would collide. ",
		e.Total, strings.Join(e.Packs, ", "), e.Sample)
	if len(e.Registered) > 0 {
		return head + fmt.Sprintf(
			"Pack(s) [%s] are part of the INSTALLED content version, so this is not a stale leftover and must "+
				"not be purged — that would leave the registry describing content that is gone. Either drop the "+
				"colliding refs from the incoming content, or publish a content version whose manifest omits "+
				"the conflicting pack (that path prunes it under the live-hosted-pack guard).",
			strings.Join(e.Registered, ", "))
	}
	return head + fmt.Sprintf(
		"They are not in the content registry either — almost certainly a telos-seed import predating the "+
			"content store. Remove the stale pack first:  telos-pull --purge-pack %s", e.Packs[0])
}

// refOwnerTable is one table whose PK is a bare global `ref`, so two packs can collide on it. Tables whose
// PK already includes `pack` (affix_defs, command_defs, display_defs, formula_defs, trust_tier_defs,
// wear_slot_defs, pack_meta) cannot collide and are deliberately absent.
//
// SOURCE OF TRUTH: this must mirror the global-ref-PK tables importPackTx writes. TestForeignRefTablesMatch
// TheSchema asserts it against the live database's actual primary keys, so a new definition table cannot be
// added without either appearing here or being proven collision-proof.
type refOwnerTable struct {
	table string
	kind  string // the label used in the operator-facing message
}

var refOwnerTables = []refOwnerTable{
	{"zones", "zone"},
	{"rooms", "room"},
	{"item_prototypes", "item"},
	{"mob_prototypes", "mob"},
	{"attribute_defs", "attribute"},
	{"resource_defs", "resource"},
	{"damage_type_defs", "damage type"},
	{"affect_defs", "affect"},
	{"ability_defs", "ability"},
	{"combat_profile_defs", "combat profile"},
	{"channel_defs", "channel"},
	{"toggle_defs", "toggle"},
	{"region_defs", "region"},
	{"track_defs", "track"},
	{"bundle_defs", "bundle"},
	{"rarity_tier_defs", "rarity tier"},
	{"loot_table_defs", "loot table"},
	{"spawn_schedule_defs", "spawn schedule"},
	{"recipe_defs", "recipe"},
	{"help_defs", "help topic"},
	{"chargen_defs", "chargen"},
}

// incomingRefs collects the refs an import would write per table, from the packs themselves — so the check
// asks the precise question ("would THIS import collide") rather than the loose one ("does any unregistered
// pack exist"), which would over-fire on a harmless disjoint seed pack.
func incomingRefs(packs []content.Pack) map[string][]string {
	out := map[string][]string{}
	add := func(table, ref string) {
		if strings.TrimSpace(ref) != "" {
			out[table] = append(out[table], ref)
		}
	}
	for i := range packs {
		pk := &packs[i]
		for _, z := range pk.Zones {
			add("zones", z.Ref)
			for _, r := range z.Rooms {
				add("rooms", r.Ref)
			}
			for _, it := range z.Items {
				add("item_prototypes", it.Ref)
			}
			for _, mb := range z.Mobs {
				add("mob_prototypes", mb.Ref)
			}
		}
		for _, d := range pk.Attributes {
			add("attribute_defs", d.Ref)
		}
		for _, d := range pk.Resources {
			add("resource_defs", d.Ref)
		}
		for _, d := range pk.DamageTypes {
			add("damage_type_defs", d.Ref)
		}
		for _, d := range pk.Affects {
			add("affect_defs", d.Ref)
		}
		for _, d := range pk.Abilities {
			add("ability_defs", d.Ref)
		}
		for _, d := range pk.CombatProfiles {
			add("combat_profile_defs", d.Ref)
		}
		for _, d := range pk.Channels {
			add("channel_defs", d.Ref)
		}
		for _, d := range pk.ToggleDefs {
			add("toggle_defs", d.Ref)
		}
		for _, d := range pk.Regions {
			add("region_defs", d.Ref)
		}
		for _, d := range pk.Tracks {
			add("track_defs", d.Ref)
		}
		for _, d := range pk.Bundles {
			add("bundle_defs", d.Ref)
		}
		for _, d := range pk.RarityTiers {
			add("rarity_tier_defs", d.Ref)
		}
		for _, d := range pk.LootTables {
			add("loot_table_defs", d.Ref)
		}
		for _, d := range pk.SpawnSchedules {
			add("spawn_schedule_defs", d.Ref)
		}
		for _, d := range pk.Recipes {
			add("recipe_defs", d.Ref)
		}
		for _, d := range pk.HelpDefs {
			add("help_defs", d.Ref)
		}
		for _, d := range pk.Chargens {
			add("chargen_defs", d.Ref)
		}
	}
	return out
}

// assertNoForeignRefsTx returns an *ErrForeignPackRefs when any ref the incoming packs would insert is
// currently owned by a pack outside `allowed`. One indexed PK probe per table, on a path that already takes
// seconds — negligible.
//
// `allowed` is the set of packs this import legitimately owns: the incoming manifest for ImportVersion, the
// batch's own names for ImportPacks. Passing the batch names for the seed path closes the REVERSE direction
// for free (pull `reference`, then re-run `make seed`, and `demo` would collide the other way).
func assertNoForeignRefsTx(ctx context.Context, tx pgx.Tx, packs []content.Pack, allowed []string) error {
	// A NIL slice marshals to SQL NULL, and `pack <> ALL(NULL)` evaluates to NULL — which matches NOTHING
	// and would silently disable this check entirely rather than erroring. An EMPTY non-nil slice is fine
	// (`x <> ALL('{}')` is true). Both current callers pass a non-nil slice, so this is a guard against a
	// future one, and the failure it guards against is total silent disablement.
	if allowed == nil {
		allowed = []string{}
	}
	refs := incomingRefs(packs)
	owners := map[string]bool{}
	var sample string
	total := 0
	for _, t := range refOwnerTables {
		list := refs[t.table]
		if len(list) == 0 {
			continue
		}
		//nolint:gosec // G202: t.table is from the fixed refOwnerTables literal above, never user input.
		q := `SELECT pack, ref FROM ` + t.table + ` WHERE ref = ANY($1) AND pack <> ALL($2)`
		rows, err := tx.Query(ctx, q, list, allowed)
		if err != nil {
			return fmt.Errorf("store: check foreign refs in %s: %w", t.table, err)
		}
		for rows.Next() {
			var pack, ref string
			if err := rows.Scan(&pack, &ref); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan foreign ref in %s: %w", t.table, err)
			}
			owners[pack] = true
			total++
			if sample == "" {
				sample = fmt.Sprintf("%s %q (owned by pack %q)", t.kind, ref, pack)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: check foreign refs in %s: %w", t.table, err)
		}
	}
	if total == 0 {
		return nil
	}
	packNames := make([]string, 0, len(owners))
	for p := range owners {
		packNames = append(packNames, p)
	}
	sort.Strings(packNames)
	registered, err := registeredSubsetTx(ctx, tx, packNames)
	if err != nil {
		return err
	}
	return &ErrForeignPackRefs{Packs: packNames, Sample: sample, Total: total, Registered: registered}
}

// registeredSubsetTx returns which of packs are in content_pack_registry, i.e. part of the installed
// content version. It is read only to choose the right REMEDY in the refusal message — the collision
// decision itself never consults the registry, deliberately: the question is "would this import collide",
// not "is the owner registered".
func registeredSubsetTx(ctx context.Context, tx pgx.Tx, packs []string) ([]string, error) {
	if len(packs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT pack FROM content_pack_registry WHERE pack = ANY($1)`, packs)
	if err != nil {
		return nil, fmt.Errorf("store: read content pack registry: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: scan content pack registry: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read content pack registry: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

// PurgePack removes every row belonging to pack, in its own transaction. It is the operator remedy the
// ErrForeignPackRefs message names — the seed→pull cutover step that previously required hand-written SQL.
//
// It REFUSES a pack listed in content_pack_registry. Purging a registered pack would desync the registry
// from the rows and turn a recovery tool into a footgun; the supported way to drop a registered pack is to
// publish a manifest that omits it, which goes through ImportVersion's prune — and therefore through the
// live-hosted-pack prune guard. This tool deliberately has no equivalent of that guard, which is exactly
// why it must not be usable on content the fleet is serving.
func (p *Pool) PurgePack(ctx context.Context, pack string) error {
	if strings.TrimSpace(pack) == "" {
		return errors.New("store: purge pack: no pack name given")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: purge pack: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Take the SAME singleton lock ImportVersion holds across its prune→import→registry section. Without
	// it the registry check below and the delete are not atomic against a concurrent import: under READ
	// COMMITTED each statement gets a fresh snapshot, so a purge can read "not registered", an import can
	// then commit a version that INSTALLS AND REGISTERS that pack, and the purge's delete then removes the
	// rows the import just wrote. Both report success and the result is precisely the torn state the
	// registry check below exists to refuse — the registry naming a pack whose every row is gone. This is
	// reachable in the exact situation the tool is for: a seed→pull cutover is when an operator runs a
	// purge, and the director-coordinated pull can fire concurrently with the CLI. One row lock, on a path
	// that runs about once per environment.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM content_version WHERE id = 1 FOR UPDATE`); err != nil {
		return fmt.Errorf("store: purge pack: lock content_version: %w", err)
	}

	var registered bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM content_pack_registry WHERE pack = $1)`, pack).Scan(&registered); err != nil {
		return fmt.Errorf("store: purge pack: read registry: %w", err)
	}
	if registered {
		return fmt.Errorf("refusing to purge pack %q: it is in the content registry, i.e. part of the "+
			"currently installed content version. Purging it would leave the registry describing content that "+
			"is no longer there. To remove a registered pack, publish a content version whose manifest omits "+
			"it — that path prunes it under the live-hosted-pack guard, which this command has no equivalent of", pack)
	}
	if err := deletePack(ctx, tx, pack); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: purge pack: commit: %w", err)
	}
	return nil
}
