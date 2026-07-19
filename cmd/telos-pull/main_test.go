package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/jackc/pgx/v5"

	"github.com/double-nibble/telosmud/db"
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentpull"
	"github.com/double-nibble/telosmud/internal/contentstore"
	"github.com/double-nibble/telosmud/internal/store"
)

// buildContentRepo writes a content tree + a manifest (with the correct content hash unless
// badHash) and tags it v1. files maps repo-relative paths to bodies; packs is the manifest pack
// list. Returns the repo dir.
func buildContentRepo(t *testing.T, files map[string]string, packs []string, badHash bool) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	writeF := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if e := os.MkdirAll(filepath.Dir(p), 0o750); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, []byte(body), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	for rel, body := range files {
		writeF(rel, body)
	}
	hash, err := contentstore.ComputeContentHash(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	if badHash {
		hash = "sha256:deadbeef"
	}
	mf := "version: v1.0.0\ncontent_hash: " + hash + "\npacks: [" + join(packs) + "]\n"
	writeF(contentstore.ManifestFile, mf)

	wt, _ := repo.Worktree()
	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	c, err := wt.Commit("v1", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTag("v1.0.0", c, nil); err != nil {
		t.Fatal(err)
	}
	return dir
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

func cfgFor(repoDir string, t *testing.T) config.Config {
	var cfg config.Config
	cfg.Content.URL = repoDir
	cfg.Content.Version = "v1.0.0"
	cfg.Content.CacheDir = t.TempDir()
	return cfg
}

// samplePack is a minimal valid one-file pack tree.
func samplePack(name string) map[string]string {
	return map[string]string{
		"packs/" + name + "/pack.yaml": "pack: " + name + "\n",
		"packs/" + name + "/zones/z.yaml": "zones:\n  - ref: " + name + "\n    name: Z\n    start_room: " + name +
			":room:a\n    rooms:\n      - {ref: " + name + ":room:a, name: A}\n",
	}
}

// TestRunCheck_OK: a well-formed published version passes --check (no Postgres/NATS touched).
func TestRunCheck_OK(t *testing.T) {
	repo := buildContentRepo(t, samplePack("mainland"), []string{"mainland"}, false)
	if err := run(context.Background(), cfgFor(repo, t), true); err != nil {
		t.Fatalf("--check on a good version should pass: %v", err)
	}
}

// TestRunCheck_HashMismatch: a tampered/mis-hashed tree is rejected.
func TestRunCheck_HashMismatch(t *testing.T) {
	repo := buildContentRepo(t, samplePack("mainland"), []string{"mainland"}, true)
	if err := run(context.Background(), cfgFor(repo, t), true); err == nil {
		t.Fatal("--check should reject a content-hash mismatch")
	}
}

// TestRunCheck_PacksDirMismatch: a manifest listing a pack with no tree (or vice versa) is rejected.
func TestRunCheck_PacksDirMismatch(t *testing.T) {
	// Manifest claims two packs but only one is on disk.
	repo := buildContentRepo(t, samplePack("mainland"), []string{"mainland", "underdark"}, false)
	if err := run(context.Background(), cfgFor(repo, t), true); err == nil {
		t.Fatal("--check should reject a manifest pack with no tree")
	}

	// An extra pack on disk not named in the manifest.
	files := samplePack("mainland")
	for k, v := range samplePack("stray") {
		files[k] = v
	}
	repo2 := buildContentRepo(t, files, []string{"mainland"}, false)
	if err := run(context.Background(), cfgFor(repo2, t), true); err == nil {
		t.Fatal("--check should reject an extra pack on disk")
	}
}

// TestRunCheck_CoreNamespaceRejected: a pack shipping a core: ref is rejected.
func TestRunCheck_CoreNamespaceRejected(t *testing.T) {
	files := map[string]string{
		"packs/evil/pack.yaml": "pack: evil\n",
		"packs/evil/z.yaml":    "zones:\n  - ref: core\n    name: Evil\n    rooms:\n      - {ref: core:room:x, name: X}\n",
	}
	repo := buildContentRepo(t, files, []string{"evil"}, false)
	if err := run(context.Background(), cfgFor(repo, t), true); err == nil {
		t.Fatal("--check should reject a reserved core: namespace ref")
	}
}

// TestRun_ConfigGuards: missing URL / version are clear errors.
func TestRun_ConfigGuards(t *testing.T) {
	if err := run(context.Background(), config.Config{}, true); err == nil {
		t.Fatal("empty content URL should error")
	}
	var cfg config.Config
	cfg.Content.URL = "https://example.invalid/repo.git"
	if err := run(context.Background(), cfg, true); err == nil {
		t.Fatal("missing content version should error")
	}
}

// TestGatedImport (gated on TELOS_TEST_DSN): a real import lands the pack rows in Postgres and they
// read back through the normal content Source path.
func TestGatedImport(t *testing.T) {
	dsn := os.Getenv("TELOS_TEST_DSN")
	if dsn == "" {
		t.Skip("TELOS_TEST_DSN not set; skipping Postgres import test")
	}
	ctx := context.Background()
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	// Unique pack/zone names so parallel/re-runs never collide, and clean them up after.
	pack := "pulltest" + time.Now().Format("150405.000000")
	files := samplePack(pack)
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer conn.Close(context.Background())
		for _, s := range []string{
			`DELETE FROM exits WHERE from_room IN (SELECT ref FROM rooms WHERE pack=$1)`,
			`DELETE FROM rooms WHERE pack=$1`,
			`DELETE FROM zones WHERE pack=$1`,
		} {
			_, _ = conn.Exec(context.Background(), s, pack)
		}
	})

	repo := buildContentRepo(t, files, []string{pack}, false)
	cfg := cfgFor(repo, t)
	cfg.Postgres.DSN = dsn

	if err := run(ctx, cfg, false); err != nil {
		t.Fatalf("import run failed: %v", err)
	}
	// Verify the pack is now readable from Postgres via the normal content Source path.
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer pool.Close()
	lc, err := content.Load(ctx, pool, []string{pack})
	if err != nil {
		t.Fatalf("load imported pack: %v", err)
	}
	if lc.Zone(pack) == nil {
		t.Fatalf("imported pack's zone not found in Postgres; zones=%d", len(lc.Zones))
	}
}

// --- #427: the forced-prune record must survive contentpull.Pull ------------------------------------
//
// This is the FIRST link of the chain that carries a break-glass record back to the operator:
//
//	contentpull.Pull sets Result.PruneForced   <-- covered here (needs a real pool + a real git version)
//	  -> cmd/telos-director maps it to PullOutcome.ForcedPacks
//	  -> director's pullResultDetail renders it into PullResult.Detail
//	  -> world's deliverPullResult shows it to the builder
//
// Every later link has a unit test. Without this one, deleting the assignment in Pull leaves the whole
// suite GREEN while the operator silently stops being told what they overrode — which is exactly what a
// review mutation demonstrated. The rest of the chain being tested is not a substitute for its source.

// blockAll is a PruneGuard that vetoes every pack it is asked about, standing in for "the fleet is hosting
// these" without needing a live directory.
func blockAll(_ context.Context, _ contentpull.ZoneLister, pruned []string) ([]string, error) {
	return pruned, nil
}

// TestGatedForcedPrunePopulatesPruneForced (gated on TELOS_TEST_DSN) drives the real pipeline against a
// real Postgres: import v1 with two packs, then pull a version that drops one while the guard blocks it.
// Without force the pull must refuse and the registry must be untouched; with force it must commit AND
// report the overridden pack.
func TestGatedForcedPrunePopulatesPruneForced(t *testing.T) {
	dsn := os.Getenv("TELOS_TEST_DSN")
	if dsn == "" {
		t.Skip("TELOS_TEST_DSN not set; skipping the Postgres force-prune test")
	}
	ctx := context.Background()
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// v1 registers both packs, so dropping one in v2 is a real prune the guard can veto.
	both := samplePack("keepme")
	for k, v := range samplePack("dropme") {
		both[k] = v
	}
	repo1 := buildContentRepo(t, both, []string{"keepme", "dropme"}, false)
	cfg1 := cfgFor(repo1, t)
	cfg1.Postgres.DSN = dsn
	if _, err := contentpull.Pull(ctx, contentpull.Options{
		ContentURL: cfg1.Content.URL, Version: cfg1.Content.Version, CacheDir: cfg1.Content.CacheDir,
		PostgresDSN: dsn,
	}); err != nil {
		t.Fatalf("seeding v1: %v", err)
	}

	// v2 drops `dropme`, and the guard says it is live-hosted.
	repo2 := buildContentRepo(t, samplePack("keepme"), []string{"keepme"}, false)
	cfg2 := cfgFor(repo2, t)
	opts := contentpull.Options{
		ContentURL: cfg2.Content.URL, Version: cfg2.Content.Version, CacheDir: cfg2.Content.CacheDir,
		PostgresDSN: dsn, PruneGuard: blockAll,
	}

	// Without force: refused, and nothing changed.
	if _, err := contentpull.Pull(ctx, opts); err == nil {
		t.Fatal("a blocked prune must refuse without force")
	}
	cur, err := pool.CurrentContentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(cur.Packs, "dropme") {
		t.Fatal("a refused pull must leave the registry untouched")
	}

	// With force: committed, AND the overridden pack is reported.
	opts.ForcePrune = true
	res, err := contentpull.Pull(ctx, opts)
	if err != nil {
		t.Fatalf("a forced pull must proceed past the guard: %v", err)
	}
	if !containsStr(res.PruneForced, "dropme") {
		t.Fatalf("Result.PruneForced = %v, want it to name the pack the operator overrode — this is the ONLY "+
			"record that reaches the person who typed the command", res.PruneForced)
	}
	cur, err = pool.CurrentContentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(cur.Packs, "dropme") {
		t.Fatal("a forced prune must actually strip the pack from the registry")
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
