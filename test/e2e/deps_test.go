//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

func nodeImage() string {
	if img := os.Getenv("SBX_DEMO_IMAGE"); img != "" {
		return img
	}
	return "mcr.microsoft.com/devcontainers/javascript-node:22"
}

// depsWorkspace creates a workspace from the hello-deps fixture that only
// sleeps when told to, and waits for its dependencies to settle.
func depsWorkspace(t *testing.T, gw *sbxapi.Client, p framework.Platform, services []sbxapi.Service) (ns, name string, own *sbxapi.Client) {
	t.Helper()
	resp, err := gw.Create(framework.Ctx(t, time.Minute), sbxapi.CreateRequest{
		RepoURL: p.FixturesURL + "/hello-deps.git", Image: nodeImage(),
		Policy: &sbxapi.PolicyRef{Name: "never"}, Services: services,
	})
	if err != nil {
		t.Fatal(err)
	}
	ns, name = resp.Workspace.Namespace, resp.Workspace.Name
	t.Cleanup(func() { _ = gw.Delete(context.Background(), ns, name) })
	own = &sbxapi.Client{BaseURL: gw.BaseURL, Token: resp.Token, HTTP: gw.HTTP}
	waitPhase(t, own, ns, name, "Running", 5*time.Minute)
	waitDepsSettled(t, own, ns, name)
	return ns, name, own
}

func waitDepsSettled(t *testing.T, c *sbxapi.Client, ns, name string) *sbxapi.Workspace {
	t.Helper()
	var w *sbxapi.Workspace
	framework.Eventually(t, 3*time.Minute, 250*time.Millisecond, "dependencies settled", func() (bool, error) {
		var err error
		w, err = c.Get(context.Background(), ns, name)
		return err == nil && w.Phase == "Running" && w.Deps != nil && !w.Deps.Pending, err
	})
	if !w.Deps.Ready {
		t.Fatalf("dependencies failed: %s", w.Deps.Message)
	}
	return w
}

func run(t *testing.T, c *sbxapi.Client, ns, name, script string) *sbxapi.ExecResponse {
	t.Helper()
	out, err := c.Exec(framework.Ctx(t, 5*time.Minute), ns, name, sbxapi.ExecRequest{Command: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("%s: exit %d: %s%s", script, out.ExitCode, out.Stdout, out.Stderr)
	}
	return out
}

// TestDependencyCacheAndExecGating: after a wake that has to install,
// `npm test` waits for the install while `ls` doesn't; the next sleep
// caches the install, and the wake after restores it from the cache.
func TestDependencyCacheAndExecGating(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := depsWorkspace(t, gw, p, nil)
	if out := run(t, own, ns, name, "npm test --silent"); !strings.Contains(out.Stdout, "ok: hello, sbx") {
		t.Fatalf("npm test: %q", out.Stdout)
	}

	// A lockfile no earlier run has seen, so the next wake misses the
	// shared cache and installs (the fixture's install takes 5s).
	run(t, own, ns, name, fmt.Sprintf(`sed -i 's/"name": "hello-deps"/"name": "hello-deps-%d"/' package.json package-lock.json`, time.Now().UnixNano()))
	suspendAndWait(t, own, ns, name)

	var wg sync.WaitGroup
	var ls, test *sbxapi.ExecResponse
	wg.Go(func() { ls = run(t, own, ns, name, "ls package.json") })
	wg.Go(func() { test = run(t, own, ns, name, "npm test --silent") })
	wg.Wait()
	t.Logf("ls waited %dms for the wake, %dms for deps; npm test waited %dms, %dms",
		ls.WaitedMillis, ls.DepsWaitedMillis, test.WaitedMillis, test.DepsWaitedMillis)
	if ls.DepsWaitedMillis != 0 {
		t.Errorf("ls waited %dms for dependencies it doesn't need", ls.DepsWaitedMillis)
	}
	if test.DepsWaitedMillis == 0 || !strings.Contains(test.Stdout, "ok: hello, sbx") {
		t.Errorf("npm test didn't wait for the install: %+v", test)
	}
	if w, _ := own.Get(framework.Ctx(t, time.Minute), ns, name); w.Deps.Source != "Install" {
		t.Errorf("source after a new lockfile = %s, want Install", w.Deps.Source)
	}

	suspendAndWait(t, own, ns, name) // uploads the install
	test = run(t, own, ns, name, "npm test --silent")
	w, err := own.Get(framework.Ctx(t, time.Minute), ns, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after the second wake: source %s, npm test waited %dms for deps", w.Deps.Source, test.DepsWaitedMillis)
	if w.Deps.Source != "Cache" {
		t.Errorf("source on the second wake = %s, want Cache", w.Deps.Source)
	}
}

// TestServicesRestartAfterAWake declares a dev server: it runs once the
// dependencies are in, and again in the new pod after a sleep.
func TestServicesRestartAfterAWake(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	ns, name, own := depsWorkspace(t, gw, p, []sbxapi.Service{{Name: "web", Command: []string{"npm", "run", "dev"}}})

	fetch := func() {
		t.Helper()
		framework.Eventually(t, time.Minute, 500*time.Millisecond, "the dev server answers", func() (bool, error) {
			out, err := own.Exec(context.Background(), ns, name, sbxapi.ExecRequest{Command: []string{"node", "-e",
				`fetch("http://127.0.0.1:3000").then(r => r.text()).then(t => process.stdout.write(t))`}})
			return err == nil && out.Stdout == "hello, visitor\n", err
		})
	}
	fetch()
	before, err := own.Services(framework.Ctx(t, time.Minute), ns, name)
	if err != nil || len(before) != 1 || !before[0].Running {
		t.Fatalf("services = %+v, %v", before, err)
	}

	suspendAndWait(t, own, ns, name)
	fetch() // wakes it; the server comes back in the new pod
	after, err := own.Services(framework.Ctx(t, time.Minute), ns, name)
	if err != nil || len(after) != 1 || !after[0].Running || after[0].Pid == 0 {
		t.Fatalf("services after the wake = %+v, %v", after, err)
	}
}
