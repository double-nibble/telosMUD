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
