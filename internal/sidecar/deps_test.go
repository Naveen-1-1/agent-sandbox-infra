package sidecar

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
)

// fakeInstaller stands in for `npm ci` in the workload: it writes a small
// node_modules, or fails.
type fakeInstaller struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (f *fakeInstaller) Install(_ context.Context, command, dir string) (int, string, error) {
	f.mu.Lock()
	f.calls++
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return 1, "npm ERR! could not resolve left-pad\n", nil
	}
	if command != "npm ci --no-audit --no-fund" {
		return 127, "unexpected command " + command, nil
	}
	for name, body := range map[string]string{
		"node_modules/left-pad/index.js":     "module.exports = pad\n",
		"node_modules/left-pad/package.json": `{"name":"left-pad"}`,
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return 0, "", err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return 0, "", err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules/.bin"), 0o755); err != nil {
		return 0, "", err
	}
	if err := os.Symlink("../left-pad/index.js", filepath.Join(dir, "node_modules/.bin/left-pad")); err != nil {
		return 0, "", err
	}
	return 0, "added 1 package\n", nil
}

func (f *fakeInstaller) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fixture) depsWorkspace(t *testing.T, inst Installer, lockfile string) (*Deps, Config) {
	t.Helper()
	cfg := f.config(t, 1)
	cfg.Installer, cfg.Arch = inst, "arm64"
	mkdir(t, metaDir(cfg.Root))
	if lockfile != "" {
		if err := os.WriteFile(filepath.Join(cfg.Root, "package-lock.json"), []byte(lockfile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d := NewDeps(cfg, f.env.Client(t, f.ws, 1))
	t.Cleanup(d.Close)
	return d, cfg
}

func waitDeps(t *testing.T, d *Deps, image string) *sidecarv1.Deps {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := d.Start(&sidecarv1.StartDepsRequest{Epoch: 1, ImageId: image})
		if err != nil {
			t.Fatal(err)
		}
		if terminal(st.GetState()) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("dependencies still %s", st.GetState())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b.WriteString(rel)
		if de.Type()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			b.WriteString(" -> " + target)
		} else if !de.IsDir() {
			body, _ := os.ReadFile(p)
			b.WriteString(": " + string(body))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestDepsInstallUploadThenHitTheCache(t *testing.T) {
	f := newFixture(t)
	inst := &fakeInstaller{}
	const lock = `{"lockfileVersion":3,"packages":{"node_modules/left-pad":{}}}`

	a, cfgA := f.depsWorkspace(t, inst, lock)
	st := waitDeps(t, a, "node@sha256:1")
	if st.GetState() != sidecarv1.DepsState_DEPS_STATE_READY || st.GetSource() != sidecarv1.DepsSource_DEPS_SOURCE_INSTALL || st.GetProfile() != "npm" {
		t.Fatalf("first workspace: %v", st)
	}
	up := a.Upload(ctx(t))
	if !up.GetUploaded() || up.GetSizeBytes() == 0 || up.GetError() != "" {
		t.Fatalf("upload = %v", up)
	}
	if again := a.Upload(ctx(t)); again.GetUploaded() || again.GetSkipped() != "already uploaded" {
		t.Errorf("second upload = %v", again)
	}

	// A second workspace with the same lockfile and image restores it
	// without installing.
	b, cfgB := f.depsWorkspace(t, inst, lock)
	st = waitDeps(t, b, "node@sha256:1")
	if st.GetSource() != sidecarv1.DepsSource_DEPS_SOURCE_CACHE || st.GetBytes() == 0 || inst.count() != 1 {
		t.Fatalf("second workspace: %v after %d installs; want a cache hit", st, inst.count())
	}
	if want, got := readTree(t, filepath.Join(cfgA.Root, "node_modules")), readTree(t, filepath.Join(cfgB.Root, "node_modules")); want != got {
		t.Errorf("restored node_modules differs:\nwant\n%s\ngot\n%s", want, got)
	}
	if _, err := os.Stat(filepath.Join(metaDir(cfgB.Root), "deps-staging")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the staging directory was left behind")
	}
	if up := b.Upload(ctx(t)); up.GetUploaded() || up.GetSkipped() != "restored from the cache" {
		t.Errorf("upload after a hit = %v", up)
	}

	// A different image is a different key: install again.
	c, _ := f.depsWorkspace(t, inst, lock)
	if st := waitDeps(t, c, "node@sha256:2"); st.GetSource() != sidecarv1.DepsSource_DEPS_SOURCE_INSTALL || inst.count() != 2 {
		t.Fatalf("other image: %v", st)
	}
}

func TestDepsWithoutALockfileAreReadyAtOnce(t *testing.T) {
	f := newFixture(t)
	inst := &fakeInstaller{}
	d, _ := f.depsWorkspace(t, inst, "")
	st, err := d.Start(&sidecarv1.StartDepsRequest{Epoch: 1, ImageId: "img"})
	if err != nil || st.GetState() != sidecarv1.DepsState_DEPS_STATE_READY || st.GetSource() != sidecarv1.DepsSource_DEPS_SOURCE_NONE {
		t.Fatalf("got %v, %v", st, err)
	}
	if d.Upload(ctx(t)) != nil || inst.count() != 0 {
		t.Error("nothing to install or upload, but something happened")
	}
}

func TestDepsFailureIsReportedAndNotUploaded(t *testing.T) {
	f := newFixture(t)
	d, _ := f.depsWorkspace(t, &fakeInstaller{fail: true}, "{}")
	st := waitDeps(t, d, "img")
	if st.GetState() != sidecarv1.DepsState_DEPS_STATE_FAILED || !strings.Contains(st.GetMessage(), "could not resolve left-pad") {
		t.Fatalf("got %v", st)
	}
	if up := d.Upload(ctx(t)); up.GetUploaded() || up.GetSkipped() == "" {
		t.Errorf("upload after a failure = %v", up)
	}
}

func TestDepsAlreadyPresentAreKept(t *testing.T) {
	f := newFixture(t)
	inst := &fakeInstaller{}
	d, cfg := f.depsWorkspace(t, inst, "{}")
	mkdir(t, filepath.Join(cfg.Root, "node_modules", "committed"))
	if st := waitDeps(t, d, "img"); st.GetSource() != sidecarv1.DepsSource_DEPS_SOURCE_PRESENT || inst.count() != 0 {
		t.Fatalf("got %v with %d installs", st, inst.count())
	}
}

func TestDepsUploadSkipsAChangedLockfile(t *testing.T) {
	f := newFixture(t)
	d, cfg := f.depsWorkspace(t, &fakeInstaller{}, `{"v":1}`)
	waitDeps(t, d, "img")
	// The agent edits the lockfile without reinstalling: node_modules no
	// longer matches either key.
	if err := os.WriteFile(filepath.Join(cfg.Root, "package-lock.json"), []byte(`{"v":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if up := d.Upload(ctx(t)); up.GetUploaded() || up.GetSkipped() != "the lockfile changed since the install" {
		t.Fatalf("upload = %v", up)
	}
}

func TestDepsResumeAnInterruptedInstall(t *testing.T) {
	f := newFixture(t)
	inst := &fakeInstaller{}
	d, cfg := f.depsWorkspace(t, inst, "{}")
	// A sidecar that died mid-install left this behind.
	mkdir(t, filepath.Join(cfg.Root, "node_modules", "half"))
	b, _ := protojson.Marshal(&sidecarv1.Deps{State: sidecarv1.DepsState_DEPS_STATE_INSTALLING, Profile: "npm"})
	if err := writeFileAtomic(d.statePath(), b); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(cfg.Root, "node_modules")); err != nil { // npm ci would clear it
		t.Fatal(err)
	}
	if st := waitDeps(t, d, "img"); st.GetSource() != sidecarv1.DepsSource_DEPS_SOURCE_INSTALL || inst.count() != 1 {
		t.Fatalf("got %v with %d installs; want the install redone", st, inst.count())
	}
}

func TestDepsStartIsIdempotentAndChecksTheEpoch(t *testing.T) {
	f := newFixture(t)
	d, _ := f.depsWorkspace(t, &fakeInstaller{}, "{}")
	if _, err := d.Start(&sidecarv1.StartDepsRequest{Epoch: 2, ImageId: "img"}); err == nil {
		t.Error("another epoch's request was accepted")
	}
	if _, err := d.Start(&sidecarv1.StartDepsRequest{Epoch: 1}); err == nil {
		t.Error("a request without an image was accepted")
	}
	first := waitDeps(t, d, "img")
	again, err := d.Start(&sidecarv1.StartDepsRequest{Epoch: 1, ImageId: "img"})
	if err != nil || again.GetKey() != first.GetKey() || !again.GetFinishedAt().AsTime().Equal(first.GetFinishedAt().AsTime()) {
		t.Fatalf("second start = %v, %v; want the same finished restore", again, err)
	}
}
