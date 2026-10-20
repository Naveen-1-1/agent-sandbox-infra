// Package gitfixture builds throwaway git repositories for tests.
package gitfixture

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// env isolates test git commands from the developer's configuration and
// gives them a fixed identity.
var env = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
	"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	"GIT_TERMINAL_PROMPT=0",
	"LC_ALL=C",
}

// Git runs git in dir and returns trimmed stdout, failing the test on error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := TryGit(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// TryGit runs git in dir and returns trimmed stdout and any error.
func TryGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", &gitError{err: err, stderr: stderr.String()}
	}
	return strings.TrimSpace(stdout.String()), nil
}

type gitError struct {
	err    error
	stderr string
}

func (e *gitError) Error() string { return e.err.Error() + ": " + strings.TrimSpace(e.stderr) }
func (e *gitError) Unwrap() error { return e.err }

// Write creates or replaces a file (and its parent directories).
func Write(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// NewRepo creates a repository with the given files committed on main and
// returns its path.
func NewRepo(t testing.TB, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	Git(t, filepath.Dir(dir), "init", "-q", dir)
	for name, content := range files {
		Write(t, filepath.Join(dir, name), content)
	}
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

// State captures everything a restore must reproduce: HEAD, local branches
// and tags, the staging area, status, and every working-tree file with its
// mode and content (or symlink target), keyed "file:<path>".
func State(t testing.TB, dir string) map[string]string {
	t.Helper()
	st := map[string]string{}
	if ref, err := TryGit(dir, "symbolic-ref", "-q", "HEAD"); err == nil {
		st["HEAD"] = ref
	} else {
		st["HEAD"] = Git(t, dir, "rev-parse", "HEAD")
	}
	st["refs"] = Git(t, dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	st["index"] = Git(t, dir, "ls-files", "-s")
	st["status"] = Git(t, dir, "status", "--porcelain=v1", "-uall")

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		var body string
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			body = "symlink -> " + target
		} else {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			body = info.Mode().Perm().String() + " " + string(b)
		}
		st["file:"+rel] = body
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}
