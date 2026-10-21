// Command sbx-costreport replays the TraceLab coding-agent sessions
// against sleep policies, with the wake and suspend times the bench
// measured, and writes what each policy costs and makes agents wait.
//
//	sbx-costreport --trace data/costsim/trace.jsonl [--bench notes/bench-aws-<date>.json]
//
// The bench defaults to the newest AWS run in notes/, else the newest kind
// run. The report goes to notes/cost-report-<date>.{md,json}.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/bench"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/costsim"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sbx-costreport:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		tracePath, benchPath, out, notes string
		tail                             time.Duration
		deps, mid, turn                  string
		internal                         bool
		expected                         int
		actualPath, modeledPath          string
	)
	flag.StringVar(&tracePath, "trace", "data/costsim/trace.jsonl", "exported TraceLab sessions (see hack/costreport/export.sql)")
	flag.StringVar(&benchPath, "bench", "", "bench results to take the platform's numbers from (default: the newest in notes/, AWS first)")
	flag.StringVar(&notes, "notes", "notes", "where bench results are and the report goes")
	flag.StringVar(&out, "out", "", "report path without extension (default notes/cost-report-<date>)")
	flag.DurationVar(&tail, "tail", 30*time.Minute, "how long a sandbox lives after its session's last event")
	flag.StringVar(&deps, "deps", string(costsim.DepsAuto), "which calls need dependencies: auto, none or shell")
	flag.BoolVar(&internal, "internal-latency", false, "time sandbox calls by their own reported duration")
	flag.StringVar(&mid, "adaptive-mid", "15m", "the adaptive config's timeout inside a turn")
	flag.StringVar(&turn, "adaptive-turn", "2m", "the adaptive config's timeout after the agent's turn ends")
	flag.IntVar(&expected, "expected-roots", 2506, "root sessions the trace should have once subagents are grouped")
	flag.StringVar(&actualPath, "actual", "", "the AWS run's billed cost (JSON from make aws-cost-actual)")
	flag.StringVar(&modeledPath, "modeled", "", "the AWS run's sbx_cost_usd_per_hour integral (JSON from make aws-bench)")
	flag.Parse()

	kind, aws := newest(notes, "bench-kind-"), newest(notes, "bench-aws-")
	if benchPath == "" {
		benchPath = aws
		if benchPath == "" {
			benchPath = kind
		}
	}
	if benchPath == "" {
		return errors.New("no bench results in " + notes + "; run make bench first, or pass --bench")
	}
	b, err := readBench(benchPath)
	if err != nil {
		return err
	}
	f, err := os.Open(tracePath)
	if err != nil {
		return fmt.Errorf("%w (run make cost-report, which exports it)", err)
	}
	sessions, err := costsim.Read(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	groups, st := costsim.GroupSessions(sessions)
	if st.Roots["total"] != expected {
		return fmt.Errorf("%d root sessions (%d raw: claude %d, codex %d), want %d; check the export or pass --expected-roots",
			st.Roots["total"], st.Raw["total"], st.Raw["claude"], st.Raw["codex"], expected)
	}

	m := b.CostSim.Mean
	opts := costsim.Options{
		Tail:  tail,
		Build: costsim.BuildOptions{Deps: costsim.DepsMode(deps), InternalLatency: internal},
		Platform: costsim.Platform{
			WakeServe: seconds(m.WakeServeSeconds), WakeDeps: seconds(m.WakeDepsSeconds), Suspend: seconds(m.SuspendSeconds),
		},
		Usage: costsim.Usage{StoredBytes: int64(m.SnapshotBytes + m.DepArchiveBytes),
			PutRequestsPerCycle: m.PutRequestsPerCycle, GetRequestsPerCycle: m.GetRequestsPerCycle},
		Prices: costsim.Reference, SandboxUsdPerHour: m.SandboxUsdPerHour, ExpectedRoots: expected,
	}
	configs := costsim.Configs(mid, turn)
	rep, err := costsim.Run(groups, st, configs, opts)
	if err != nil {
		return err
	}
	shell := opts
	shell.Build.Deps = costsim.DepsShell
	pessimistic, err := costsim.Run(groups, st, configs, shell)
	if err != nil {
		return err
	}

	if out == "" {
		out = filepath.Join(notes, "cost-report-"+time.Now().Format("2006-01-02"))
	}
	md, err := os.Create(out + ".md")
	if err != nil {
		return err
	}
	defer md.Close()
	writeHeader(md, benchPath, b, kind, aws)
	rep.Markdown(md)
	writeSensitivity(md, pessimistic)
	if err := writeValidation(md, actualPath, modeledPath); err != nil {
		return err
	}
	if err := writePerRepo(md, groups, st, configs, opts, b); err != nil {
		return err
	}
	j, err := json.MarshalIndent(struct {
		Bench           string          `json:"bench"`
		Report          *costsim.Report `json:"report"`
		DepsShellReport *costsim.Report `json:"depsShellSensitivity"`
	}{benchPath, rep, pessimistic}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out+".json", append(j, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s.md and %s.json\n", out, out)
	return nil
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// newest is the latest notes/<prefix><date>.json.
func newest(dir, prefix string) string {
	files, _ := filepath.Glob(filepath.Join(dir, prefix+"*.json"))
	sort.Strings(files)
	if len(files) == 0 {
		return ""
	}
	return files[len(files)-1]
}

func readBench(path string) (*bench.Result, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b bench.Result
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(b.Repos) == 0 || b.CostSim.Mean.WakeServeSeconds <= 0 {
		return nil, fmt.Errorf("%s: no complete bench run in it", path)
	}
	return &b, nil
}

func writeHeader(w io.Writer, benchPath string, b *bench.Result, kind, aws string) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
	p("# Sleep policy cost report\n")
	p("TraceLab v0.0.1 coding-agent sessions (Claude Code and Codex), replayed against sleep policies with the platform's measured wake and suspend times from `%s` (%s, %s).\n",
		filepath.Base(benchPath), b.Env, b.StartedAt.Format("2006-01-02"))
	m := b.CostSim.Mean
	p("Platform numbers used (mean of %d repositories): wake to serving %.1fs, to dependencies %.1fs, suspend %.1fs, %.0f PUT + %.0f GET requests per cycle, %s snapshot + %s dependency archive stored while asleep, benched sandbox $%.4f/h.\n",
		len(b.Repos), m.WakeServeSeconds, m.WakeDepsSeconds, m.SuspendSeconds, m.PutRequestsPerCycle, m.GetRequestsPerCycle,
		costsim.Bytes(m.SnapshotBytes), costsim.Bytes(m.DepArchiveBytes), m.SandboxUsdPerHour)

	runs := map[string]*bench.Result{}
	for env, path := range map[string]string{"kind": kind, "aws": aws} {
		if path == "" {
			continue
		}
		if r, err := readBench(path); err == nil {
			runs[env] = r
		}
	}
	p("## Bench: kind vs AWS\n")
	if runs["aws"] == nil {
		p("No AWS run yet; kind only.\n")
	}
	envs := []string{"kind", "aws"}
	for _, repo := range repoNames(runs) {
		p("**%s**\n", repo)
		p("| Metric | kind avg | kind p95 | AWS avg | AWS p95 |")
		p("|---|---:|---:|---:|---:|")
		for _, row := range benchRows {
			cells := []string{row.label}
			for _, env := range envs {
				r := find(runs[env], repo)
				if r == nil {
					cells = append(cells, "–", "–")
					continue
				}
				avg, p95 := row.get(r)
				cells = append(cells, row.fmt(avg), row.fmt(p95))
			}
			p("| %s |", strings.Join(cells, " | "))
		}
		p("")
	}
}

func repoNames(runs map[string]*bench.Result) []string {
	seen := map[string]bool{}
	var out []string
	for _, env := range []string{"kind", "aws"} {
		if r := runs[env]; r != nil {
			for _, rr := range r.Repos {
				if !seen[rr.Name] {
					seen[rr.Name] = true
					out = append(out, rr.Name)
				}
			}
		}
	}
	return out
}

func find(r *bench.Result, name string) *bench.RepoResult {
	if r == nil {
		return nil
	}
	for i := range r.Repos {
		if r.Repos[i].Name == name {
			return &r.Repos[i]
		}
	}
	return nil
}

type benchRow struct {
	label string
	get   func(*bench.RepoResult) (avg, p95 float64)
	fmt   func(float64) string
}

func sum(s bench.Summary) (float64, float64) { return s.Avg, s.P95 }

func secondsFmt(v float64) string { return fmt.Sprintf("%.1fs", v) }
func mbFmt(v float64) string      { return costsim.Bytes(v) }
func countFmt(v float64) string   { return fmt.Sprintf("%.1f", v) }

var benchRows = []benchRow{
	{"First run: up (clone, disk, pod)", func(r *bench.RepoResult) (float64, float64) {
		return r.FirstRun.ProvisionSeconds, r.FirstRun.ProvisionSeconds
	}, secondsFmt},
	{"First run: dependency install (cache miss)", func(r *bench.RepoResult) (float64, float64) {
		return r.FirstRun.DepsInstallSeconds, r.FirstRun.DepsInstallSeconds
	}, secondsFmt},
	{"Suspend", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.SuspendSeconds) }, secondsFmt},
	{"Files ready (from the wake request)", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.FilesReadySeconds) }, secondsFmt},
	{"Deps ready, cache hit", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.DepsReadySeconds) }, secondsFmt},
	{"Wake to serving, hinted", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.ServingSeconds["hinted"]) }, secondsFmt},
	{"Wake to serving, unhinted", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.ServingSeconds["unhinted"]) }, secondsFmt},
	{"Agent waited, hinted", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.AgentWaitSeconds["hinted"]) }, secondsFmt},
	{"Agent waited, unhinted", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.AgentWaitSeconds["unhinted"]) }, secondsFmt},
	{"Snapshot bundle", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.BundleBytes) }, mbFmt},
	{"Dependency archive", func(r *bench.RepoResult) (float64, float64) {
		return float64(r.DepArchiveBytes), float64(r.DepArchiveBytes)
	}, mbFmt},
	{"S3 PUT-class requests per cycle", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.S3PutPerCycle) }, countFmt},
	{"S3 GET-class requests per cycle", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.S3GetPerCycle) }, countFmt},
	{"Sidecar CPU per wake", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.SidecarCPUSeconds) }, secondsFmt},
	{"Sidecar peak RSS", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.SidecarPeakRSSBytes) }, mbFmt},
	{"Agent CPU per wake", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.AgentCPUSeconds) }, secondsFmt},
	{"Agent peak RSS", func(r *bench.RepoResult) (float64, float64) { return sum(r.Summary.AgentPeakRSSBytes) }, mbFmt},
}

func writeSensitivity(w io.Writer, r *costsim.Report) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
	p("\n## Sensitivity: every shell call needs dependencies\n")
	p("The trace doesn't say what a shell call ran, so this reruns the policies as if every one ran the package manager and had to wait for the dependency archive (%.1fs into a wake instead of %.1fs).\n",
		r.Options.WakeDeps, r.Options.WakeServe)
	p("| Config | Saved vs baseline | Visible waits | Total wait | Hidden wakes |")
	p("|---|---:|---:|---:|---:|")
	for _, c := range r.Configs {
		hidden := "–"
		if h := c.UserMessageWakes.HiddenRate; h != nil {
			hidden = fmt.Sprintf("%.1f%%", 100**h)
		}
		saved := "–"
		if c.SavedVsBase != nil {
			saved = fmt.Sprintf("%+.1f%%", 100**c.SavedVsBase)
		}
		p("| %s | %s | %d | %.1f h | %s |", c.Name, saved, c.Waits.Visible, c.Waits.TotalHours, hidden)
	}
}

// writePerRepo reruns the policies with each repository's own numbers.
func writePerRepo(w io.Writer, groups []costsim.Group, st costsim.GroupStats, configs []costsim.Config, base costsim.Options, b *bench.Result) error {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
	p("\n## Appendix: per repository\n")
	p("The same simulation with each repository's own bench numbers instead of their mean.\n")
	p("| Repository | Config | Saved vs baseline | $ / session | Visible waits | Hidden wakes |")
	p("|---|---|---:|---:|---:|---:|")
	for _, rr := range b.Repos {
		in := b.CostSim.PerRepo[rr.Name]
		o := base
		o.Platform = costsim.Platform{WakeServe: seconds(in.WakeServeSeconds), WakeDeps: seconds(in.WakeDepsSeconds), Suspend: seconds(in.SuspendSeconds)}
		o.Usage = costsim.Usage{StoredBytes: int64(in.SnapshotBytes + in.DepArchiveBytes), PutRequestsPerCycle: in.PutRequestsPerCycle, GetRequestsPerCycle: in.GetRequestsPerCycle}
		o.SandboxUsdPerHour = in.SandboxUsdPerHour
		r, err := costsim.Run(groups, st, configs, o)
		if err != nil {
			return err
		}
		for _, c := range r.Configs {
			saved, hidden := "–", "–"
			if c.SavedVsBase != nil {
				saved = fmt.Sprintf("%+.1f%%", 100**c.SavedVsBase)
			}
			if h := c.UserMessageWakes.HiddenRate; h != nil {
				hidden = fmt.Sprintf("%.1f%%", 100**h)
			}
			p("| %s | %s | %s | $%.5f | %d | %s |", rr.Name, c.Name, saved, c.PerSession, c.Waits.Visible, hidden)
		}
	}
	return nil
}

// actualCost is the billed cost of an AWS run, from Cost Explorer.
type actualCost struct {
	Run       string             `json:"run"`
	Start     string             `json:"start"`
	End       string             `json:"end"`
	TotalUsd  float64            `json:"totalUsd"`
	ByService map[string]float64 `json:"byService"`
}

// modeledCost is what the platform's own metrics said over the same run.
type modeledCost struct {
	Start         string  `json:"start"`
	End           string  `json:"end"`
	WorkspacesUsd float64 `json:"workspacesUsd"` // the integral of sbx_cost_usd_per_hour
	StorageUsd    float64 `json:"storageUsd"`    // the integral of sbx_object_store_cost_usd_per_hour
	NodeHours     float64 `json:"nodeHours"`
	NodeUsd       float64 `json:"nodeUsd"` // node hours at list price
	ControlUsd    float64 `json:"controlPlaneUsd"`
}

func writeValidation(w io.Writer, actualPath, modeledPath string) error {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
	p("\n## Validation against the AWS bill\n")
	if actualPath == "" || modeledPath == "" {
		p("Not yet: it needs an AWS run (`make aws-bench`) and its billed cost, checked the next day (`make aws-cost-actual`).")
		return nil
	}
	var a actualCost
	var m modeledCost
	for path, v := range map[string]any{actualPath: &a, modeledPath: &m} {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, v); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	p("Run `%s`, billed %s to %s (Cost Explorer, filtered by the run tag) against what the platform measured over the bench window %s to %s.\n", a.Run, a.Start, a.End, m.Start, m.End)
	p("| | USD |")
	p("|---|---:|")
	p("| Billed in total (tagged resources) | $%.4f |", a.TotalUsd)
	services := make([]string, 0, len(a.ByService))
	for s := range a.ByService {
		services = append(services, s)
	}
	sort.Strings(services)
	for _, s := range services {
		p("| — %s | $%.4f |", s, a.ByService[s])
	}
	p("| sbx_cost_usd_per_hour over the window (workspace requests) | $%.4f |", m.WorkspacesUsd)
	p("| sbx_object_store_cost_usd_per_hour over the window | $%.4f |", m.StorageUsd)
	p("| Nodes at list price (%.2f node-hours) | $%.4f |", m.NodeHours, m.NodeUsd)
	p("| EKS control plane | $%.4f |", m.ControlUsd)
	p("\nThe platform's metric prices what workspaces request, not the nodes they run on: the gap to the bill is idle node capacity, the platform's own pods, the control plane, and the setup and teardown outside the bench window.")
	return nil
}
