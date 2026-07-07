package contentstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestFile is the name (at the content-repo root) of the file describing a published version.
// CI commits it at the tagged commit so a tag is self-describing.
const ManifestFile = "manifest.yaml"

// Manifest describes one published content version (#212 slice 3). It is committed at the content
// repo root at the tagged commit; the importer reads it to learn which packs the version ships and
// to verify the tree's integrity against ContentHash.
type Manifest struct {
	// Version is the human name of this version (a tag, e.g. "v1.4.0", or a monotonic "content/42").
	Version string `yaml:"version"`
	// ContentHash is the deterministic hash of the packs/ tree at publish time (see ComputeContentHash).
	// The importer recomputes it over the checkout and rejects a mismatch. It is a CHECKOUT-INTEGRITY
	// check (guards against accidental drift/corruption of a pulled tree), NOT authenticity — the
	// manifest is itself unsigned and mutable, so the real trust anchor is the resolved git SHA that
	// pins this manifest (PublishedSource.Resolve), of which this hash is a corroborating detail.
	ContentHash string `yaml:"content_hash"`
	// Packs is the set of pack names this version ships. It is the single source of "what's in this
	// version" — the importer loads exactly these, so no out-of-band enabled list is needed.
	Packs []string `yaml:"packs"`
	// CreatedAt / CIRun are provenance (RFC3339 timestamp + the CI run URL). Optional.
	CreatedAt string `yaml:"created_at"`
	CIRun     string `yaml:"ci_run"`
	// EngineMin is an optional minimum engine version that can serve this content (a future gate).
	EngineMin string `yaml:"engine_min"`
}

// ReadManifest reads and parses ManifestFile from the root of fsys (a checked-out content version's
// Resolved.FS). A missing manifest is an error: a published version must describe itself.
func ReadManifest(fsys fs.FS) (Manifest, error) {
	data, err := fs.ReadFile(fsys, ManifestFile)
	if err != nil {
		return Manifest{}, fmt.Errorf("contentstore: read %s: %w", ManifestFile, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("contentstore: parse %s: %w", ManifestFile, err)
	}
	if m.Version == "" {
		return Manifest{}, fmt.Errorf("contentstore: %s has no version", ManifestFile)
	}
	if len(m.Packs) == 0 {
		return Manifest{}, fmt.Errorf("contentstore: %s lists no packs", ManifestFile)
	}
	// Reject a duplicate pack name — it would otherwise import the same pack twice.
	seen := make(map[string]bool, len(m.Packs))
	for _, p := range m.Packs {
		if seen[p] {
			return Manifest{}, fmt.Errorf("contentstore: %s lists pack %q more than once", ManifestFile, p)
		}
		seen[p] = true
	}
	return m, nil
}

// ComputeContentHash returns a deterministic SHA-256 over the entire packs/ subtree of fsys — every
// regular file's path and bytes, walked in sorted path order. Any edit, rename, add, or delete
// under packs/ changes the hash, so it is the version's integrity identity. CI writes it into the
// manifest at publish time; the importer recomputes it over the checkout and compares. A tree with
// no packs/ dir hashes the empty set (a legitimately empty version), not an error.
func ComputeContentHash(fsys fs.FS) (string, error) {
	var files []string
	info, serr := fs.Stat(fsys, "packs")
	switch {
	case errors.Is(serr, fs.ErrNotExist):
		// A missing packs/ dir is legitimately an empty version — hash the empty set.
	case serr != nil:
		// A permission/I/O error, or an os.Root-refused symlinked packs/, must NOT masquerade as an
		// empty version (which would hash to the well-known empty digest and hide the condition).
		return "", fmt.Errorf("contentstore: stat packs/ for content hash: %w", serr)
	case !info.IsDir():
		return "", fmt.Errorf("contentstore: packs is not a directory")
	default:
		if werr := fs.WalkDir(fsys, "packs", func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.Type().IsRegular() { // skip dirs; skip symlinks/devices (the FS is os.Root-confined anyway)
				files = append(files, p)
			}
			return nil
		}); werr != nil {
			return "", fmt.Errorf("contentstore: walk packs/ for content hash: %w", werr)
		}
	}
	sort.Strings(files)

	h := sha256.New()
	for _, f := range files {
		data, rerr := fs.ReadFile(fsys, f)
		if rerr != nil {
			return "", fmt.Errorf("contentstore: read %q for content hash: %w", f, rerr)
		}
		// Domain-separate path from content, and one file from the next, so no concatenation collision
		// (e.g. renaming a/b -> ab can't yield the same stream): "<pathlen>\n<path><contentlen>\n<content>".
		_, _ = fmt.Fprintf(h, "%d\n%s%d\n", len(f), f, len(data))
		_, _ = h.Write(data)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyContentHash recomputes the packs/ hash of fsys and checks it equals expected (as recorded in
// a manifest). A mismatch means the checked-out tree is not the published bytes — reject it.
func VerifyContentHash(fsys fs.FS, expected string) error {
	if strings.TrimSpace(expected) == "" {
		return fmt.Errorf("contentstore: manifest has no content_hash to verify against")
	}
	got, err := ComputeContentHash(fsys)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("contentstore: content hash mismatch: manifest %s, computed %s", expected, got)
	}
	return nil
}
