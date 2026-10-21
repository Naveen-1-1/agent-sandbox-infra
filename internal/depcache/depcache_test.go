package depcache_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/depcache"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func write(t *testing.T, path, body string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestDetectPicksTheFirstProfileWithALockfile(t *testing.T) {
	for _, c := range []struct {
		files []string
		want  string
	}{
		{[]string{"package.json", "package-lock.json"}, "npm"},
		{[]string{"npm-shrinkwrap.json"}, "npm"},
		{[]string{"requirements.txt"}, "pip"},
		{[]string{"uv.lock", "requirements.txt", "package-lock.json"}, "uv"},
		{[]string{"package.json", "README.md"}, ""},
	} {
		root := t.TempDir()
		for _, f := range c.files {
			write(t, filepath.Join(root, f), f+" contents", 0o644)
		}
		p, sum, ok, err := depcache.Detect(root)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Name; got != c.want || ok != (c.want != "") || (ok && len(sum) != 64) {
			t.Errorf("%v: got %q (ok=%v, sum=%q), want %q", c.files, got, ok, sum, c.want)
		}
	}
}

func TestDetectRefusesALockfileThatIsASymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "package-lock.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := depcache.Detect(root); err == nil {
		t.Fatal("a symlinked lockfile was hashed")
	}
}

func TestKeyCoversEveryInput(t *testing.T) {
	base := depcache.Key("npm", "lock", "image@sha256:1", "arm64")
	if !depcache.ValidKey(base) {
		t.Fatalf("key %q isn't valid", base)
	}
	for _, other := range []string{
		depcache.Key("pip", "lock", "image@sha256:1", "arm64"),
		depcache.Key("npm", "lock2", "image@sha256:1", "arm64"),
		depcache.Key("npm", "lock", "image@sha256:2", "arm64"),
		depcache.Key("npm", "lock", "image@sha256:1", "amd64"),
	} {
		if other == base {
			t.Error("a different input gave the same key")
		}
	}
	if depcache.Key("npm", "lock", "image@sha256:1", "arm64") != base {
		t.Error("the key isn't deterministic")
	}
}

func TestNeedsDeps(t *testing.T) {
	npm, _ := depcache.Lookup("npm")
	pip, _ := depcache.Lookup("pip")
	for _, c := range []struct {
		p    depcache.Profile
		argv []string
		want bool
	}{
		{npm, []string{"npm", "test"}, true},
		{npm, []string{"/usr/local/bin/node", "index.js"}, true},
		{npm, []string{"sh", "-c", "cd app && npx jest --ci"}, true},
		{npm, []string{"./node_modules/.bin/tsc", "-p", "."}, true},
		{npm, []string{"env", "CI=1", "node", "x.js"}, true},
		{npm, []string{"ls", "-la"}, false},
		{npm, []string{"git", "status"}, false},
		{npm, []string{"cat", "package.json"}, false},
		{npm, []string{"sh", "-c", "grep -r TODO src | wc -l"}, false},
		{pip, []string{".venv/bin/python", "-m", "app"}, true},
		{pip, []string{"sh", "-c", "pytest -q"}, true},
		{pip, []string{"node", "x.js"}, false},
	} {
		if got := c.p.NeedsDeps(c.argv); got != c.want {
			t.Errorf("%s NeedsDeps(%q) = %v, want %v", c.p.Name, c.argv, got, c.want)
		}
	}
}

func TestPackExtractRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "node_modules")
	write(t, filepath.Join(src, "left-pad/index.js"), "module.exports = pad\n", 0o644)
	write(t, filepath.Join(src, "left-pad/cli.js"), "#!/usr/bin/env node\n", 0o755)
	write(t, filepath.Join(src, ".package-lock.json"), "{}\n", 0o644)
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../left-pad/cli.js", filepath.Join(src, ".bin/left-pad")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/python3", filepath.Join(src, "python")); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "left-pad/index.js"), old, old); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	packed, err := depcache.Pack(context.Background(), src, &buf)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	got, err := depcache.Extract(context.Background(), &buf, dst, depcache.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got != packed || got.Files != 3 {
		t.Fatalf("extracted %+v, packed %+v", got, packed)
	}
	if want, have := tree(t, src), tree(t, dst); want != have {
		t.Errorf("restored tree differs:\nwant:\n%s\ngot:\n%s", want, have)
	}
	if st, _ := os.Stat(filepath.Join(dst, "left-pad/index.js")); !st.ModTime().Equal(old) {
		t.Errorf("mtime %v, want %v", st.ModTime(), old)
	}
}

// tree lists every entry with its type, permissions and content or target.
func tree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, _ := os.Lstat(p)
		b.WriteString(rel + " " + info.Mode().String())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			b.WriteString(" -> " + target)
		case info.Mode().IsRegular():
			body, _ := os.ReadFile(p)
			b.WriteString(" " + string(body))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

type entry struct {
	hdr  tar.Header
	body string
}

func archive(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := e.hdr
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func file(name, body string) entry {
	return entry{hdr: tar.Header{Name: name, Typeflag: tar.TypeReg}, body: body}
}

func link(name, target string) entry {
	return entry{hdr: tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}}
}

func TestExtractRefusesHostileArchives(t *testing.T) {
	outside := t.TempDir()
	for _, c := range []struct {
		name    string
		entries []entry
	}{
		{"parent path", []entry{file("../evil", "x")}},
		{"absolute path", []entry{file("/tmp/evil", "x")}},
		{"climbing path", []entry{file("a/../../evil", "x")}},
		{"relative link out", []entry{link("out", "../../etc")}},
		{"write through an absolute link", []entry{link("dir", outside), file("dir/evil", "x")}},
		{"write through a relative link", []entry{link("up", "."), link("up/../../x", "y")}},
		{"hard link", []entry{file("a", "x"), {hdr: tar.Header{Name: "b", Typeflag: tar.TypeLink, Linkname: "a"}}}},
		{"device", []entry{{hdr: tar.Header{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}}}},
		{"fifo", []entry{{hdr: tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}}}},
		{"duplicate file", []entry{file("a", "1"), file("a", "2")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "dst")
			if _, err := depcache.Extract(context.Background(), archive(t, c.entries...), dst, depcache.Limits{}); err == nil {
				t.Fatal("extracted without an error")
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Fatalf("wrote outside the target: %v", entries)
			}
		})
	}
}

func TestExtractDropsSetuidAndEnforcesLimits(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst")
	suid := entry{hdr: tar.Header{Name: "bin/tool", Typeflag: tar.TypeReg, Mode: 0o4755}, body: "#!/bin/sh\n"}
	if _, err := depcache.Extract(context.Background(), archive(t, suid), dst, depcache.Limits{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dst, "bin/tool")); st.Mode()&fs.ModeSetuid != 0 || st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755 without setuid", st.Mode())
	}

	big := archive(t, file("a", strings.Repeat("x", 600)), file("b", strings.Repeat("y", 600)))
	_, err := depcache.Extract(context.Background(), big, filepath.Join(t.TempDir(), "dst"), depcache.Limits{MaxBytes: 1000})
	if !errors.Is(err, depcache.ErrUnsafe) {
		t.Fatalf("over the byte limit: %v, want ErrUnsafe", err)
	}
	many := archive(t, file("a", "1"), file("b", "2"), file("c", "3"))
	_, err = depcache.Extract(context.Background(), many, filepath.Join(t.TempDir(), "dst"), depcache.Limits{MaxEntries: 2})
	if !errors.Is(err, depcache.ErrUnsafe) {
		t.Fatalf("over the entry limit: %v, want ErrUnsafe", err)
	}
}

func TestExtractNeedsAFreshTarget(t *testing.T) {
	dst := t.TempDir() // exists
	if _, err := depcache.Extract(context.Background(), archive(t, file("a", "1")), dst, depcache.Limits{}); err == nil {
		t.Fatal("extracted into an existing directory")
	}
}
