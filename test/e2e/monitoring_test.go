//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

// prometheus is the bundled Prometheus, or skips the test without one.
func prometheus(t *testing.T) string {
	t.Helper()
	c := framework.NewClients(t)
	if _, err := c.Kube.CoreV1().Services(framework.Namespace()).Get(context.Background(), "sbx-monitoring-prometheus", metav1.GetOptions{}); err != nil {
		t.Skip("the monitoring stack isn't deployed")
	}
	return "http://" + framework.PortForward(t, framework.Namespace(), "svc/sbx-monitoring-prometheus", 9090)
}

type promResult struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func promQuery(t *testing.T, base, q string) promResult {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/query?query=" + url.QueryEscape(q))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r promResult
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// value is the first sample of a query, or -1 without one.
func value(r promResult) float64 {
	if len(r.Data.Result) == 0 {
		return -1
	}
	s, _ := r.Data.Result[0].Value[1].(string)
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// TestPlatformMetricsReachPrometheus: Prometheus scrapes the controller and
// every gateway replica, and a running workspace shows up in the counts
// and the cost.
func TestPlatformMetricsReachPrometheus(t *testing.T) {
	prom := prometheus(t)
	gw := framework.Gateway(t)
	p := framework.DeployedPlatform(t, framework.NewClients(t))
	resp, err := gw.Create(framework.Ctx(t, time.Minute), sbxapi.CreateRequest{
		RepoURL: p.FixturesURL + "/hello-node.git", Image: "debian:trixie-slim", Policy: &sbxapi.PolicyRef{Name: "never"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ns, name := resp.Workspace.Namespace, resp.Workspace.Name
	t.Cleanup(func() { _ = gw.Delete(context.Background(), ns, name) })
	own := &sbxapi.Client{BaseURL: gw.BaseURL, Token: resp.Token, HTTP: gw.HTTP}
	waitPhase(t, own, ns, name, "Running", 5*time.Minute)

	for q, want := range map[string]string{
		`count(up{job="sbx-controller"} == 1)`:        "the controller",
		`count(up{job="sbx-gateway-signals"} == 1)`:   "the gateway replicas",
		`max(sbx_workspaces{phase="Running"})`:        "a running workspace",
		`max(sbx_cost_usd_per_hour{phase="Running"})`: "the cost of a running workspace",
	} {
		framework.Eventually(t, 2*time.Minute, 2*time.Second, "Prometheus to see "+want, func() (bool, error) {
			return value(promQuery(t, prom, q)) > 0, nil
		})
	}
	if v := value(promQuery(t, prom, `count(up{job="sbx-gateway-signals"} == 1)`)); v < 2 {
		t.Errorf("%v gateway replicas scraped, want every one", v)
	}
}

// TestDashboardQueriesRun evaluates every query on the three dashboards
// against the live Prometheus: none may fail to parse or run.
func TestDashboardQueriesRun(t *testing.T) {
	prom := prometheus(t)
	files, err := filepath.Glob("../../charts/sbx/dashboards/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("dashboards: %v %v", files, err)
	}
	n := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Panels []struct {
				Title   string `json:"title"`
				Targets []struct {
					Expr string `json:"expr"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		for _, p := range d.Panels {
			for _, tg := range p.Targets {
				q := strings.ReplaceAll(tg.Expr, "$__rate_interval", "1m")
				if r := promQuery(t, prom, q); r.Status != "success" {
					t.Errorf("%s / %s: %s: %s", filepath.Base(f), p.Title, q, r.Error)
				}
				n++
			}
		}
	}
	t.Logf("%d dashboard queries ran", n)
}

// TestGrafanaHasTheDashboards checks Grafana loaded all three.
func TestGrafanaHasTheDashboards(t *testing.T) {
	prometheus(t) // skips without the stack
	addr := framework.PortForward(t, framework.Namespace(), "svc/sbx-grafana", 80)
	pass := framework.SecretValue(t, framework.Namespace(), "sbx-grafana", "admin-password")
	framework.Eventually(t, 2*time.Minute, 2*time.Second, "Grafana to load the dashboards", func() (bool, error) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/search?tag=sbx", nil)
		req.SetBasicAuth("admin", strings.TrimSpace(pass))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		var found []struct {
			UID string `json:"uid"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&found); err != nil {
			return false, err
		}
		uids := map[string]bool{}
		for _, d := range found {
			uids[d.UID] = true
		}
		return uids["sbx-fleet"] && uids["sbx-wake"] && uids["sbx-storage-cost"], nil
	})
	http.DefaultClient.CloseIdleConnections()
}
