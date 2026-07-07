// Package contentstore resolves an EXTERNAL, versioned content store (#212 slice 3) to a
// filesystem tree the content loader can read. Decision A (git tag/SHA pull): a "published
// version" is a git tag/SHA in the content repository; Resolve fetches it into an on-disk cache,
// checks it out, and returns an fs.FS rooted at the content tree — which internal/content wraps
// with content.NewFSSource (reusing loadPackFS verbatim). The importer (cmd/telos-pull) is the
// only runtime consumer; the world keeps reading from Postgres (Model 1). The interface leaves
// room for a non-git artifact-store impl behind the same seam later.
package contentstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	httptransport "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// versionPattern constrains a version string to tag/branch/SHA characters, rejecting go-git
// revision metacharacters (@{…} reflog, :/regex commit search, ^ ~ : path/tree addressing) that
// would make a recorded "version" something other than the immutable identity the API promises.
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// PublishedSource resolves an immutable published content version to a filesystem tree.
type PublishedSource interface {
	// Resolve fetches the given version (a git tag or SHA; the default branch also resolves) and
	// returns a filesystem rooted at the content tree (so fs.Stat(FS, "packs/<name>") works) plus
	// the immutable commit SHA it resolved to. A tag is mutable, so callers should treat the SHA as
	// the true identity. Callers should Close the Resolved when done reading.
	Resolve(ctx context.Context, version string) (Resolved, error)
}

// Resolved is a checked-out content version.
type Resolved struct {
	// FS is rooted at the content-repo tree (packs/<name>/… live under it), via os.Root so a
	// symlink committed in the (semi-trusted) content repo CANNOT escape the checkout at read time
	// (a committed `packs/x/a.yaml -> /etc/passwd` would otherwise be followed by os.DirFS). It
	// reads the on-disk checkout; it stays valid until Close, or until the next Resolve on the same
	// source re-checks-out the cache.
	FS fs.FS
	// SHA is the immutable commit the version resolved to (a tag peeled to its commit).
	SHA string

	root *os.Root // held open so FS works; Close releases its descriptor
}

// Close releases the underlying rooted directory handle. Safe to call on a zero Resolved.
func (r Resolved) Close() error {
	if r.root != nil {
		return r.root.Close()
	}
	return nil
}

// GitPublishedSource pulls published versions from a git remote (decision A). It keeps ONE working
// clone per remote under CacheDir and checks out the requested version into it on each Resolve.
// It is built for the single-invocation importer (cmd/telos-pull), so it does no cross-process
// locking; concurrent Resolves against the same CacheDir are not supported.
type GitPublishedSource struct {
	url      string
	cacheDir string
	auth     transport.AuthMethod
}

// NewGit returns a git-backed PublishedSource. url is the content repo remote; cacheDir is where the
// working clone lives (defaults to <os.TempDir>/telos-content when empty); token is an optional PAT
// for a private repo (GitHub-style x-access-token basic auth) — empty means anonymous HTTPS.
func NewGit(gitURL, cacheDir, token string) *GitPublishedSource {
	if cacheDir == "" {
		// Prefer a user-private cache dir over a world-shared temp dir (a private content repo's tree
		// + .git/config land here). Fall back to TempDir only if the user cache dir is unavailable.
		if base, err := os.UserCacheDir(); err == nil {
			cacheDir = filepath.Join(base, "telos-content")
		} else {
			cacheDir = filepath.Join(os.TempDir(), "telos-content")
		}
	}
	var auth transport.AuthMethod
	if token != "" {
		auth = &httptransport.BasicAuth{Username: "x-access-token", Password: token}
	}
	return &GitPublishedSource{url: gitURL, cacheDir: cacheDir, auth: auth}
}

// Resolve implements PublishedSource: ensure the working clone exists and is up to date, resolve the
// version to a commit, check it out, and return the checked-out tree as an fs.FS.
func (g *GitPublishedSource) Resolve(ctx context.Context, version string) (Resolved, error) {
	if version == "" {
		return Resolved{}, fmt.Errorf("contentstore: empty version")
	}
	if !versionPattern.MatchString(version) {
		return Resolved{}, fmt.Errorf("contentstore: invalid version %q (allowed: letters, digits, and . _ / + -)", version)
	}
	repoDir := filepath.Join(g.cacheDir, urlKey(g.url))
	repo, err := g.openOrClone(ctx, repoDir)
	if err != nil {
		return Resolved{}, err
	}
	// ResolveRevision uniformly resolves a tag (peeling an annotated tag to its commit) or a
	// short/full SHA to the commit hash — so the caller's version may be either.
	hash, err := repo.ResolveRevision(plumbing.Revision(version))
	if err != nil {
		return Resolved{}, fmt.Errorf("contentstore: resolve version %q in %s: %w", version, g.safeURL(), err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return Resolved{}, fmt.Errorf("contentstore: worktree: %w", err)
	}
	// Force a clean detached checkout at the resolved commit (discarding any prior version's tree).
	if err := wt.Checkout(&git.CheckoutOptions{Hash: *hash, Force: true}); err != nil {
		return Resolved{}, fmt.Errorf("contentstore: checkout %s: %w", hash.String(), err)
	}
	// Root the FS at the checkout via os.Root so a committed symlink cannot be followed OUT of the
	// tree at read time (the importer YAML-reads packs/**; os.DirFS would follow an escaping symlink).
	root, err := os.OpenRoot(repoDir)
	if err != nil {
		return Resolved{}, fmt.Errorf("contentstore: open checkout root: %w", err)
	}
	return Resolved{FS: root.FS(), SHA: hash.String(), root: root}, nil
}

// openOrClone returns the working clone at dir, creating it (a no-checkout clone that carries all
// tags) on first use and otherwise opening + fetching it so newly-published tags are visible. A
// cache dir that exists but is NOT a valid repo (a clone killed mid-flight — SIGTERM, disk full)
// is removed and re-cloned, so the importer self-heals rather than staying wedged forever.
func (g *GitPublishedSource) openOrClone(ctx context.Context, dir string) (*git.Repository, error) {
	repo, err := git.PlainOpen(dir)
	switch {
	case err == nil:
		ferr := repo.FetchContext(ctx, &git.FetchOptions{Auth: g.auth, Tags: git.AllTags, Force: true})
		if ferr != nil && !errors.Is(ferr, git.NoErrAlreadyUpToDate) {
			return nil, fmt.Errorf("contentstore: fetch %s: %w", g.safeURL(), ferr)
		}
		return repo, nil
	case errors.Is(err, git.ErrRepositoryNotExists):
		// nothing cached yet — clone below.
	default:
		// A partial/corrupt cache dir: discard it so the clone below starts clean (else
		// PlainClone would fail with ErrRepositoryAlreadyExists and never recover).
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return nil, fmt.Errorf("contentstore: remove corrupt cache %s: %w", dir, rmErr)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil { // 0700: a private-repo tree + .git/config live here
		return nil, fmt.Errorf("contentstore: cache dir: %w", err)
	}
	repo, err = git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL: g.url, Auth: g.auth, NoCheckout: true, Tags: git.AllTags,
	})
	if err != nil {
		return nil, fmt.Errorf("contentstore: clone %s: %w", g.safeURL(), err)
	}
	return repo, nil
}

// urlKey is a filesystem-safe, collision-resistant directory name for a remote URL, so different
// content remotes never share a working clone.
func urlKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

// safeURL returns g.url with any embedded userinfo (a credential) redacted, for use in error
// messages/logs. An operator SHOULD pass a token via NewGit's token parameter (held in memory,
// never persisted), but if credentials are embedded in the URL instead, this keeps them out of
// error strings. An unparseable URL is fully redacted rather than echoed.
func (g *GitPublishedSource) safeURL() string {
	u, err := url.Parse(g.url)
	if err != nil {
		return "<redacted-url>"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	return u.String()
}
