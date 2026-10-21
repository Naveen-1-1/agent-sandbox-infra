//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/bench"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

// logWriter sends the bench's progress lines to the test log.
type logWriter struct{ t *testing.T }

func (w logWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// TestBenchMeasuresTheFixture runs the bench's own code for two short
// cycles on the fixture repository: one hinted, one not. Every measurement
// it relies on comes back, and the hint hides the wake.
func TestBenchMeasuresTheFixture(t *testing.T) {
	gw := framework.Gateway(t)
	c := framework.NewClients(t)
	p := framework.DeployedPlatform(t, c)
	framework.Eventually(t, 3*time.Minute, time.Second, "other tests' workspaces to go", func() (bool, error) {
		list, err := gw.List(context.Background())
		return err == nil && len(list) == 0, err
	})
	r := &bench.Runner{
		Gateway: gw, Cluster: &bench.Kube{Client: c.Kube, Namespace: framework.Namespace()}, Out: logWriter{t},
		Options: bench.Options{Env: "e2e", Cycles: 2, Thinking: 12 * time.Second},
	}
	res, err := r.Run(framework.Ctx(t, 15*time.Minute), &bench.Config{Repos: []bench.Repo{{
		Name: "hello-deps", URL: p.FixturesURL + "/hello-deps.git", Profile: "npm", Image: nodeImage(),
		Command: "npm test --silent", Edit: "README.md",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Platform.Kubernetes == "" || len(res.Platform.Nodes) == 0 {
		t.Errorf("platform = %+v", res.Platform)
	}
	repo := res.Repos[0]
	if repo.FirstRun.DepsSource != "Install" && repo.FirstRun.DepsSource != "Cache" {
		t.Errorf("first run dependencies from %q", repo.FirstRun.DepsSource)
	}
	if repo.DepArchiveBytes <= 0 || repo.SandboxUsdPerHour <= 0 {
		t.Errorf("archive %d bytes, sandbox $%v/h", repo.DepArchiveBytes, repo.SandboxUsdPerHour)
	}
	if len(repo.Cycles) != 2 || !repo.Cycles[0].Hinted || repo.Cycles[1].Hinted {
		t.Fatalf("cycles = %+v", repo.Cycles)
	}
	for _, cy := range repo.Cycles {
		if cy.DepsSource != "Cache" || cy.ServingSeconds <= 0 || cy.SuspendSeconds <= 0 || cy.BundleBytes <= 0 {
			t.Errorf("cycle %d = %+v", cy.N, cy)
		}
		if cy.S3.PutClass <= 0 || cy.S3.GetClass <= 0 {
			t.Errorf("cycle %d S3 requests = %+v", cy.N, cy.S3)
		}
		if cy.Sidecar.CPUSeconds <= 0 || cy.Sidecar.PeakRSSBytes <= 0 || cy.Agent.PeakRSSBytes <= 0 {
			t.Errorf("cycle %d usage: sidecar %+v, agent %+v", cy.N, cy.Sidecar, cy.Agent)
		}
	}
	if hinted, unhinted := repo.Cycles[0].AgentWaitSeconds, repo.Cycles[1].AgentWaitSeconds; hinted >= unhinted {
		t.Errorf("the hinted wake waited %.1fs, the unhinted one %.1fs", hinted, unhinted)
	}
}
