package contentstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func fsWith(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

func TestReadManifest(t *testing.T) {
	fsys := fsWith(map[string]string{
		"manifest.yaml": "version: v1.4.0\ncontent_hash: sha256:abc\npacks: [mainland, underdark]\ncreated_at: 2026-07-07T00:00:00Z\nci_run: https://ci/1\n",
	})
	m, err := ReadManifest(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v1.4.0" || m.ContentHash != "sha256:abc" {
		t.Fatalf("bad parse: %+v", m)
	}
	if len(m.Packs) != 2 || m.Packs[0] != "mainland" || m.Packs[1] != "underdark" {
		t.Fatalf("packs not parsed: %+v", m.Packs)
	}
}

func TestReadManifest_Errors(t *testing.T) {
	// Missing manifest.
	if _, err := ReadManifest(fsWith(nil)); err == nil {
		t.Error("missing manifest should error")
	}
	// No version.
	if _, err := ReadManifest(fsWith(map[string]string{"manifest.yaml": "packs: [a]\n"})); err == nil {
		t.Error("manifest without a version should error")
	}
	// No packs.
	if _, err := ReadManifest(fsWith(map[string]string{"manifest.yaml": "version: v1\n"})); err == nil {
		t.Error("manifest without packs should error")
	}
	// Duplicate pack.
	if _, err := ReadManifest(fsWith(map[string]string{"manifest.yaml": "version: v1\npacks: [a, a]\n"})); err == nil {
		t.Error("manifest with a duplicate pack should error")
	}
}

func TestComputeContentHash_StatErrorSurfaced(t *testing.T) {
	// packs/ present but a FILE, not a dir → surfaced, not treated as an empty version.
	if _, err := ComputeContentHash(fsWith(map[string]string{"packs": "i am a file\n"})); err == nil {
		t.Fatal("a non-directory packs/ must be an error, not the empty-set hash")
	}
}

// TestManifestOverResolvedFS ties PR1+PR2: build a git content repo whose manifest carries the
// correct content_hash, resolve it (os.Root FS), then ReadManifest + VerifyContentHash over that
// real rooted checkout — the exact path the importer will take.
func TestManifestOverResolvedFS(t *testing.T) {
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
	writeF("packs/sample/pack.yaml", "pack: sample\n")
	writeF("packs/sample/zones/z.yaml", "zones: []\n")
	// Compute the hash over the on-disk packs/ (the bytes that will be committed), then pin it in the
	// manifest — exactly what content-repo CI does at tag time.
	hash, err := ComputeContentHash(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	writeF("manifest.yaml", "version: v1.0.0\ncontent_hash: "+hash+"\npacks: [sample]\n")

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

	res, err := NewGit(dir, t.TempDir(), "").Resolve(context.Background(), "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	m, err := ReadManifest(res.FS)
	if err != nil {
		t.Fatalf("ReadManifest over resolved FS: %v", err)
	}
	if m.Version != "v1.0.0" || len(m.Packs) != 1 || m.Packs[0] != "sample" {
		t.Fatalf("manifest parsed wrong: %+v", m)
	}
	if err := VerifyContentHash(res.FS, m.ContentHash); err != nil {
		t.Fatalf("content hash should verify over the resolved checkout: %v", err)
	}
}

func TestComputeContentHash_DeterministicAndSensitive(t *testing.T) {
	base := map[string]string{
		"packs/a/pack.yaml":    "pack: a\n",
		"packs/a/zones/z.yaml": "zones: []\n",
		"packs/b/pack.yaml":    "pack: b\n",
		"manifest.yaml":        "version: v1\npacks: [a, b]\n", // NOT under packs/, must not affect the hash
		"README.md":            "ignored\n",
	}
	h1, err := ComputeContentHash(fsWith(base))
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" || h1 == "sha256:" {
		t.Fatalf("empty hash: %q", h1)
	}
	// Deterministic: same tree → same hash.
	h2, _ := ComputeContentHash(fsWith(base))
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %s vs %s", h1, h2)
	}
	// Insensitive to files OUTSIDE packs/ (manifest self-reference, docs).
	outside := cloneMap(base)
	outside["manifest.yaml"] = "version: v2\npacks: [a, b]\ncontent_hash: sha256:whatever\n"
	outside["README.md"] = "totally different\n"
	if h, _ := ComputeContentHash(fsWith(outside)); h != h1 {
		t.Fatalf("hash changed on a non-packs edit: %s vs %s", h, h1)
	}
	// Sensitive to a content edit under packs/.
	edited := cloneMap(base)
	edited["packs/a/pack.yaml"] = "pack: a\n# changed\n"
	if h, _ := ComputeContentHash(fsWith(edited)); h == h1 {
		t.Fatal("hash did not change on a packs/ content edit")
	}
	// Sensitive to a rename (path is domain-separated from content).
	renamed := cloneMap(base)
	delete(renamed, "packs/a/zones/z.yaml")
	renamed["packs/a/zones/zz.yaml"] = "zones: []\n"
	if h, _ := ComputeContentHash(fsWith(renamed)); h == h1 {
		t.Fatal("hash did not change on a rename")
	}
}

func TestVerifyContentHash(t *testing.T) {
	fsys := fsWith(map[string]string{"packs/a/pack.yaml": "pack: a\n"})
	good, _ := ComputeContentHash(fsys)
	if err := VerifyContentHash(fsys, good); err != nil {
		t.Fatalf("matching hash should verify: %v", err)
	}
	if err := VerifyContentHash(fsys, "sha256:deadbeef"); err == nil {
		t.Fatal("a mismatched hash must fail verification")
	}
	if err := VerifyContentHash(fsys, ""); err == nil {
		t.Fatal("an empty expected hash must fail verification")
	}
}

func TestEmitManifest(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("packs/reference/pack.yaml", "pack: reference\n")
	write("packs/reference/zones/z.yaml", "zones: []\n")
	write("packs/second/pack.yaml", "pack: second\n")

	m, err := EmitManifest(dir, "v1.0.0", "https://ci/run/1")
	if err != nil {
		t.Fatalf("EmitManifest: %v", err)
	}
	if m.Version != "v1.0.0" || m.CIRun != "https://ci/run/1" {
		t.Fatalf("manifest fields wrong: %+v", m)
	}
	if len(m.Packs) != 2 || m.Packs[0] != "reference" || m.Packs[1] != "second" {
		t.Fatalf("emitted packs = %v, want sorted [reference second]", m.Packs)
	}

	// The written manifest.yaml reads back and its hash verifies over the same tree — the exact
	// round-trip the importer performs after a git checkout.
	fsys := os.DirFS(dir)
	got, err := ReadManifest(fsys)
	if err != nil {
		t.Fatalf("ReadManifest of emitted file: %v", err)
	}
	if got.Version != "v1.0.0" || got.ContentHash != m.ContentHash {
		t.Fatalf("round-tripped manifest = %+v, want the emitted one", got)
	}
	if err := VerifyContentHash(fsys, got.ContentHash); err != nil {
		t.Fatalf("emitted content hash must verify over the tree: %v", err)
	}
	// Writing the manifest did not change the hash (it is at the root, not under packs/): a second
	// emit is stable.
	m2, err := EmitManifest(dir, "v1.0.0", "")
	if err != nil {
		t.Fatal(err)
	}
	if m2.ContentHash != m.ContentHash {
		t.Fatalf("re-emit changed the hash: %s -> %s", m.ContentHash, m2.ContentHash)
	}
}

func TestListPacks(t *testing.T) {
	fsys := fsWith(map[string]string{
		"packs/alpha/pack.yaml": "pack: alpha\n",
		"packs/beta.yaml":       "pack: beta\n", // single-file pack
		"packs/README.md":       "ignored\n",    // not a pack
	})
	got, err := ListPacks(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("ListPacks = %v, want [alpha beta]", got)
	}
}

func TestComputeContentHash_EmptyTree(t *testing.T) {
	// No packs/ dir → the empty set, not an error.
	h, err := ComputeContentHash(fsWith(map[string]string{"manifest.yaml": "version: v1\n"}))
	if err != nil {
		t.Fatalf("empty tree should not error: %v", err)
	}
	if h == "" {
		t.Fatal("empty tree should still produce a hash")
	}
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
