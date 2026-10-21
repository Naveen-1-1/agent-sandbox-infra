package costsim

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
)

// Config is one policy setup to compare.
type Config struct {
	Name     string            `json:"name"`
	Label    string            `json:"label"`
	Policy   string            `json:"policy"`
	Params   map[string]string `json:"params,omitempty"`
	Hints    bool              `json:"hints"`
	Baseline bool              `json:"baseline,omitempty"`
}

// Configs are the setups the report compares: never sleeping, the
// baseline (15 minutes idle, no hint), shorter timeouts with hints, and
// adaptive (a short timeout once the agent's turn ends, a long one
// inside a turn).
func Configs(adaptiveMid, adaptiveTurn string) []Config {
	fx := func(idle string) map[string]string {
		return map[string]string{"idleTimeout": idle, "afterTurnEnded": "0"}
	}
	return []Config{
		{Name: "never", Label: "never sleep", Policy: "never"},
		{Name: "fixed-15m-nohint", Label: "fixed 15m, no hint", Policy: "fixed", Params: fx("15m"), Baseline: true},
		{Name: "fixed-15m", Label: "fixed 15m + hint", Policy: "fixed", Params: fx("15m"), Hints: true},
		{Name: "fixed-5m", Label: "fixed 5m + hint", Policy: "fixed", Params: fx("5m"), Hints: true},
		{Name: "fixed-2m", Label: "fixed 2m + hint", Policy: "fixed", Params: fx("2m"), Hints: true},
		{Name: "adaptive", Label: fmt.Sprintf("adaptive (%s mid-turn, %s after the turn) + hint", adaptiveMid, adaptiveTurn),
			Policy: "fixed", Params: map[string]string{"idleTimeout": adaptiveMid, "afterTurnEnded": adaptiveTurn}, Hints: true},
	}
}

// Options for a report.
type Options struct {
	Tail     time.Duration
	Build    BuildOptions
	Platform Platform
	Usage    Usage
	Prices   Prices // the reference prices everything is simulated at
	// SandboxUsdPerHour is the benched sandbox's price, for scaling.
	SandboxUsdPerHour float64
	ExpectedRoots     int
}

// WaitDist describes visible waits, in seconds.
type WaitDist struct {
	P50     float64  `json:"p50Seconds"`
	P90     float64  `json:"p90Seconds"`
	P99     float64  `json:"p99Seconds"`
	Max     float64  `json:"maxSeconds"`
	Buckets []Bucket `json:"buckets"`
}

// Bucket counts waits up to LE seconds (and over the previous bucket);
// LE 0 is the overflow bucket.
type Bucket struct {
	LE float64 `json:"leSeconds"`
	N  int     `json:"n"`
}

var bucketBounds = []float64{1, 2, 5, 10, 20, 30, 60}

func waitDist(xs []float64) WaitDist {
	d := WaitDist{P50: quantile(xs, 0.5), P90: quantile(xs, 0.9), P99: quantile(xs, 0.99)}
	for _, x := range xs {
		d.Max = math.Max(d.Max, x)
	}
	for i, le := range append(bucketBounds, 0) {
		b := Bucket{LE: le}
		for _, x := range xs {
			lo := 0.0
			if i > 0 {
				lo = bucketBounds[i-1]
			}
			if x > lo && (le == 0 || x <= le) {
				b.N++
			}
		}
		d.Buckets = append(d.Buckets, b)
	}
	return d
}

// ConfigResult is one config over every session.
type ConfigResult struct {
	Config
	Hours struct {
		Awake      float64 `json:"awake"`
		Suspending float64 `json:"suspending"`
		Waking     float64 `json:"waking"`
		Asleep     float64 `json:"asleep"`
	} `json:"hours"`
	Suspends     int      `json:"suspends"`
	Aborts       int      `json:"aborts"`
	HintWakes    int      `json:"hintWakes"`
	RequestWakes int      `json:"requestWakes"`
	UnusedHints  int      `json:"unusedHints"`
	Cost         Cost     `json:"cost"`
	PerSession   float64  `json:"perSessionUsd"`
	SavedVsBase  *float64 `json:"savedVsBaseline"`
	SavedVsNever *float64 `json:"savedVsNever"`
	Waits        struct {
		Visible           int           `json:"visible"`
		ChangeVsBaseline  *float64      `json:"changeVsBaseline"`
		TotalHours        float64       `json:"totalHours"`
		PerSessionSeconds float64       `json:"perSessionSeconds"`
		ByCause           map[Cause]int `json:"byCause"`
	} `json:"waits"`
	UserMessageWakes struct {
		Count      int      `json:"count"`
		Hidden     int      `json:"hidden"`
		HiddenRate *float64 `json:"hiddenRate"`
		Visible    WaitDist `json:"visible"` // the waits of the ones not hidden
	} `json:"userMessageWakes"`
}

// Report is the whole comparison.
type Report struct {
	Options struct {
		TailMinutes     float64  `json:"tailMinutes"`
		Deps            DepsMode `json:"deps"`
		InternalLatency bool     `json:"internalLatency"`
		WakeServe       float64  `json:"wakeServeSeconds"`
		WakeDeps        float64  `json:"wakeDepsSeconds"`
		Suspend         float64  `json:"suspendSeconds"`
		Usage           Usage    `json:"usage"`
		Prices          Prices   `json:"prices"`
		SandboxPerHour  float64  `json:"sandboxUsdPerHour"`
	} `json:"options"`
	Sessions struct {
		GroupStats
		Expected  int `json:"expectedRoots"`
		Simulated int `json:"simulated"`
	} `json:"sessions"`
	Quality      Quality              `json:"quality"`
	Gaps         map[Class]GapStats   `json:"gaps"`
	HiddenWindow HiddenWindow         `json:"hiddenWindow"`
	Configs      []ConfigResult       `json:"configs"`
	AtScale      Extrapolation        `json:"atScale"`
	Assumptions  []string             `json:"assumptions"`
	PerProvider  map[string]Breakdown `json:"perProvider"`
}

// HiddenWindow is how long agents take from a person's input to their
// first sandbox call: the time a hinted wake has to hide in.
type HiddenWindow struct {
	Inputs       int     `json:"inputs"` // inputs followed by a sandbox call
	P50          float64 `json:"p50Seconds"`
	P90          float64 `json:"p90Seconds"`
	AtLeastServe float64 `json:"atLeastServe"` // share at least as long as a wake to serving
	AtLeastDeps  float64 `json:"atLeastDeps"`
}

// Breakdown is a provider's share of the sessions and the baseline cost.
type Breakdown struct {
	Sessions     int     `json:"sessions"`
	BaselineUsd  float64 `json:"baselineUsd"`
	BestSavedPct float64 `json:"bestSavedVsBaseline"`
}

// Extrapolation scales the savings per session to a year of sessions.
type Extrapolation struct {
	Basis         string  `json:"basis"`
	Scale         float64 `json:"scale"`         // sandbox $/h over the reference $/h
	UnscaledShare float64 `json:"unscaledShare"` // storage and requests in the baseline
	Rows          []struct {
		Config            string             `json:"config"`
		SavedPerSessionRe float64            `json:"savedPerSessionReferenceUsd"`
		SavedPerSession   float64            `json:"savedPerSessionUsd"`
		PerYear           map[string]float64 `json:"perYearUsd"`
	} `json:"rows"`
}

// Volumes are the yearly session counts the report extrapolates to.
var Volumes = []struct {
	Label string
	N     float64
}{{"1M", 1e6}, {"10M", 1e7}, {"100M", 1e8}}

// Run simulates every config over every group.
func Run(groups []Group, st GroupStats, configs []Config, o Options) (*Report, error) {
	r := &Report{Gaps: map[Class]GapStats{}, PerProvider: map[string]Breakdown{}}
	r.Options.TailMinutes, r.Options.Deps, r.Options.InternalLatency = o.Tail.Minutes(), o.Build.Deps, o.Build.InternalLatency
	r.Options.WakeServe, r.Options.WakeDeps, r.Options.Suspend = o.Platform.WakeServe.Seconds(), o.Platform.WakeDeps.Seconds(), o.Platform.Suspend.Seconds()
	r.Options.Usage, r.Options.Prices, r.Options.SandboxPerHour = o.Usage, o.Prices, o.SandboxUsdPerHour
	r.Sessions.GroupStats, r.Sessions.Expected, r.Sessions.Simulated = st, o.ExpectedRoots, len(groups)

	tls := make([]Timeline, len(groups))
	var gaps [][]Gap
	var windows []float64
	for i, g := range groups {
		tls[i] = Build(g, o.Build)
		r.Quality.add(tls[i].Quality)
		gaps = append(gaps, Gaps(tls[i], o.Tail.Microseconds()))
		for _, in := range tls[i].Inputs {
			if in.First >= 0 {
				windows = append(windows, float64(in.First-in.T)/1e6)
			}
		}
	}
	r.Gaps = SummarizeGaps(gaps)
	r.HiddenWindow = HiddenWindow{Inputs: len(windows), P50: quantile(windows, 0.5), P90: quantile(windows, 0.9),
		AtLeastServe: shareAtLeast(windows, o.Platform.WakeServe.Seconds()), AtLeastDeps: shareAtLeast(windows, o.Platform.WakeDeps.Seconds())}

	perProvider := map[string]map[string]float64{} // provider → config → cost
	for _, c := range configs {
		pol, err := policy.New(c.Policy, c.Params)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Name, err)
		}
		cr := ConfigResult{Config: c}
		cr.Waits.ByCause = map[Cause]int{}
		var visible []float64
		for i, tl := range tls {
			res, err := Simulate(tl, pol, c.Hints, o.Platform, o.Tail)
			if err != nil {
				return nil, err
			}
			cost := o.Prices.Of(res, o.Usage)
			cr.Cost.add(cost)
			p := groups[i].Provider
			if perProvider[p] == nil {
				perProvider[p] = map[string]float64{}
			}
			perProvider[p][c.Name] += cost.TotalUsd
			cr.Hours.Awake += res.Awake.Hours()
			cr.Hours.Suspending += res.Suspending.Hours()
			cr.Hours.Waking += res.Waking.Hours()
			cr.Hours.Asleep += res.Asleep.Hours()
			cr.Suspends += res.Suspends
			cr.Aborts += res.Aborts
			cr.HintWakes += res.HintWakes
			cr.RequestWakes += res.RequestWakes
			cr.UnusedHints += res.UnusedHints
			cr.UserMessageWakes.Count += res.UserMessageWakes
			cr.UserMessageWakes.Hidden += res.Hidden
			for _, h := range res.Holds {
				cr.Waits.Visible++
				cr.Waits.ByCause[h.Cause]++
				cr.Waits.TotalHours += h.Wait.Hours()
				if h.Cause == CauseUserMessage {
					visible = append(visible, h.Wait.Seconds())
				}
			}
		}
		if n := len(tls); n > 0 {
			cr.PerSession = cr.Cost.TotalUsd / float64(n)
			cr.Waits.PerSessionSeconds = cr.Waits.TotalHours * 3600 / float64(n)
		}
		cr.UserMessageWakes.HiddenRate = ratio(float64(cr.UserMessageWakes.Hidden), float64(cr.UserMessageWakes.Count))
		cr.UserMessageWakes.Visible = waitDist(visible)
		r.Configs = append(r.Configs, cr)
	}

	var base, never *ConfigResult
	for i := range r.Configs {
		switch {
		case r.Configs[i].Baseline:
			base = &r.Configs[i]
		case r.Configs[i].Policy == "never":
			never = &r.Configs[i]
		}
	}
	for i := range r.Configs {
		c := &r.Configs[i]
		if base != nil {
			c.SavedVsBase = saved(base.Cost.TotalUsd, c.Cost.TotalUsd)
			c.Waits.ChangeVsBaseline = change(float64(base.Waits.Visible), float64(c.Waits.Visible))
		}
		if never != nil {
			c.SavedVsNever = saved(never.Cost.TotalUsd, c.Cost.TotalUsd)
		}
	}
	for p, byConfig := range perProvider {
		b := Breakdown{Sessions: r.Sessions.Roots[p]}
		if base != nil {
			b.BaselineUsd = byConfig[base.Name]
			for _, c := range configs {
				if b.BaselineUsd > 0 && !c.Baseline && c.Policy != "never" {
					b.BestSavedPct = math.Max(b.BestSavedPct, (b.BaselineUsd-byConfig[c.Name])/b.BaselineUsd)
				}
			}
		}
		r.PerProvider[p] = b
	}
	r.AtScale = extrapolate(r.Configs, base, o)
	r.Assumptions = assumptions(o)
	return r, nil
}

func extrapolate(cs []ConfigResult, base *ConfigResult, o Options) Extrapolation {
	var e Extrapolation
	if base == nil || len(cs) == 0 {
		return e
	}
	e.Scale = o.SandboxUsdPerHour / ReferenceUsdPerHour
	e.Basis = fmt.Sprintf("Simulated at $%.4f/h (one m7g.large on demand); the benched sandbox costs $%.4f/h, so savings scale by %.3f.",
		ReferenceUsdPerHour, o.SandboxUsdPerHour, e.Scale)
	if base.Cost.TotalUsd > 0 {
		e.UnscaledShare = (base.Cost.StorageUsd + base.Cost.RequestsUsd) / base.Cost.TotalUsd
	}
	for _, c := range cs {
		if c.Baseline {
			continue
		}
		row := struct {
			Config            string             `json:"config"`
			SavedPerSessionRe float64            `json:"savedPerSessionReferenceUsd"`
			SavedPerSession   float64            `json:"savedPerSessionUsd"`
			PerYear           map[string]float64 `json:"perYearUsd"`
		}{Config: c.Name, SavedPerSessionRe: base.PerSession - c.PerSession, PerYear: map[string]float64{}}
		row.SavedPerSession = row.SavedPerSessionRe * e.Scale
		for _, v := range Volumes {
			row.PerYear[v.Label] = v.N * row.SavedPerSession
		}
		e.Rows = append(e.Rows, row)
	}
	return e
}

func assumptions(o Options) []string {
	return []string{
		"One sandbox per session group (a session and its subagents, grouped by session id), awake from its first event; deleted " +
			fmt.Sprintf("%s after its last event (the session tail), snapshot included.", o.Tail),
		"Only what the gateway sees is activity: sandbox tool calls (Bash, Read, Edit, exec_command, apply_patch, ...) starting and ending, wake-hints, and wakes finishing. Model text, reasoning, web and MCP tools are invisible to it.",
		"A running sandbox call keeps the sandbox awake (the policy's command veto), however long it runs; the emitted-to-result span counts, approval prompts included.",
		"A person's input is the user message that starts a user-initiated round, or an answer to AskUserQuestion; with hints the harness sends a wake-hint then, which also counts as activity. Turn-ended is sent at a turn's last agent-side instant in every config.",
		fmt.Sprintf("Wakes take the bench's averages: %.1fs to serving, %.1fs to dependencies; a suspend takes %.1fs and is billed awake. A call during a suspend cuts it short at no wait.",
			o.Platform.WakeServe.Seconds(), o.Platform.WakeDeps.Seconds(), o.Platform.Suspend.Seconds()),
		fmt.Sprintf("Calls needing dependencies: %s (the trace has no executables, so shell calls can't be told apart).", o.Build.Deps),
		"Holds are closed-loop: a held call delays everything after it in its session.",
		"Hints are always delivered; the policy decides instantly (the controller polls every few seconds); the operation limiter and cluster capacity are ignored.",
		fmt.Sprintf("Asleep, a sandbox pays for %s in S3 (snapshot plus dependency archive, not shared); each suspend makes %.0f PUT-class and each wake %.0f GET-class requests.",
			Bytes(float64(o.Usage.StoredBytes)), o.Usage.PutRequestsPerCycle, o.Usage.GetRequestsPerCycle),
		fmt.Sprintf("Prices: $%.4f/h awake (one m7g.large on demand), S3 $%.3f/GB-month, $%.4f per 1k PUT, $%.4f per 1k GET (AWS us-east-1 list prices). No provisioning cost.",
			o.Prices.AwakeUsdPerHour, o.Prices.ObjectGiBMonth, o.Prices.PutPer1k, o.Prices.GetPer1k),
		"Extrapolation multiplies per-session savings at the reference price by the benched sandbox's price over the reference; storage and request costs are scaled with it although they don't depend on the sandbox's size (see their share).",
	}
}

func shareAtLeast(xs []float64, v float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	n := 0
	for _, x := range xs {
		if x >= v {
			n++
		}
	}
	return float64(n) / float64(len(xs))
}

func ratio(a, b float64) *float64 {
	if b == 0 {
		return nil
	}
	v := a / b
	return &v
}

func saved(base, c float64) *float64 {
	if base == 0 {
		return nil
	}
	v := (base - c) / base
	return &v
}

func change(base, c float64) *float64 {
	if base == 0 {
		return nil
	}
	v := (c - base) / base
	return &v
}

// Headline sums the comparison up in a few sentences.
func (r *Report) Headline() []string {
	var base, never *ConfigResult
	for i := range r.Configs {
		switch c := &r.Configs[i]; {
		case c.Baseline:
			base = c
		case c.Policy == "never":
			never = c
		}
	}
	if base == nil || never == nil {
		return nil
	}
	out := []string{fmt.Sprintf("Over %d sessions, never sleeping costs $%.2f; the baseline (%s) costs $%.2f, %s less, with %d visible waits (%.1f h in all).",
		r.Sessions.Simulated, never.Cost.TotalUsd, base.Label, base.Cost.TotalUsd, pct(*base.SavedVsNever), base.Waits.Visible, base.Waits.TotalHours)}
	for _, c := range r.Configs {
		if c.Baseline || c.Policy == "never" || c.SavedVsBase == nil {
			continue
		}
		hidden := ""
		if h := c.UserMessageWakes.HiddenRate; h != nil {
			hidden = fmt.Sprintf(", and %s of the wakes for a person's message finish before the agent's first call", pct(*h))
		}
		out = append(out, fmt.Sprintf("%s: %s vs the baseline, visible waits %s%s.", c.Label, signed(*c.SavedVsBase, "cheaper", "dearer"),
			signed(-*c.Waits.ChangeVsBaseline, "fewer", "more"), hidden))
	}
	return out
}

func signed(v float64, pos, neg string) string {
	if v >= 0 {
		return fmt.Sprintf("%s %s", pct(v), pos)
	}
	return fmt.Sprintf("%s %s", pct(-v), neg)
}

// Markdown writes the report's sections (sessions onwards).
func (r *Report) Markdown(w io.Writer) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

	if h := r.Headline(); len(h) > 0 {
		p("## Summary\n")
		for _, s := range h {
			p("- %s", s)
		}
		p("")
	}

	p("## Sessions\n")
	p("| | Claude Code | Codex | Total |")
	p("|---|---:|---:|---:|")
	row := func(label string, c Counts) { p("| %s | %d | %d | %d |", label, c["claude"], c["codex"], c["total"]) }
	row("Raw sessions in the trace", r.Sessions.Raw)
	row("of which subagents", r.Sessions.Subagents)
	row("Root sessions (subagents grouped)", r.Sessions.Roots)
	diff := r.Sessions.Roots["total"] - r.Sessions.Expected
	p("\nExpected %d roots: %s. Simulated %d (%d have no timestamps); %d subagents had no parent in the trace.\n",
		r.Sessions.Expected, map[bool]string{true: "matches", false: fmt.Sprintf("off by %+d", diff)}[diff == 0],
		r.Sessions.Simulated, r.Sessions.Empty, r.Sessions.Orphans)

	p("## Where the time goes\n")
	p("| Class | Hours | Share of session time | Gaps | Median | p90 |")
	p("|---|---:|---:|---:|---:|---:|")
	for _, c := range Classes {
		g := r.Gaps[c]
		share := pct(g.Share)
		if c == ClassTail {
			share = fmt.Sprintf("(%.0f min per session)", r.Options.TailMinutes)
		}
		p("| %s | %.1f | %s | %d | %s | %s |", classLabel[c], g.Hours, share, g.Gaps, dur(g.P50), dur(g.P90))
	}
	hw := r.HiddenWindow
	p("\nA person's input to the agent's first sandbox call (the window a hinted wake hides in): median %s, p90 %s over %d inputs; "+
		"%s are at least a wake to serving (%.1fs), %s at least a wake with dependencies (%.1fs).\n",
		dur(hw.P50), dur(hw.P90), hw.Inputs, pct(hw.AtLeastServe), r.Options.WakeServe, pct(hw.AtLeastDeps), r.Options.WakeDeps)

	p("## Policies\n")
	p("| Config | Total cost | Saved vs baseline | Saved vs never | $ / session | Visible waits | Δ vs baseline | Total wait | Hidden wakes | Visible wait p50 / p90 / max |")
	p("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, c := range r.Configs {
		u := c.UserMessageWakes
		hidden := "–"
		if u.Count > 0 {
			hidden = fmt.Sprintf("%s (%d of %d)", pct(*u.HiddenRate), u.Hidden, u.Count)
		}
		v := u.Visible
		vis := "–"
		if u.Count > u.Hidden {
			vis = fmt.Sprintf("%s / %s / %s", dur(v.P50), dur(v.P90), dur(v.Max))
		}
		label := c.Label
		if c.Baseline {
			label += " (baseline)"
		}
		p("| %s | $%.2f | %s | %s | $%.5f | %d | %s | %.1f h | %s | %s |", label, c.Cost.TotalUsd, pctp(c.SavedVsBase),
			pctp(c.SavedVsNever), c.PerSession, c.Waits.Visible, pctp(c.Waits.ChangeVsBaseline), c.Waits.TotalHours, hidden, vis)
	}
	p("\nA hint costs something too: it wakes a sleeping sandbox even when no sandbox call follows (unused hints, below), and, like any turn starting, restarts the idle timer.")

	p("\n## Policy detail\n")
	p("| Config | Awake h | Suspending h | Waking h | Asleep h | Suspends | Aborted | Hint wakes | Request wakes | Unused hints | Waits: user message / mid-turn / deps / abort |")
	p("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|")
	for _, c := range r.Configs {
		b := c.Waits.ByCause
		p("| %s | %.1f | %.1f | %.1f | %.1f | %d | %d | %d | %d | %d | %d / %d / %d / %d |", c.Name, c.Hours.Awake, c.Hours.Suspending,
			c.Hours.Waking, c.Hours.Asleep, c.Suspends, c.Aborts, c.HintWakes, c.RequestWakes, c.UnusedHints,
			b[CauseUserMessage], b[CauseMidTurn], b[CauseDeps], b[CauseAbort])
	}
	p("\nVisible waits of user-message wakes that weren't hidden, by length:\n")
	p("| Config | ≤1s | ≤2s | ≤5s | ≤10s | ≤20s | ≤30s | ≤60s | >60s |")
	p("|---|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, c := range r.Configs {
		var cells []string
		for _, b := range c.UserMessageWakes.Visible.Buckets {
			cells = append(cells, fmt.Sprint(b.N))
		}
		p("| %s | %s |", c.Name, strings.Join(cells, " | "))
	}

	providers := make([]string, 0, len(r.PerProvider))
	for k := range r.PerProvider {
		providers = append(providers, k)
	}
	sort.Strings(providers)
	p("\nBy agent:\n")
	for _, k := range providers {
		b := r.PerProvider[k]
		p("- %s: %d sessions, baseline $%.2f, best config saves %s.", k, b.Sessions, b.BaselineUsd, pct(b.BestSavedPct))
	}

	e := r.AtScale
	p("\n## At scale\n")
	p("%s Storage and requests are %s of the baseline's cost.\n", e.Basis, pct(e.UnscaledShare))
	p("| Config | Saved / session (reference) | Saved / session (benched sandbox) | 1M sessions/yr | 10M | 100M |")
	p("|---|---:|---:|---:|---:|---:|")
	for _, row := range e.Rows {
		p("| %s | $%.5f | $%.5f | %s | %s | %s |", row.Config, row.SavedPerSessionRe, row.SavedPerSession,
			money(row.PerYear["1M"]), money(row.PerYear["10M"]), money(row.PerYear["100M"]))
	}

	p("\n## Assumptions\n")
	for _, a := range r.Assumptions {
		p("- %s", a)
	}

	q := r.Quality
	p("\n## Data quality\n")
	p("- %d tool calls never returned (taken as instant), %d returned before they started, %d duplicate call ids.", q.OpenTools, q.NegativeTools, q.DuplicateTools)
	type kv struct {
		k string
		v int
	}
	var others []kv
	for k, v := range q.OtherTools {
		others = append(others, kv{k, v})
	}
	sort.Slice(others, func(i, j int) bool {
		return others[i].v > others[j].v || others[i].v == others[j].v && others[i].k < others[j].k
	})
	var top []string
	for i, o := range others {
		if i == 12 {
			break
		}
		top = append(top, fmt.Sprintf("%s %d", o.k, o.v))
	}
	p("- Tools counted as the model's own time (not sandbox, subagent or human), most common: %s.", strings.Join(top, ", "))
}

var classLabel = map[Class]string{
	ClassToolRunning: "Tool running", ClassAIThinking: "AI thinking", ClassHumanWait: "Human wait", ClassTail: "Session tail",
}

func pct(v float64) string { return fmt.Sprintf("%.1f%%", 100*v) }

func pctp(v *float64) string {
	if v == nil {
		return "–"
	}
	return fmt.Sprintf("%+.1f%%", 100**v)
}

func dur(sec float64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%.1fs", sec)
	case sec < 3600:
		return fmt.Sprintf("%.1f min", sec/60)
	}
	return fmt.Sprintf("%.1f h", sec/3600)
}

// Bytes formats a size in decimal units.
func Bytes(b float64) string {
	switch {
	case b >= 1e9:
		return fmt.Sprintf("%.2f GB", b/1e9)
	case b >= 1e6:
		return fmt.Sprintf("%.1f MB", b/1e6)
	case b >= 1e3:
		return fmt.Sprintf("%.1f KB", b/1e3)
	}
	return fmt.Sprintf("%.0f B", b)
}

func money(v float64) string {
	switch {
	case math.Abs(v) >= 1e6:
		return fmt.Sprintf("$%.2fM", v/1e6)
	case math.Abs(v) >= 1e3:
		return fmt.Sprintf("$%.1fk", v/1e3)
	}
	return fmt.Sprintf("$%.0f", v)
}
