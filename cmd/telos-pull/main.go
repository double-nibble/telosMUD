// Command telos-pull imports a PUBLISHED content version from the external content store into
// Postgres and broadcasts hot-reload invalidations (#212 slice 3). It is the versioned successor to
// telos-seed's embedded-demo path: it resolves a git tag/SHA (content.url / content.version), reads
// the version's manifest, verifies the tree against the manifest's content hash, and imports exactly
// the manifest's packs atomically. `--check` runs the whole pre-flight without writing anything — the
// gate the content repo's CI runs on a merge before tagging a version.
//
// The world never runs this; it keeps reading content from Postgres (Model 1). telos-pull is a
// CI/ops step that refreshes Postgres from a pinned version.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/contentstore"
	"github.com/double-nibble/telosmud/internal/store"
)

func main() {
	check := flag.Bool("check", false, "validate the published version without importing or broadcasting (CI merge gate)")
	flag.BoolVar(check, "n", false, "shorthand for --check")
	emit := flag.Bool("emit-manifest", false, "compute content_hash + packs over a local content tree and write manifest.yaml (content-repo publish tooling)")
	dir := flag.String("dir", ".", "the content-repo directory (for --emit-manifest)")
	manifestVersion := flag.String("manifest-version", "", "the version/tag to stamp into the emitted manifest (for --emit-manifest)")
	ciRun := flag.String("ci-run", "", "the CI run URL to record in the emitted manifest (for --emit-manifest)")
	flag.Parse()

	// --emit-manifest operates on a LOCAL content tree (no config, no Postgres, no git) — the content
	// repo's publish CI runs it to stamp content_hash + packs on a version before tagging.
	if *emit {
		if *manifestVersion == "" {
			slog.Error("--emit-manifest requires --manifest-version")
			os.Exit(1)
		}
		m, err := contentstore.EmitManifest(*dir, *manifestVersion, *ciRun)
		if err != nil {
			slog.Error("emit manifest failed", "err", err)
			os.Exit(1)
		}
		slog.Info("wrote manifest", "dir", *dir, "version", m.Version, "content_hash", m.ContentHash, "packs", m.Packs)
		return
	}

	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	if err := run(context.Background(), cfg, *check); err != nil {
		slog.Error("telos-pull failed", "err", err)
		os.Exit(1)
	}
}

// run executes the pull pipeline: resolve → manifest → verify hash → packs==dirs → load → lint, then
// (unless check) import atomically + broadcast. It returns an error instead of exiting so it is
// testable. --check stops after validation and touches neither Postgres nor NATS.
func run(ctx context.Context, cfg config.Config, check bool) error {
	if cfg.Content.URL == "" {
		return fmt.Errorf("no content store configured (set content.url / TELOS_CONTENT_URL)")
	}
	if cfg.Content.Version == "" {
		return fmt.Errorf("no content version pinned (set content.version / TELOS_CONTENT_VERSION)")
	}

	// 1. Resolve the published version (git tag/SHA) to a checked-out tree + immutable SHA.
	src := contentstore.NewGit(cfg.Content.URL, cfg.Content.CacheDir, cfg.Content.Token)
	res, err := src.Resolve(ctx, cfg.Content.Version)
	if err != nil {
		return fmt.Errorf("resolve content version %q: %w", cfg.Content.Version, err)
	}
	defer func() { _ = res.Close() }()

	// 2. Read the manifest and verify the tree's integrity against its content hash.
	manifest, err := contentstore.ReadManifest(res.FS)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if err := contentstore.VerifyContentHash(res.FS, manifest.ContentHash); err != nil {
		return fmt.Errorf("content hash verification (checkout does not match published bytes): %w", err)
	}

	// 3. The manifest's pack list must exactly match the pack trees on disk — neither an extra pack
	//    (hashed but not imported) nor a manifest pack with no tree.
	if err := assertPacksMatchDirs(res.FS, manifest.Packs); err != nil {
		return err
	}

	// 4. Load exactly the manifest's packs from the checkout (deterministic order for a reproducible
	//    import). LoadPacks parses/merges each; a parse error fails here.
	packNames := append([]string(nil), manifest.Packs...)
	sort.Strings(packNames)
	packs, err := content.NewFSSource(res.FS).LoadPacks(ctx, packNames)
	if err != nil {
		return fmt.Errorf("load packs: %w", err)
	}
	if len(packs) != len(packNames) {
		return fmt.Errorf("a manifest pack has no packs/<name> tree: manifest lists %d, loaded %d", len(packNames), len(packs))
	}
	// 5. Reject a reserved core-namespace ref (it would clobber the embedded bootstrap pack).
	if v := content.LintReservedCoreRefs(packs); len(v) > 0 {
		return fmt.Errorf("pack %q ships a reserved core: namespace %s %q", v[0].Pack, v[0].Kind, v[0].Ref)
	}

	if check {
		slog.Info("content version OK (dry run — nothing imported or broadcast)",
			"version", manifest.Version, "sha", res.SHA, "packs", packNames)
		return nil
	}

	// 6. Import the version atomically into Postgres: ImportVersion prunes packs a prior version had
	//    that this one drops, stamps the content_version + registry, and mints the monotonic version —
	//    all in one tx (#212 slice 4 PR A). Re-importing the same SHA is idempotent (no bump/prune).
	pool, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()
	version, pruned, changed, err := pool.ImportVersion(ctx, packs, store.VersionMeta{
		ContentSHA: res.SHA, ManifestVersion: manifest.Version, ContentHash: manifest.ContentHash,
	})
	if err != nil {
		return fmt.Errorf("import version: %w", err)
	}
	if len(pruned) > 0 {
		// telos-pull is an uncoordinated CI/ops importer: it cannot check whether a dropped pack is
		// live-hosted (that gate is the director's job, PR E). Warn loudly — dropping a pack players are
		// standing in is a rolling-reboot operation, not a hot swap.
		slog.Warn("pruned packs no longer in this version (dropping a live-hosted pack strands players — treat as a rolling reboot)", "pruned", pruned)
	}
	if !changed {
		// The SHA already matches what Postgres serves — nothing was re-imported. Skip the broadcast: a
		// re-broadcast of an already-applied version is a needless fleet-wide prototype + Lua re-swap.
		slog.Info("content already at this version; nothing imported or broadcast", "version", version, "sha", res.SHA)
		return nil
	}
	slog.Info("imported content version", "version", version, "manifest", manifest.Version, "sha", res.SHA, "packs", packNames)

	// 7. Broadcast hot-reload invalidations so running shards pick up the new rows without a restart,
	//    stamped with the AUTHORITATIVE minted version. OPTIONAL + non-fatal (mirrors telos-seed): if
	//    NATS is unreachable the rows are still imported; running shards hot-reload on their next boot
	//    (reconcile-on-join, PR D) or reload.
	broadcast(ctx, cfg.NATS.URL, packs, version)
	return nil
}

// broadcast publishes per-ref invalidations for the imported packs, stamped with the authoritative
// content version. Best-effort: a bus failure is logged, never fatal — the rows are already imported.
func broadcast(ctx context.Context, natsURL string, packs []content.Pack, version uint64) {
	bus, err := contentbus.Connect(natsURL)
	if err != nil {
		slog.Warn("content bus unreachable; imported but running shards not hot-reloaded", "err", err)
		return
	}
	defer func() { _ = bus.Close() }()
	total := 0
	for _, pk := range packs {
		n, perr := contentbus.PublishPack(ctx, bus, pk, version)
		total += n
		if perr != nil {
			slog.Warn("publishing content invalidations failed (partial)", "pack", pk.Pack, "published", n, "err", perr)
		}
	}
	slog.Info("published content invalidations", "count", total, "version", version)
}

// assertPacksMatchDirs checks the set of pack names present under packs/ (a dir packs/<name>/ or a
// single file packs/<name>.yaml) equals want exactly — so the imported set and the hashed tree
// agree (the hash covers ALL of packs/, but only the manifest's packs are imported). The single-file
// form is matched as `.yaml` ONLY, mirroring loadPackFS (which does not recognize a `.yml` single
// file); an incidental top-level non-YAML file under packs/ (README/LICENSE) is hashed but is not a
// pack, so it is intentionally ignored here.
func assertPacksMatchDirs(fsys fs.FS, want []string) error {
	entries, err := fs.ReadDir(fsys, "packs")
	if err != nil {
		return fmt.Errorf("read packs/ dir: %w", err)
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			onDisk[name] = true
			continue
		}
		if strings.HasSuffix(name, ".yaml") {
			onDisk[strings.TrimSuffix(name, ".yaml")] = true
		}
	}
	wantSet := map[string]bool{}
	var missing []string
	for _, p := range want {
		wantSet[p] = true
		if !onDisk[p] {
			missing = append(missing, p)
		}
	}
	var extra []string
	for p := range onDisk {
		if !wantSet[p] {
			extra = append(extra, p)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		sort.Strings(missing)
		sort.Strings(extra)
		return fmt.Errorf("packs/ tree vs manifest mismatch: missing on disk [%s]; on disk but not in manifest [%s]",
			strings.Join(missing, ","), strings.Join(extra, ","))
	}
	return nil
}
