package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Commit message trailers that describe how to restore a snapshot.
const (
	trailerHead  = "sbx-head: "  // symbolic HEAD ref, or "detached"
	trailerIndex = "sbx-index: " // "staged", or "head" when the index had conflicts
	// One per local branch and tag: "<object> <ref>". The bundle leaves out
	// refs that point into the base's history (a branch made without new
	// commits, main itself), so the snapshot commit records them all.
	trailerRef = "sbx-ref: "
	wipRef     = "refs/sbx/wip"
	baseRef    = "refs/sbx/base"
)

// MetaDir is the workspace directory where sbx keeps its own state. It is
// never included in a snapshot.
const MetaDir = ".sbx"

// Options controls what a snapshot contains.
type Options struct {
	// Base is the upstream commit the workspace started from. Objects it can
	// reach are left out of the bundle.
	Base string
	// Exclude lists patterns that are never saved from the working tree or
	// staging area, such as dependency directories. A pattern without a slash
	// matches a file or directory with that name anywhere ("node_modules");
	// a pattern with a slash is a glob from the repository root.
	Exclude []string
	// Secrets keeps credentials out of the snapshot. Nil means
	// DefaultSecretPolicy.
	Secrets *SecretPolicy
	// Limits caps file, content and bundle sizes.
	Limits Limits
	// Time stamps the synthetic commits, so snapshotting the same state twice
	// yields the same commit IDs.
	Time time.Time
	// WorkDir is an empty scratch directory for the shadow repository and the
	// bundle. The caller removes it.
	WorkDir string
}

// Result describes a snapshot bundle written to disk.
type Result struct {
	WIP        string   // commit holding the working tree
	Index      string   // commit holding the staging area
	Head       string   // symbolic HEAD (e.g. refs/heads/main), "" if detached
	HeadCommit string   // commit HEAD pointed at, "" for an unborn branch
	BundlePath string   // the bundle file inside WorkDir
	Size       int64    // bundle size in bytes
	SHA256     string   // hex SHA-256 of the bundle file
	Refs       []string // refs recorded in the bundle
	NewBytes   int64    // total size of files the base doesn't have
}

// Create snapshots the repository at repo into a bundle. It reads the user's
// repository but never writes to it. A *PolicyError means a secret or size
// rule refused the snapshot.
func Create(ctx context.Context, repo string, opts Options) (*Result, error) {
	if opts.WorkDir == "" {
		return nil, errors.New("snapshot: WorkDir is required")
	}
	if opts.Time.IsZero() {
		return nil, errors.New("snapshot: Time is required for deterministic commits")
	}

	user := git{dir: repo}
	gitDir, err := user.run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}

	shadow, err := newShadow(ctx, opts.WorkDir, gitDir)
	if err != nil {
		return nil, err
	}
	// Plumbing on the user's worktree, with new objects going to the shadow.
	plumb := user.with(
		"GIT_DIR="+gitDir,
		"GIT_WORK_TREE="+repo,
		"GIT_OBJECT_DIRECTORY="+filepath.Join(shadow, "objects"),
	)

	secrets := DefaultSecretPolicy()
	if opts.Secrets != nil {
		secrets = *opts.Secrets
	}
	drop := func(p string) bool { return matchAny(opts.Exclude, p) || secrets.isSecretPath(p) }

	res := &Result{}
	res.Head, res.HeadCommit = readHead(ctx, plumb)

	indexTree, indexMode, err := stagedTree(ctx, plumb, gitDir, opts.WorkDir, res.HeadCommit, drop)
	if err != nil {
		return nil, err
	}
	wipTree, err := worktreeTree(ctx, plumb, gitDir, opts.WorkDir, opts.Exclude, drop)
	if err != nil {
		return nil, err
	}

	refs, err := listRefs(ctx, user.with("GIT_DIR="+gitDir))
	if err != nil {
		return nil, err
	}
	commit := plumb.with(commitEnv(opts.Time)...)
	headTrailer := res.Head
	if headTrailer == "" {
		headTrailer = "detached"
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "sbx snapshot\n\n%s%s\n%s%s\n", trailerHead, headTrailer, trailerIndex, indexMode)
	for _, r := range refs {
		fmt.Fprintf(&msg, "%s%s %s\n", trailerRef, r.sha, r.name)
	}

	parents := []string{}
	if res.HeadCommit != "" {
		parents = append(parents, "-p", res.HeadCommit)
	}
	res.Index, err = commit.run(ctx, append(append([]string{"commit-tree", indexTree}, parents...), "-m", "sbx index")...)
	if err != nil {
		return nil, err
	}
	res.WIP, err = commit.run(ctx, append(append([]string{"commit-tree", wipTree}, parents...), "-p", res.Index, "-m", msg.String())...)
	if err != nil {
		return nil, err
	}

	sg := git{dir: shadow, env: []string{"GIT_DIR=" + shadow}}
	if res.Refs, err = copyRefs(ctx, sg, refs, res.WIP); err != nil {
		return nil, err
	}

	// Check exactly what the bundle will carry before writing it.
	objs, err := listNewObjects(ctx, sg, opts.Base)
	if err != nil {
		return nil, err
	}
	if res.NewBytes, err = checkPolicy(ctx, sg, objs, secrets, opts.Limits); err != nil {
		return nil, err
	}

	res.BundlePath = filepath.Join(opts.WorkDir, "snapshot.bundle")
	args := []string{"bundle", "create", "-q", res.BundlePath, "--branches", "--tags", wipRef}
	if opts.Base != "" {
		args = append(args, "--not", opts.Base)
	}
	if _, err := sg.run(ctx, args...); err != nil {
		return nil, err
	}
	if res.Size, res.SHA256, err = hashFile(res.BundlePath); err != nil {
		return nil, err
	}
	if max := opts.Limits.MaxBundleBytes; max > 0 && res.Size > max {
		return nil, &PolicyError{Code: CodeTooLarge, Message: fmt.Sprintf("bundle is %d bytes, over the %d-byte limit", res.Size, max)}
	}
	return res, nil
}

// ErrShallowBase means the repository is a shallow clone. Git can write a
// bundle from it, but that bundle can't be fetched into an empty repository
// (the boundary commit's parents are missing), so shallow workspaces fetch
// their base from origin instead.
var ErrShallowBase = errors.New("snapshot: cannot bundle the base of a shallow clone")

// CreateBaseBundle writes a bundle holding everything reachable from base, so
// later restores don't depend on the upstream remote. Like Create, it never
// writes to the user's repository.
func CreateBaseBundle(ctx context.Context, repo, base, workDir string) (path string, size int64, sum string, err error) {
	user := git{dir: repo}
	gitDir, err := user.run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", 0, "", err
	}
	if _, err := os.Stat(filepath.Join(gitDir, "shallow")); err == nil {
		return "", 0, "", ErrShallowBase
	}
	shadow, err := newShadow(ctx, workDir, gitDir)
	if err != nil {
		return "", 0, "", err
	}
	sg := git{dir: shadow, env: []string{"GIT_DIR=" + shadow}}
	if _, err := sg.run(ctx, "update-ref", baseRef, base); err != nil {
		return "", 0, "", err
	}
	path = filepath.Join(workDir, "base.bundle")
	if _, err := sg.run(ctx, "bundle", "create", "-q", path, baseRef); err != nil {
		return "", 0, "", err
	}
	size, sum, err = hashFile(path)
	return path, size, sum, err
}

// newShadow creates a bare repository whose object store falls back to the
// user's objects. If the user's repository is shallow, the shadow is too, so
// history walks stop at the same boundary.
func newShadow(ctx context.Context, workDir, gitDir string) (string, error) {
	shadow := filepath.Join(workDir, "shadow.git")
	if _, err := (git{dir: workDir}).run(ctx, "init", "-q", "--bare", shadow); err != nil {
		return "", err
	}
	alt := filepath.Join(shadow, "objects", "info", "alternates")
	if err := os.WriteFile(alt, []byte(filepath.Join(gitDir, "objects")+"\n"), 0o644); err != nil {
		return "", err
	}
	if b, err := os.ReadFile(filepath.Join(gitDir, "shallow")); err == nil {
		//nolint:gosec // shadow is the directory we just created under WorkDir
		if err := os.WriteFile(filepath.Join(shadow, "shallow"), b, 0o644); err != nil {
			return "", err
		}
	}
	return shadow, nil
}

// readHead returns the symbolic HEAD ("" when detached) and the commit it
// points at ("" for an unborn branch).
func readHead(ctx context.Context, g git) (ref, commit string) {
	ref, _ = g.run(ctx, "symbolic-ref", "-q", "HEAD")
	commit, _ = g.run(ctx, "rev-parse", "-q", "--verify", "HEAD^{commit}")
	return ref, commit
}

// stagedTree writes the user's staging area as a tree, minus dropped paths.
// It works on a copy of the index because `git write-tree` may rewrite the
// index it reads. When the index has unresolved conflicts it falls back to
// HEAD's tree.
func stagedTree(ctx context.Context, g git, gitDir, workDir, head string, drop func(string) bool) (tree, mode string, err error) {
	idx := filepath.Join(workDir, "index.staged")
	if err := copyIndex(filepath.Join(gitDir, "index"), idx); err != nil {
		return "", "", err
	}
	if err := removeFromIndex(ctx, g.with("GIT_INDEX_FILE="+idx), drop); err != nil {
		return "", "", err
	}
	tree, err = g.with("GIT_INDEX_FILE="+idx).run(ctx, "write-tree")
	if err == nil {
		return tree, "staged", nil
	}
	if head == "" {
		tree, err = g.with("GIT_INDEX_FILE="+filepath.Join(workDir, "index.empty")).run(ctx, "write-tree")
		return tree, "head", err
	}
	tree, err = g.run(ctx, "rev-parse", head+"^{tree}")
	return tree, "head", err
}

// worktreeTree writes the working tree as a tree: every tracked or untracked
// file that isn't gitignored, minus dropped paths (even tracked ones).
func worktreeTree(ctx context.Context, g git, gitDir, workDir string, exclude []string, drop func(string) bool) (string, error) {
	idx := filepath.Join(workDir, "index.wip")
	if err := copyIndex(filepath.Join(gitDir, "index"), idx); err != nil {
		return "", err
	}
	g = g.with("GIT_INDEX_FILE=" + idx)

	// The pathspec excludes keep `git add` from even hashing large dependency
	// directories; removeFromIndex then applies the full rules.
	add := []string{"add", "-A", "--", "."}
	for _, p := range pathspecs(append([]string{MetaDir}, exclude...)) {
		add = append(add, ":(exclude,glob)"+p)
	}
	if _, err := g.run(ctx, add...); err != nil {
		return "", err
	}
	metaOrDrop := func(p string) bool { return matchPath(MetaDir, p) || drop(p) }
	if err := removeFromIndex(ctx, g, metaOrDrop); err != nil {
		return "", err
	}
	return g.run(ctx, "write-tree")
}

// removeFromIndex deletes every entry for which drop returns true from the
// index named by GIT_INDEX_FILE.
func removeFromIndex(ctx context.Context, g git, drop func(string) bool) error {
	out, err := g.raw(ctx, nil, "ls-files", "-z", "--cached")
	if err != nil {
		return err
	}
	var rm strings.Builder
	for _, p := range strings.Split(out, "\x00") {
		if p != "" && drop(p) {
			rm.WriteString(p + "\x00")
		}
	}
	if rm.Len() == 0 {
		return nil
	}
	_, err = g.runInput(ctx, []byte(rm.String()), "update-index", "-z", "--force-remove", "--stdin")
	return err
}

// pathspecs turns exclude patterns into glob pathspecs. A pattern without a
// slash matches that name anywhere, as a file or as a directory.
func pathspecs(patterns []string) []string {
	var out []string
	for _, p := range patterns {
		p = strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/")
		if p == "" {
			continue
		}
		if strings.Contains(p, "/") {
			out = append(out, p, p+"/**")
			continue
		}
		out = append(out, "**/"+p, "**/"+p+"/**")
	}
	return out
}

type ref struct{ sha, name string }

// listRefs reads the user's local branches and tags, sorted by name.
func listRefs(ctx context.Context, user git) ([]ref, error) {
	out, err := user.run(ctx, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, err
	}
	var refs []ref
	for _, line := range strings.Split(out, "\n") {
		if sha, name, ok := strings.Cut(line, " "); ok {
			refs = append(refs, ref{sha: sha, name: name})
		}
	}
	return refs, nil
}

// copyRefs mirrors the user's refs into the shadow and points refs/sbx/wip
// at the snapshot commit.
func copyRefs(ctx context.Context, shadow git, refs []ref, wip string) ([]string, error) {
	var stdin strings.Builder
	names := []string{}
	for _, r := range refs {
		fmt.Fprintf(&stdin, "update %s %s\n", r.name, r.sha)
		names = append(names, r.name)
	}
	fmt.Fprintf(&stdin, "update %s %s\n", wipRef, wip)
	names = append(names, wipRef)
	if _, err := shadow.runInput(ctx, []byte(stdin.String()), "update-ref", "--stdin"); err != nil {
		return nil, err
	}
	return names, nil
}

// copyIndex copies the index file; a missing index means an empty one.
func copyIndex(src, dst string) error {
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func commitEnv(t time.Time) []string {
	date := strconv.FormatInt(t.Unix(), 10) + " +0000"
	return []string{
		"GIT_AUTHOR_NAME=sbx", "GIT_AUTHOR_EMAIL=sbx@localhost", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=sbx", "GIT_COMMITTER_EMAIL=sbx@localhost", "GIT_COMMITTER_DATE=" + date,
	}
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}
