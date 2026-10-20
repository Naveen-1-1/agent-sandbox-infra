package snapshot_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
	gf "github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/gitfixture"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

var (
	freeze  = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	exclude = []string{"node_modules", ".venv"}
)

// workspace clones a small upstream repo and makes the kind of mess an agent
// leaves behind: unstaged, staged and partly-staged edits, new files, a
// deletion, an agent commit and tag, plus files that must not be saved.
func workspace(t *testing.T, cloneArgs ...string) (origin, ws, base string) {
	t.Helper()
	origin = gf.NewRepo(t, map[string]string{
		"a.txt":      "a1\n",
		"dir/b.txt":  "b1\n",
		"c.txt":      "c1\n",
		"script.sh":  "#!/bin/sh\necho hi\n",
		".gitignore": "*.log\nnode_modules/\n",
		"big.bin":    randomHex(t, 256<<10),
	})
	gf.Git(t, origin, "update-index", "--chmod=+x", "script.sh")
	gf.Git(t, origin, "commit", "-q", "-m", "make script executable")
	base = gf.Git(t, origin, "rev-parse", "HEAD")

	ws = filepath.Join(t.TempDir(), "ws")
	gf.Git(t, filepath.Dir(ws), append(append([]string{"clone", "-q"}, cloneArgs...), "file://"+origin, ws)...)

	// The agent commits and tags on main, and works on a side branch.
	gf.Write(t, filepath.Join(ws, "committed.txt"), "agent commit\n")
	gf.Git(t, ws, "add", "committed.txt")
	gf.Git(t, ws, "commit", "-q", "-m", "agent work")
	gf.Git(t, ws, "tag", "agent-v1")
	gf.Git(t, ws, "branch", "feature")

	gf.Write(t, filepath.Join(ws, "a.txt"), "a2 unstaged\n")
	gf.Write(t, filepath.Join(ws, "dir/b.txt"), "b2 staged\n")
	gf.Git(t, ws, "add", "dir/b.txt")
	gf.Write(t, filepath.Join(ws, "dir/b.txt"), "b3 partly staged\n")
	gf.Write(t, filepath.Join(ws, "staged-new.txt"), "new and staged\n")
	gf.Git(t, ws, "add", "staged-new.txt")
	gf.Write(t, filepath.Join(ws, "untracked ü.txt"), "untracked\n")
	if err := os.Remove(filepath.Join(ws, "c.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}

	// None of these may be saved.
	gf.Write(t, filepath.Join(ws, "debug.log"), "ignored\n")
	gf.Write(t, filepath.Join(ws, "node_modules/x/index.js"), "dep\n")
	gf.Write(t, filepath.Join(ws, ".env"), "API_KEY=secret\n")
	gf.Write(t, filepath.Join(ws, "keys/id_rsa"), "PRIVATE\n")
	gf.Write(t, filepath.Join(ws, ".venv/bin/python"), "venv\n")
	return origin, ws, base
}

func snapshotOf(t *testing.T, ws, base string) *snapshot.Result {
	t.Helper()
	res, err := snapshot.Create(context.Background(), ws, snapshot.Options{
		Base: base, Exclude: exclude, Time: freeze, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// unsaved are paths a snapshot deliberately leaves out.
var unsaved = []string{"debug.log", ".env", "node_modules/", "keys/", ".venv/"}

func isUnsaved(path string) bool {
	for _, p := range unsaved {
		if path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p)) {
			return true
		}
	}
	return false
}

// withoutUnsaved drops the files a snapshot leaves out, so a restored state
// can be compared with the original.
func withoutUnsaved(st map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range st {
		if path, ok := strings.CutPrefix(k, "file:"); ok && isUnsaved(path) {
			continue
		}
		out[k] = v
	}
	var status []string
	for _, line := range strings.Split(st["status"], "\n") {
		if len(line) > 3 && isUnsaved(strings.Trim(line[3:], `"`)) {
			continue
		}
		status = append(status, line)
	}
	out["status"] = strings.Join(status, "\n")
	return out
}

func TestRoundTripRestoresWorkspace(t *testing.T) {
	origin, ws, base := workspace(t)
	want := withoutUnsaved(gf.State(t, ws))
	res := snapshotOf(t, ws, base)

	dst := filepath.Join(t.TempDir(), "restored")
	err := snapshot.Restore(context.Background(), dst, snapshot.RestoreOptions{
		OriginURL: "file://" + origin, Base: base, Snapshot: res.BundlePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, gf.State(t, dst)); diff != "" {
		t.Errorf("restored workspace differs (-want +got):\n%s", diff)
	}
	if res.Head != "refs/heads/main" {
		t.Errorf("Head = %q, want refs/heads/main", res.Head)
	}
}

func TestSnapshotLeavesUserRepositoryUntouched(t *testing.T) {
	_, ws, base := workspace(t)
	before := dirDigest(t, filepath.Join(ws, ".git"))
	snapshotOf(t, ws, base)
	if after := dirDigest(t, filepath.Join(ws, ".git")); !cmp.Equal(before, after) {
		t.Errorf(".git changed during snapshot:\n%s", cmp.Diff(before, after))
	}
}

func TestSnapshotIsDeterministic(t *testing.T) {
	_, ws, base := workspace(t)
	first, second := snapshotOf(t, ws, base), snapshotOf(t, ws, base)
	if first.WIP != second.WIP || first.Index != second.Index {
		t.Errorf("same state gave different commits: %s/%s vs %s/%s", first.WIP, first.Index, second.WIP, second.Index)
	}
}

func TestBundleHoldsOnlyChangesSinceBase(t *testing.T) {
	_, ws, base := workspace(t)
	res := snapshotOf(t, ws, base)
	// big.bin (256 KiB of random hex) lives in the base and must not be resent.
	if res.Size > 32<<10 {
		t.Errorf("bundle is %d bytes; base content leaked into it", res.Size)
	}
	gf.Git(t, ws, "bundle", "verify", "-q", res.BundlePath) // prerequisites are satisfied by the base
	heads := gf.Git(t, ws, "bundle", "list-heads", res.BundlePath)
	for _, ref := range []string{"refs/heads/main", "refs/heads/feature", "refs/tags/agent-v1", "refs/sbx/wip"} {
		if !strings.Contains(heads, ref) {
			t.Errorf("bundle is missing %s:\n%s", ref, heads)
		}
	}
}

func TestDetachedHead(t *testing.T) {
	origin, ws, base := workspace(t)
	gf.Git(t, ws, "checkout", "-q", "--detach", "agent-v1")
	want := withoutUnsaved(gf.State(t, ws))
	res := snapshotOf(t, ws, base)

	dst := filepath.Join(t.TempDir(), "restored")
	if err := snapshot.Restore(context.Background(), dst, snapshot.RestoreOptions{
		OriginURL: "file://" + origin, Base: base, Snapshot: res.BundlePath,
	}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, gf.State(t, dst)); diff != "" {
		t.Errorf("restored workspace differs (-want +got):\n%s", diff)
	}
}

func TestShallowBase(t *testing.T) {
	origin, ws, base := workspace(t, "--depth", "1")
	want := withoutUnsaved(gf.State(t, ws))
	res := snapshotOf(t, ws, base)

	dst := filepath.Join(t.TempDir(), "restored")
	if err := snapshot.Restore(context.Background(), dst, snapshot.RestoreOptions{
		OriginURL: "file://" + origin, Base: base, Depth: 1, Snapshot: res.BundlePath,
	}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, gf.State(t, dst)); diff != "" {
		t.Errorf("restored workspace differs (-want +got):\n%s", diff)
	}
}

func TestRestoreFromBaseBundleWithoutOrigin(t *testing.T) {
	_, ws, base := workspace(t)
	want := withoutUnsaved(gf.State(t, ws))
	res := snapshotOf(t, ws, base)
	baseBundle, _, _, err := snapshot.CreateBaseBundle(context.Background(), ws, base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "restored")
	if err := snapshot.Restore(context.Background(), dst, snapshot.RestoreOptions{
		OriginURL: "file:///nonexistent/origin", Base: base, BaseBundle: baseBundle, Snapshot: res.BundlePath,
	}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, gf.State(t, dst)); diff != "" {
		t.Errorf("restored workspace differs (-want +got):\n%s", diff)
	}
}

func TestBaseBundleRefusesShallowClone(t *testing.T) {
	_, ws, base := workspace(t, "--depth", "1")
	_, _, _, err := snapshot.CreateBaseBundle(context.Background(), ws, base, t.TempDir())
	if !errors.Is(err, snapshot.ErrShallowBase) {
		t.Fatalf("CreateBaseBundle on a shallow clone: err = %v, want ErrShallowBase", err)
	}
}

func TestFreshCheckoutOfBase(t *testing.T) {
	origin, _, base := workspace(t)
	dst := filepath.Join(t.TempDir(), "fresh")
	if err := snapshot.Restore(context.Background(), dst, snapshot.RestoreOptions{
		OriginURL: "file://" + origin, Base: base, Branch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if got := gf.Git(t, dst, "rev-parse", "HEAD"); got != base {
		t.Errorf("HEAD = %s, want base %s", got, base)
	}
	if st := gf.Git(t, dst, "status", "--porcelain"); st != "" {
		t.Errorf("fresh checkout is dirty:\n%s", st)
	}
}

func TestRestoreRefusesNonEmptyDir(t *testing.T) {
	dst := t.TempDir()
	gf.Write(t, filepath.Join(dst, "keep.txt"), "user data\n")
	err := snapshot.Restore(context.Background(), dst, snapshot.RestoreOptions{Base: "abc", OriginURL: "file:///x"})
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("Restore into non-empty dir: err = %v, want 'not empty'", err)
	}
}

// dirDigest maps every file under root to its size and content hash.
func dirDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
