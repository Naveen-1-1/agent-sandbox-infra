//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/demo"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

// TestDemo runs `make demo`'s session: the hinted wake must be hidden behind
// the thinking time, the unhinted one visible, and the git state intact.
func TestDemo(t *testing.T) {
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	image := os.Getenv("SBX_DEMO_IMAGE")
	if image == "" {
		image = "mcr.microsoft.com/devcontainers/javascript-node:22"
	}
	var out strings.Builder
	res, err := demo.Run(framework.Ctx(t, 10*time.Minute), gw, demo.Options{
		RepoURL: p.FixturesURL + "/hello-deps.git", Image: image, Thinking: 12 * time.Second, Out: &out,
	})
	t.Log("\n" + out.String())
	if err != nil {
		t.Fatal(err)
	}
	if res.Hinted.Waited > time.Second {
		t.Errorf("the hinted wake was visible: the first command waited %s", res.Hinted.Waited)
	}
	if res.Hinted.DepsWaited > time.Second {
		t.Errorf("after the hint, npm test still waited %s for node_modules", res.Hinted.DepsWaited)
	}
	if res.Unhinted.Waited < time.Second {
		t.Errorf("the unhinted command waited only %s; the second wake didn't happen", res.Unhinted.Waited)
	}
	if res.Unhinted.DepsSource != "Cache" {
		t.Errorf("node_modules on the last wake came from %q, want the cache", res.Unhinted.DepsSource)
	}
	if !res.Hinted.StateIntact || !res.Unhinted.StateIntact {
		t.Error("git state changed across a sleep")
	}
}
