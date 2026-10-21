package metrics_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/cost"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
)

// recorder is a Registerer that keeps what is registered, so every metric
// can be listed from its description, recorded values or not.
type recorder struct{ cs []prometheus.Collector }

func (r *recorder) Register(c prometheus.Collector) error   { r.cs = append(r.cs, c); return nil }
func (r *recorder) MustRegister(cs ...prometheus.Collector) { r.cs = append(r.cs, cs...) }
func (r *recorder) Unregister(prometheus.Collector) bool    { return false }

var fqName = regexp.MustCompile(`fqName: "([^"]+)"`)

// defined lists every metric the controller and gateway can export.
func defined(t *testing.T) map[string]bool {
	t.Helper()
	r := &recorder{}
	metrics.NewWorkspace(r, func(context.Context) (map[string]int, error) { return nil, nil })
	metrics.NewSnapshot(r)
	metrics.NewSleep(r, func() (map[string]int, map[string]int) { return nil, nil }, []string{"suspend"})
	metrics.NewGateway(r, func() int { return 0 })
	metrics.NewCost(r, func(context.Context) (cost.Summary, error) { return cost.Summary{}, nil })
	metrics.NewStorage(r)
	metrics.NewObjectStore(r)
	names := map[string]bool{}
	for _, c := range r.cs {
		ch := make(chan *prometheus.Desc, 64)
		c.Describe(ch)
		close(ch)
		for d := range ch {
			if m := fqName.FindStringSubmatch(d.String()); m != nil {
				names[m[1]] = true
			}
		}
	}
	return names
}

var metricRef = regexp.MustCompile(`\bsbx_[a-z0-9_]+`)

// exprs returns every PromQL expression in a dashboard, by panel title.
func exprs(t *testing.T, path string) (uid string, out map[string][]string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		UID    string `json:"uid"`
		Title  string `json:"title"`
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if d.UID == "" || d.Title == "" || len(d.Panels) == 0 {
		t.Fatalf("%s: a dashboard needs a uid, a title and panels", path)
	}
	out = map[string][]string{}
	for _, p := range d.Panels {
		if len(p.Targets) == 0 {
			t.Errorf("%s: panel %q queries nothing", path, p.Title)
		}
		for _, tg := range p.Targets {
			if strings.TrimSpace(tg.Expr) == "" {
				t.Errorf("%s: panel %q has an empty query", path, p.Title)
			}
			out[p.Title] = append(out[p.Title], tg.Expr)
		}
	}
	return d.UID, out
}

// TestDashboardsUseOnlyDefinedMetrics fails when a dashboard queries a
// metric nothing exports, such as after a rename.
func TestDashboardsUseOnlyDefinedMetrics(t *testing.T) {
	have := defined(t)
	files, err := filepath.Glob("../../charts/sbx/dashboards/*.json")
	if err != nil || len(files) != 3 {
		t.Fatalf("dashboards = %v (%v), want fleet, wake and storage-cost", files, err)
	}
	uids := map[string]bool{}
	used := map[string]bool{}
	for _, f := range files {
		uid, panels := exprs(t, f)
		if uids[uid] {
			t.Errorf("%s: uid %q is taken", f, uid)
		}
		uids[uid] = true
		for title, qs := range panels {
			for _, q := range qs {
				for _, ref := range metricRef.FindAllString(q, -1) {
					name := ref
					for _, suffix := range []string{"_bucket", "_sum", "_count"} {
						if base, ok := strings.CutSuffix(ref, suffix); ok && have[base] {
							name = base
						}
					}
					if !have[name] {
						t.Errorf("%s: panel %q queries %s, which nothing exports", filepath.Base(f), title, ref)
					}
					used[name] = true
				}
			}
		}
	}
	// And the other way: say which exported metrics no dashboard shows.
	var unused []string
	for n := range have {
		if !used[n] {
			unused = append(unused, n)
		}
	}
	sort.Strings(unused)
	t.Logf("exported but on no dashboard: %v", unused)
}
