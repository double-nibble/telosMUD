// Package contentpull is the shared content-install pipeline (#212): resolve a published version from
// the external git content store, verify it, import it atomically into Postgres, and broadcast a
// hot-reload. Both the CLI/CI importer (cmd/telos-pull) and the director-coordinated in-game pull
// (slice 4 PR E) run this same pipeline, so the two never diverge.
package contentpull

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/contentstore"
	"github.com/double-nibble/telosmud/internal/store"
)

// Options configures one pull. ContentURL + Version identify the published version in the external git
// store; Token is an optional PAT for a private repo; CacheDir is the on-disk checkout cache. PostgresDSN
// is the import target; NATSURL is the hot-reload broadcast bus (both empty on a Check dry run). Check
// runs the validation pre-flight only — no import, no broadcast (the content-repo CI merge gate).
type Options struct {
	ContentURL  string
	Version     string
	Token       string
	CacheDir    string
	PostgresDSN string
	NATSURL     string
	Check       bool

	// PruneGuard, when set (the director-coordinated path), is consulted before the import with the packs
	// this version would PRUNE. A non-empty result REFUSES the pull before any DB change — the veto that
	// stops hot-stripping a pack players are standing in. nil (telos-pull / CI) skips it: the CLI importer
	// has no fleet view. See guard.go.
	PruneGuard PruneGuard
}

// Result reports what a pull did. On a Check run only SHA/ManifestVersion/Packs are set (Checked=true).
// Changed is false on an idempotent re-pull of the same SHA (nothing imported or broadcast).
type Result struct {
	Version         uint64   // the minted monotonic content version (0 on Check / not reached)
	SHA             string   // the resolved immutable git commit
	ManifestVersion string   // the human manifest tag
	Packs           []string // the version's pack set (sorted)
	Pruned          []string // packs a prior version had that this one drops
	Changed         bool     // false => the SHA already matched Postgres (no import/broadcast)
	Published       int      // hot-reload invalidations broadcast
	Checked         bool     // true => a Check dry run (validated only)
}

// Pull runs the install pipeline: resolve → manifest → verify hash → packs==dirs → load → lint, then
// (unless Check) import atomically + broadcast. Returns a Result the caller logs. The import is atomic
// and the broadcast is best-effort (a NATS failure is non-fatal — the rows are durable and shards catch
// up on reconnect via reconcile-on-join).
func Pull(ctx context.Context, opts Options) (Result, error) {
	if opts.ContentURL == "" {
		return Result{}, fmt.Errorf("no content store configured (set content.url / TELOS_CONTENT_URL)")
	}
	if opts.Version == "" {
		return Result{}, fmt.Errorf("no content version pinned (set content.version / TELOS_CONTENT_VERSION)")
	}

	// 1. Resolve the published version (git tag/SHA) to a checked-out tree + immutable SHA.
	src := contentstore.NewGit(opts.ContentURL, opts.CacheDir, opts.Token)
	res, err := src.Resolve(ctx, opts.Version)
	if err != nil {
		return Result{}, fmt.Errorf("resolve content version %q: %w", opts.Version, err)
	}
	defer func() { _ = res.Close() }()

	// 2. Read the manifest and verify the tree's integrity against its content hash.
	manifest, err := contentstore.ReadManifest(res.FS)
	if err != nil {
		return Result{}, fmt.Errorf("read manifest: %w", err)
	}
	if err := contentstore.VerifyContentHash(res.FS, manifest.ContentHash); err != nil {
		return Result{}, fmt.Errorf("content hash verification (checkout does not match published bytes): %w", err)
	}

	// 3. The manifest's pack list must exactly match the pack trees on disk.
	if err := assertPacksMatchDirs(res.FS, manifest.Packs); err != nil {
		return Result{}, err
	}

	// 4. Load exactly the manifest's packs from the checkout (deterministic order).
	packNames := append([]string(nil), manifest.Packs...)
	sort.Strings(packNames)
	packs, err := content.NewFSSource(res.FS).LoadPacks(ctx, packNames)
	if err != nil {
		return Result{}, fmt.Errorf("load packs: %w", err)
	}
	if len(packs) != len(packNames) {
		return Result{}, fmt.Errorf("a manifest pack has no packs/<name> tree: manifest lists %d, loaded %d", len(packNames), len(packs))
	}
	// 5. Reject a reserved core-namespace ref (it would clobber the embedded bootstrap pack).
	if v := content.LintReservedCoreRefs(packs); len(v) > 0 {
		return Result{}, fmt.Errorf("pack %q ships a reserved core: namespace %s %q", v[0].Pack, v[0].Kind, v[0].Ref)
	}

	base := Result{SHA: res.SHA, ManifestVersion: manifest.Version, Packs: packNames}
	if opts.Check {
		base.Checked = true
		return base, nil
	}

	// 6. Open Postgres — the import target and, for the director's guard, the source of the current
	//    registry + each pack's zones.
	pool, err := store.Open(ctx, opts.PostgresDSN)
	if err != nil {
		return Result{}, fmt.Errorf("connect to postgres: %w", err)
	}
	defer pool.Close()

	// 6a. Live-hosted-pack prune guard (director-coordinated path only). Compute the packs this version
	//     would prune (registry − incoming) and, if any, let the guard veto stripping one that is currently
	//     hosted. A veto refuses the pull BEFORE any DB change (the import below is what commits the prune).
	if opts.PruneGuard != nil {
		cur, err := pool.CurrentContentVersion(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("read current content registry: %w", err)
		}
		if would := prunePreview(cur.Packs, packNames); len(would) > 0 {
			blocked, err := opts.PruneGuard(ctx, pool, would)
			if err != nil {
				return Result{}, err
			}
			if len(blocked) > 0 {
				return Result{}, fmt.Errorf(
					"refusing content version %q: it would strip live-hosted pack(s) [%s] — players are in those zones; drain them or roll a reboot before removing the pack(s)",
					opts.Version, strings.Join(blocked, ", "))
			}
		}
	}

	// 7. Import the version atomically (prune dropped packs + stamp content_version/registry + mint the
	//    monotonic version, all one tx). Idempotent by SHA (changed=false => no bump/prune).
	version, pruned, changed, err := pool.ImportVersion(ctx, packs, store.VersionMeta{
		ContentSHA: res.SHA, ManifestVersion: manifest.Version, ContentHash: manifest.ContentHash,
	})
	if err != nil {
		return Result{}, fmt.Errorf("import version: %w", err)
	}
	base.Version = version
	base.Pruned = pruned
	base.Changed = changed
	if !changed {
		return base, nil // the SHA already matched Postgres — nothing imported, skip the broadcast
	}

	// 8. Broadcast the hot-reload (stamped with the authoritative version), then the version-complete
	//    sentinel. Best-effort — a NATS failure is non-fatal (rows are durable; shards catch up on
	//    reconnect via reconcile-on-join).
	base.Published = broadcast(ctx, opts.NATSURL, packs, version)
	return base, nil
}

// broadcast publishes per-ref invalidations for each pack, then the trailing version-complete sentinel,
// stamped with the authoritative version. Best-effort: a bus failure is logged, never fatal. Returns the
// count of invalidations published.
func broadcast(ctx context.Context, natsURL string, packs []content.Pack, version uint64) int {
	bus, err := contentbus.Connect(natsURL)
	if err != nil {
		slog.Warn("content bus unreachable; imported but running shards not hot-reloaded", "err", err)
		return 0
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
	// The trailing version-complete sentinel — LAST on the wire, after every pack (reconcile-on-join).
	if err := contentbus.PublishVersionComplete(ctx, bus, version); err != nil {
		slog.Warn("publishing the version-complete sentinel failed", "version", version, "err", err)
	}
	return total
}

// assertPacksMatchDirs checks the set of pack names present under packs/ (a dir packs/<name>/ or a
// single file packs/<name>.yaml) equals want exactly — so the imported set and the hashed tree agree
// (the hash covers ALL of packs/, but only the manifest's packs are imported). Single-file is matched as
// `.yaml` only, mirroring loadPackFS; an incidental top-level non-YAML file under packs/ is ignored.
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
