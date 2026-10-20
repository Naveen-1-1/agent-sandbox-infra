package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// RestoreOptions says where a workspace's state comes from.
type RestoreOptions struct {
	// OriginURL is where Base is fetched from when BaseBundle is empty. It
	// becomes the "origin" remote with any credentials removed.
	OriginURL string
	// Base is the upstream commit the workspace started from.
	Base string
	// BaseBundle is a bundle from CreateBaseBundle. If empty, Base is fetched
	// from OriginURL.
	BaseBundle string
	// Depth makes the fetch from OriginURL shallow (0 fetches full history).
	Depth int
	// Snapshot is a bundle from Create. If empty, Restore checks out Base on
	// a new branch named Branch instead.
	Snapshot string
	// Branch is the branch created at Base for a fresh workspace.
	Branch string
}

// Restore rebuilds a workspace in dir, which must be empty or missing.
func Restore(ctx context.Context, dir string, o RestoreOptions) error {
	if o.Base == "" {
		return errors.New("snapshot: Base is required")
	}
	if err := ensureEmptyDir(dir); err != nil {
		return err
	}
	g := git{dir: dir}
	if _, err := g.run(ctx, "init", "-q"); err != nil {
		return err
	}
	if o.OriginURL != "" {
		if _, err := g.run(ctx, "remote", "add", "origin", StripCredentials(o.OriginURL)); err != nil {
			return err
		}
	}

	if err := fetchBase(ctx, g, o); err != nil {
		return err
	}
	if o.Snapshot == "" {
		return checkoutBase(ctx, g, o)
	}
	return applySnapshot(ctx, g, o.Snapshot)
}

func fetchBase(ctx context.Context, g git, o RestoreOptions) error {
	if o.BaseBundle != "" {
		_, err := g.run(ctx, "fetch", "-q", o.BaseBundle, baseRef)
		return err
	}
	if o.OriginURL == "" {
		return errors.New("snapshot: need BaseBundle or OriginURL to get the base commit")
	}
	args := []string{"fetch", "-q", "--no-tags"}
	if o.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(o.Depth))
	}
	// Fetch from the URL itself so credentials are used but never stored.
	_, err := g.run(ctx, append(args, o.OriginURL, o.Base)...)
	return err
}

func checkoutBase(ctx context.Context, g git, o RestoreOptions) error {
	branch := o.Branch
	if branch == "" {
		branch = "main"
	}
	ref := "refs/heads/" + branch
	steps := [][]string{
		{"update-ref", ref, o.Base},
		{"symbolic-ref", "HEAD", ref},
		{"read-tree", "--reset", "-u", "HEAD"},
	}
	for _, s := range steps {
		if _, err := g.run(ctx, s...); err != nil {
			return err
		}
	}
	return nil
}

// applySnapshot fetches the snapshot's branches and tags, puts HEAD back,
// writes the working tree, and then restores the staging area on top.
func applySnapshot(ctx context.Context, g git, bundle string) error {
	if _, err := g.run(ctx, "fetch", "-q", "--update-head-ok", bundle,
		"refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*", wipRef+":"+wipRef); err != nil {
		return err
	}
	msg, err := g.run(ctx, "log", "-1", "--format=%B", wipRef)
	if err != nil {
		return err
	}
	head, err := trailer(msg, trailerHead)
	if err != nil {
		return err
	}

	if head == "detached" {
		parent, err := g.run(ctx, "rev-parse", "-q", "--verify", wipRef+"^1")
		if err != nil {
			return fmt.Errorf("snapshot: detached snapshot has no HEAD commit: %w", err)
		}
		if _, err := g.run(ctx, "update-ref", "--no-deref", "HEAD", parent); err != nil {
			return err
		}
	} else if _, err := g.run(ctx, "symbolic-ref", "HEAD", head); err != nil {
		return err
	}

	// Working tree first, then the index on top without touching files.
	if _, err := g.run(ctx, "read-tree", "--reset", "-u", wipRef); err != nil {
		return err
	}
	index, err := lastParent(ctx, g, wipRef) // the index commit is always the last parent
	if err != nil {
		return err
	}
	if _, err := g.run(ctx, "read-tree", index); err != nil {
		return err
	}
	// Refresh stat info so `git status` doesn't rehash every file. Exit code
	// 1 only means some files differ from the index, which is expected.
	_, _ = g.run(ctx, "update-index", "-q", "--refresh")
	_, err = g.run(ctx, "update-ref", "-d", wipRef)
	return err
}

func lastParent(ctx context.Context, g git, rev string) (string, error) {
	out, err := g.run(ctx, "rev-list", "--parents", "-n", "1", rev)
	if err != nil {
		return "", err
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return "", fmt.Errorf("snapshot: %s has no parents", rev)
	}
	return f[len(f)-1], nil
}

func trailer(msg, key string) (string, error) {
	for _, line := range strings.Split(msg, "\n") {
		if v, ok := strings.CutPrefix(line, key); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("snapshot: commit message has no %q trailer", strings.TrimSpace(key))
}

func ensureEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("snapshot: restore target %s is not empty", dir)
	}
	return nil
}
