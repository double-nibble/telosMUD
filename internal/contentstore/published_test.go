package contentstore

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/double-nibble/telosmud/internal/content"
)

// makeContentRepo builds a throwaway git content repo with a one-pack tree and a tag, returning its
// path and the tagged commit SHA. This is the in-test fixture that lets slice-3 PRs exercise the
// real resolve→checkout→fs.FS path with NO external content repo.
func makeContentRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("packs/sample/pack.yaml", "pack: sample\n")
	write("packs/sample/zones/z.yaml",
		"zones:\n  - ref: sample\n    name: Sample\n    start_room: sample:room:a\n    rooms:\n      - {ref: sample:room:a, name: Room A}\n")
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	c, err := wt.Commit("v1.0.0 content", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTag("v1.0.0", c, nil); err != nil {
		t.Fatal(err)
	}
	return dir, c.String()
}

// TestGitPublishedSource_ResolveAndLoad: resolve a tag, wrap the checkout as a content.Source, and
// load the pack — the full external-store read path, end to end, against a local fixture.
func TestGitPublishedSource_ResolveAndLoad(t *testing.T) {
	repoDir, wantSHA := makeContentRepo(t)
	src := NewGit(repoDir, t.TempDir(), "")

	res, err := src.Resolve(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer res.Close()
	if res.SHA != wantSHA {
		t.Fatalf("resolved SHA = %s, want %s", res.SHA, wantSHA)
	}
	// The checkout feeds content.NewFSSource verbatim (reusing loadPackFS).
	packs, err := content.NewFSSource(res.FS).LoadPacks(context.Background(), []string{"sample"})
	if err != nil {
		t.Fatalf("LoadPacks: %v", err)
	}
	if len(packs) != 1 || packs[0].Pack != "sample" {
		t.Fatalf("expected the sample pack, got %+v", packs)
	}
	if len(packs[0].Zones) != 1 || packs[0].Zones[0].Ref != "sample" {
		t.Fatalf("sample pack zone not loaded: %+v", packs[0].Zones)
	}
}

// TestGitPublishedSource_ReusesClone: a second Resolve reuses the cached clone (open+fetch, no
// re-clone) and still resolves correctly.
func TestGitPublishedSource_ReusesClone(t *testing.T) {
	repoDir, _ := makeContentRepo(t)
	cache := t.TempDir()
	src := NewGit(repoDir, cache, "")
	if _, err := src.Resolve(context.Background(), "v1.0.0"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if _, err := src.Resolve(context.Background(), "v1.0.0"); err != nil {
		t.Fatalf("second Resolve (cached): %v", err)
	}
	// Exactly one working clone dir was created under the cache.
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one working clone under the cache, got %d: %v", len(entries), entries)
	}
}

// TestGitPublishedSource_UnknownVersion: an unknown tag/ref is a clean error, not a panic.
func TestGitPublishedSource_UnknownVersion(t *testing.T) {
	repoDir, _ := makeContentRepo(t)
	src := NewGit(repoDir, t.TempDir(), "")
	if _, err := src.Resolve(context.Background(), "v9.9.9-nope"); err == nil {
		t.Fatal("resolving an unknown version should error")
	}
	if _, err := src.Resolve(context.Background(), ""); err == nil {
		t.Fatal("empty version should error")
	}
}

// TestGitPublishedSource_ResolvesShortSHA: a version given as a commit SHA also resolves.
func TestGitPublishedSource_ResolvesShortSHA(t *testing.T) {
	repoDir, sha := makeContentRepo(t)
	src := NewGit(repoDir, t.TempDir(), "")
	res, err := src.Resolve(context.Background(), sha[:10])
	if err != nil {
		t.Fatalf("Resolve short SHA: %v", err)
	}
	defer res.Close()
	if res.SHA != sha {
		t.Fatalf("short-SHA resolved to %s, want %s", res.SHA, sha)
	}
}

// TestGitPublishedSource_AnnotatedTagPeels: an ANNOTATED tag (a tag object, not a lightweight ref)
// must resolve to its underlying COMMIT, since Checkout needs a commit hash. (The other tests use
// lightweight tags, which never exercise the peel path.)
func TestGitPublishedSource_AnnotatedTagPeels(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("version: v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "t", Email: "t@example.com"}
	c, err := wt.Commit("c", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTag("v2.0.0", c, &git.CreateTagOptions{Tagger: sig, Message: "release v2"}); err != nil {
		t.Fatal(err)
	}
	res, err := NewGit(dir, t.TempDir(), "").Resolve(context.Background(), "v2.0.0")
	if err != nil {
		t.Fatalf("Resolve annotated tag: %v", err)
	}
	defer res.Close()
	if res.SHA != c.String() {
		t.Fatalf("annotated tag resolved to %s, want the commit %s", res.SHA, c.String())
	}
}

// TestGitPublishedSource_MovedTagRefetched: a re-pointed tag is picked up on a subsequent Resolve
// (the force refspec + open+fetch path), so a cached clone never serves a stale tag target.
func TestGitPublishedSource_MovedTagRefetched(t *testing.T) {
	dir := t.TempDir()
	repo, _ := git.PlainInit(dir, false)
	sig := &object.Signature{Name: "t", Email: "t@example.com"}
	wt, _ := repo.Worktree()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = wt.AddGlob(".")
	c1, _ := wt.Commit("c1", &git.CommitOptions{Author: sig})
	if _, err := repo.CreateTag("latest", c1, nil); err != nil {
		t.Fatal(err)
	}

	cache := t.TempDir()
	src := NewGit(dir, cache, "")
	res1, err := src.Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	res1.Close()
	if res1.SHA != c1.String() {
		t.Fatalf("first resolve = %s, want %s", res1.SHA, c1.String())
	}

	// Move the tag to a new commit.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = wt.AddGlob(".")
	c2, _ := wt.Commit("c2", &git.CommitOptions{Author: sig})
	_ = repo.DeleteTag("latest")
	if _, err := repo.CreateTag("latest", c2, nil); err != nil {
		t.Fatal(err)
	}

	res2, err := src.Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Close()
	if res2.SHA != c2.String() {
		t.Fatalf("moved tag resolved to %s, want the new commit %s", res2.SHA, c2.String())
	}
}

// TestGitPublishedSource_SymlinkEscapeBlocked (SECURITY, #212 slice-3 review): a symlink committed
// under packs/ that points OUT of the content tree must NOT be followed at read time — os.Root
// rejects the escape, so a malicious content repo cannot read arbitrary host files into an import.
func TestGitPublishedSource_SymlinkEscapeBlocked(t *testing.T) {
	// A secret host file OUTSIDE the content repo.
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.yaml")
	if err := os.WriteFile(secret, []byte("pack: stolen\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "packs/evil"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "packs/evil/pack.yaml"), []byte("pack: evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The escaping symlink, named as a pack file so loadPackFS would try to read it.
	if err := os.Symlink(secret, filepath.Join(dir, "packs/evil/steal.yaml")); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if err := wt.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	c, err := wt.Commit("evil", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTag("v1", c, nil); err != nil {
		t.Fatal(err)
	}

	res, err := NewGit(dir, t.TempDir(), "").Resolve(context.Background(), "v1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer res.Close()
	// The structural guarantee: reading the escaping symlink through the rooted FS fails.
	if _, err := fs.ReadFile(res.FS, "packs/evil/steal.yaml"); err == nil {
		t.Fatal("SECURITY: rooted FS followed an escaping symlink out of the checkout")
	}
	// And the secret must never surface in a loaded pack.
	packs, _ := content.NewFSSource(res.FS).LoadPacks(context.Background(), []string{"evil"})
	for _, p := range packs {
		if p.Pack == "stolen" {
			t.Fatal("SECURITY: escaping symlink leaked an out-of-tree file into a pack")
		}
	}
}

// TestGitPublishedSource_RejectsBadVersion: revision metacharacters are rejected before touching git.
func TestGitPublishedSource_RejectsBadVersion(t *testing.T) {
	src := NewGit("https://example.invalid/repo.git", t.TempDir(), "")
	for _, bad := range []string{"HEAD@{1}", ":/fix", "v1^", "main~2", "a:b"} {
		if _, err := src.Resolve(context.Background(), bad); err == nil {
			t.Errorf("version %q should be rejected", bad)
		}
	}
}

// TestSafeURL_RedactsCredentials: embedded userinfo never appears in the URL used for errors/logs.
func TestSafeURL_RedactsCredentials(t *testing.T) {
	g := NewGit("https://user:supersecrettoken@example.com/org/content.git", "", "")
	got := g.safeURL()
	if strings.Contains(got, "supersecrettoken") || strings.Contains(got, "user") {
		t.Fatalf("safeURL leaked credentials: %q", got)
	}
	if !strings.Contains(got, "example.com") {
		t.Fatalf("safeURL over-redacted (lost the host): %q", got)
	}
}
