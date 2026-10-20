// Package snapshot saves a workspace's git state as a bundle containing only
// the agent's changes, and restores it into a fresh directory.
//
// A snapshot is two synthetic commits, laid out like `git stash`:
//
//	wip   tree = working tree (tracked + untracked, minus ignored and denied paths)
//	      parents = HEAD, index
//	index tree = the staging area, parent = HEAD
//
// plus every local branch and tag, bundled with `--not <base>` so the bundle
// holds only objects the upstream base commit doesn't already have.
//
// Snapshotting never writes to the user's repository: new objects go to a
// throwaway shadow repository that borrows the user's objects through git's
// alternates mechanism.
package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// git runs git commands with a controlled environment: no system or global
// config (so the user's ~/.gitconfig can't change our plumbing), and any
// directory ownership accepted (the sidecar may run as a different user).
type git struct {
	dir string   // working directory
	env []string // extra environment, e.g. GIT_DIR, GIT_INDEX_FILE
}

func (g git) with(env ...string) git {
	return git{dir: g.dir, env: append(append([]string{}, g.env...), env...)}
}

func (g git) run(ctx context.Context, args ...string) (string, error) {
	return g.runInput(ctx, nil, args...)
}

func (g git) runInput(ctx context.Context, stdin []byte, args ...string) (string, error) {
	full := append([]string{"-c", "safe.directory=*", "-c", "core.quotepath=off"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = g.dir
	cmd.Env = append(baseEnv(), g.env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// baseEnv keeps PATH and a few locale variables, and isolates git from
// system and global configuration.
func baseEnv() []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
