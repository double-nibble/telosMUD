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
	"log/slog"
	"os"

	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/contentpull"
	"github.com/double-nibble/telosmud/internal/contentstore"
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

// run executes the shared pull pipeline (internal/contentpull) and logs the outcome. It returns an
// error instead of exiting so it is testable. --check stops after validation and touches neither
// Postgres nor NATS. The pipeline itself is shared with the director-coordinated pull (slice 4 PR E).
func run(ctx context.Context, cfg config.Config, check bool) error {
	res, err := contentpull.Pull(ctx, contentpull.Options{
		ContentURL:  cfg.Content.URL,
		Version:     cfg.Content.Version,
		Token:       cfg.Content.Token,
		CacheDir:    cfg.Content.CacheDir,
		PostgresDSN: cfg.Postgres.DSN,
		NATSURL:     cfg.NATS.URL,
		Check:       check,
	})
	if err != nil {
		return err
	}
	switch {
	case res.Checked:
		slog.Info("content version OK (dry run — nothing imported or broadcast)",
			"version", res.ManifestVersion, "sha", res.SHA, "packs", res.Packs)
	case !res.Changed:
		slog.Info("content already at this version; nothing imported or broadcast",
			"version", res.Version, "sha", res.SHA)
	default:
		if len(res.Pruned) > 0 {
			// telos-pull is an uncoordinated CI/ops importer: it cannot check whether a dropped pack is
			// live-hosted (that gate is the director's job, PR E). Warn loudly — dropping a pack players
			// are standing in is a rolling-reboot operation, not a hot swap.
			slog.Warn("pruned packs no longer in this version (dropping a live-hosted pack strands players — treat as a rolling reboot)", "pruned", res.Pruned)
		}
		slog.Info("imported content version", "version", res.Version, "manifest", res.ManifestVersion,
			"sha", res.SHA, "packs", res.Packs, "invalidations", res.Published)
	}
	return nil
}
