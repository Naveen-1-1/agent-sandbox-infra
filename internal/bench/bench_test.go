package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestSummarize(t *testing.T) {
	if got := Summarize(nil); got != (Summary{}) {
		t.Errorf("empty: %+v", got)
	}
	got := Summarize([]float64{5, 1, 4, 2, 3})
	if got.N != 5 || got.Avg != 3 || got.P95 != 5 || got.Min != 1 || got.Max != 5 {
		t.Errorf("1..5: %+v", got)
	}
	xs := make([]float64, 20)
	for i := range xs {
		xs[i] = float64(i + 1)
	}
	if got := Summarize(xs).P95; got != 19 { // nearest rank: the 19th of 20
		t.Errorf("p95 of 1..20 = %v, want 19", got)
	}
}

func TestParseStat(t *testing.T) {
	// A name with spaces and a parenthesis; utime 150, stime 50, cutime 20, cstime 10.
	line := "42 (sbx (side) car) S 1 42 42 0 -1 4194560 100 0 0 0 150 50 20 10 20 0 8 0 300 10000000 2000"
	comm, ticks, err := parseStat(line)
	if err != nil || comm != "sbx (side) car" || ticks != 230 {
		t.Errorf("parseStat = %q %d %v", comm, ticks, err)
	}
	if _, _, err := parseStat("garbage"); err == nil {
		t.Error("a bad line parsed")
	}
	if got := parseStatus("Name:\tsbx\nVmPeak:\t 9 kB\nVmHWM:\t   40960 kB\nVmRSS:\t 100 kB\n"); got != 40960*1024 {
		t.Errorf("VmHWM = %d", got)
	}
}

func stat(comm string, utime int) string {
	return fmt.Sprintf("7 (%s) S 1 7 7 0 -1 0 0 0 0 0 %d 0 0 0 20 0 1 0 1 1 1", comm, utime)
}

func status(kB int) string { return fmt.Sprintf("Name:\tx\nVmHWM:\t%d kB\n", kB) }

func TestParseSidecarAndAgent(t *testing.T) {
	sep := "\n" + sectionSep + "\n"
	u, err := parseSidecar(stat("sbx-sidecar", 230) + sep + status(40960) + sep + "104857600\n")
	if err != nil || u.CPUSeconds != 2.3 || u.PeakRSSBytes != 40960*1024 || u.CgroupPeakBytes != 104857600 {
		t.Errorf("sidecar = %+v, %v", u, err)
	}
	if u, err := parseSidecar(stat("sbx-sidecar", 100) + sep + status(1) + sep); err != nil || u.CgroupPeakBytes != 0 {
		t.Errorf("sidecar without memory.peak = %+v, %v", u, err)
	}

	// The supervisor, the agent, and an orphan the supervisor reaped.
	out := sep + stat("sbx-agent", 10) + sep + status(5000) +
		sep + stat("sbx-agent", 90) + sep + status(30000) +
		sep + stat("npm", 1000) + sep + status(900000) + "\n"
	a, err := parseAgent(out)
	if err != nil || a.CPUSeconds != 1 || a.PeakRSSBytes != 30000*1024 {
		t.Errorf("agent = %+v, %v; want the supervisor and agent only", a, err)
	}
	if _, err := parseAgent("nothing"); err == nil {
		t.Error("no processes parsed")
	}
}

const page = `# TYPE sbx_object_store_requests_total counter
sbx_object_store_requests_total{caller="controller",op="put"} %d
sbx_object_store_requests_total{caller="controller",op="get"} %d
sbx_object_store_requests_total{caller="broker",op="head"} %d
sbx_object_store_requests_total{caller="usage",op="list"} %d
# TYPE sbx_object_store_delegated_requests_total counter
sbx_object_store_delegated_requests_total{kind="snapshots",op="put"} %d
sbx_object_store_delegated_requests_total{kind="deps",op="get"} %d
# TYPE sbx_cost_usd_per_hour gauge
sbx_cost_usd_per_hour{phase="Running"} 0.0123
sbx_cost_usd_per_hour{phase="Suspended"} 0.0001
`

// pageAt is a controller's counters after k steps of 3 controller puts,
// 1 get, 2 broker heads, 5 usage lists, 1 pod put and 2 pod gets.
func pageAt(k int) string {
	return fmt.Sprintf(page, 3*k, k, 2*k, 5*k, k, 2*k)
}

func TestCountersSince(t *testing.T) {
	prev, err := ParseMetrics([]string{pageAt(1)})
	if err != nil {
		t.Fatal(err)
	}
	// Two replicas: their counters add up.
	cur, err := ParseMetrics([]string{pageAt(2), pageAt(1)})
	if err != nil {
		t.Fatal(err)
	}
	if cur.RunningUsdPerHour != 0.0123 {
		t.Errorf("running cost = %v", cur.RunningUsdPerHour)
	}
	// Controller puts 3 → 9 and pod puts 1 → 3 are the put class; gets
	// 1 → 3, heads 2 → 6 and pod gets 2 → 6 the get class.
	r := cur.Since(prev)
	if r.PutClass != 6+2 || r.GetClass != 2+4+4 {
		t.Errorf("since = %+v", r)
	}
	if _, ok := r.ByOp["usage list"]; ok {
		t.Error("the usage poller's listing was counted")
	}
	if r.ByOp["pods get deps"] != 4 || r.ByOp["broker head"] != 4 {
		t.Errorf("by op = %v", r.ByOp)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.json")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := `{"repos":[{"name":"a","url":"https://x/a.git","commit":"` + strings.Repeat("a", 40) +
		`","profile":"npm","image":"node","command":"npm test","edit":"README.md"}]}`
	c, err := LoadConfig(write(good))
	if err != nil || len(c.Repos) != 1 {
		t.Fatalf("good config: %v", err)
	}
	if err := c.Only([]string{"b"}); err == nil {
		t.Error("an unknown repository was accepted")
	}
	for _, bad := range []string{
		strings.Replace(good, strings.Repeat("a", 40), "main", 1),
		strings.Replace(good, `"npm"`, `"yarn"`, 1),
		strings.Replace(good, `"edit":"README.md"`, `"edit":""`, 1),
		`{"repos":[]}`,
	} {
		if _, err := LoadConfig(write(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	// The committed config is valid.
	if _, err := LoadConfig("../../test/bench/repos.json"); err != nil {
		t.Errorf("test/bench/repos.json: %v", err)
	}
}
