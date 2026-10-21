package bench

import "time"

// Result is a whole run, written as JSON.
type Result struct {
	Env        string    `json:"env"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Platform   Platform  `json:"platform"`
	Options    struct {
		Cycles          int     `json:"cycles"`
		ThinkingSeconds float64 `json:"thinkingSeconds"`
	} `json:"options"`
	Repos []RepoResult `json:"repos"`
	// CostSim is what the cost report takes from the run.
	CostSim CostSim `json:"costsim"`
}

// Platform describes where the bench ran.
type Platform struct {
	Commit       string `json:"commit,omitempty"`
	Kubernetes   string `json:"kubernetes"`
	ToolsMode    string `json:"toolsMode,omitempty"`
	RuntimeImage string `json:"runtimeImage,omitempty"`
	Nodes        []Node `json:"nodes"`
}

// Node is one cluster node.
type Node struct {
	Name         string `json:"name"`
	InstanceType string `json:"instanceType,omitempty"`
	Arch         string `json:"arch"`
	CPU          string `json:"cpu"`
	Memory       string `json:"memory"`
}

// RepoResult is one repository's measurements.
type RepoResult struct {
	Repo
	Workspace string   `json:"workspace"`
	FirstRun  FirstRun `json:"firstRun"`
	// FirstSuspend is cycle 0: the suspend that uploads the dependency
	// archive and the wake that first restores it. It isn't summarized.
	FirstSuspend      Cycle       `json:"firstSuspend"`
	DepArchiveBytes   int64       `json:"depArchiveBytes"`
	SandboxUsdPerHour float64     `json:"sandboxUsdPerHour"` // at the platform's list prices
	Cycles            []Cycle     `json:"cycles"`
	Summary           RepoSummary `json:"summary"`
}

// FirstRun is the cold start: nothing cached.
type FirstRun struct {
	// ProvisionSeconds is from the create call until the workspace serves:
	// the clone, the disk and the pod.
	ProvisionSeconds  float64 `json:"provisionSeconds"`
	FilesReadySeconds float64 `json:"filesReadySeconds"` // from the controller's timeline
	ServingSeconds    float64 `json:"servingSeconds"`
	// DepsInstallSeconds is from serving until the dependencies are in.
	DepsInstallSeconds float64 `json:"depsInstallSeconds"`
	DepsSource         string  `json:"depsSource"` // Install on a cache miss
	CommandSeconds     float64 `json:"commandSeconds"`
	CommandOutput      string  `json:"commandOutput"` // its last lines, to show it did something
}

// Cycle is one suspend and wake.
type Cycle struct {
	N              int     `json:"n"`
	Hinted         bool    `json:"hinted"`
	SuspendSeconds float64 `json:"suspendSeconds"` // the suspend call until Suspended
	BundleBytes    int64   `json:"bundleBytes"`
	// Wake stages, from the wake request (the hint, or else the command).
	FilesReadySeconds float64 `json:"filesReadySeconds"`
	ServingSeconds    float64 `json:"servingSeconds"`
	DepsReadySeconds  float64 `json:"depsReadySeconds"`
	DepsSource        string  `json:"depsSource"`
	// AgentWaitSeconds is how long the command was held: for the wake,
	// then for the dependencies.
	AgentWaitSeconds float64  `json:"agentWaitSeconds"`
	CommandSeconds   float64  `json:"commandSeconds"` // sent until answered
	S3               Requests `json:"s3"`
	Sidecar          Usage    `json:"sidecar"` // this wake's pod, through the command
	Agent            Usage    `json:"agent"`
	Node             string   `json:"node"`
}

// RepoSummary summarizes the measured cycles.
type RepoSummary struct {
	SuspendSeconds    Summary `json:"suspendSeconds"`
	BundleBytes       Summary `json:"bundleBytes"`
	FilesReadySeconds Summary `json:"filesReadySeconds"`
	DepsReadySeconds  Summary `json:"depsReadySeconds"`
	// ServingSeconds and AgentWaitSeconds are by mode: hinted, unhinted
	// and all.
	ServingSeconds      map[string]Summary `json:"servingSeconds"`
	AgentWaitSeconds    map[string]Summary `json:"agentWaitSeconds"`
	S3PutPerCycle       Summary            `json:"s3PutPerCycle"`
	S3GetPerCycle       Summary            `json:"s3GetPerCycle"`
	SidecarCPUSeconds   Summary            `json:"sidecarCpuSeconds"`
	SidecarPeakRSSBytes Summary            `json:"sidecarPeakRssBytes"`
	AgentCPUSeconds     Summary            `json:"agentCpuSeconds"`
	AgentPeakRSSBytes   Summary            `json:"agentPeakRssBytes"`
	CacheHits           int                `json:"cacheHits"` // cycles whose dependencies came from the cache
}

// SimInputs are the averages the cost simulation runs on.
type SimInputs struct {
	WakeServeSeconds    float64 `json:"wakeServeSeconds"`
	WakeDepsSeconds     float64 `json:"wakeDepsSeconds"`
	SuspendSeconds      float64 `json:"suspendSeconds"`
	PutRequestsPerCycle float64 `json:"putRequestsPerCycle"`
	GetRequestsPerCycle float64 `json:"getRequestsPerCycle"`
	SnapshotBytes       float64 `json:"snapshotBytes"`
	DepArchiveBytes     float64 `json:"depArchiveBytes"`
	SandboxUsdPerHour   float64 `json:"sandboxUsdPerHour"`
}

// CostSim is SimInputs per repository and their mean.
type CostSim struct {
	Mean    SimInputs            `json:"mean"`
	PerRepo map[string]SimInputs `json:"perRepo"`
}

func summarize(cs []Cycle) RepoSummary {
	pick := func(f func(Cycle) float64, keep func(Cycle) bool) Summary {
		var xs []float64
		for _, c := range cs {
			if keep == nil || keep(c) {
				xs = append(xs, f(c))
			}
		}
		return Summarize(xs)
	}
	hinted := func(c Cycle) bool { return c.Hinted }
	unhinted := func(c Cycle) bool { return !c.Hinted }
	byMode := func(f func(Cycle) float64) map[string]Summary {
		return map[string]Summary{"hinted": pick(f, hinted), "unhinted": pick(f, unhinted), "all": pick(f, nil)}
	}
	s := RepoSummary{
		SuspendSeconds:      pick(func(c Cycle) float64 { return c.SuspendSeconds }, nil),
		BundleBytes:         pick(func(c Cycle) float64 { return float64(c.BundleBytes) }, nil),
		FilesReadySeconds:   pick(func(c Cycle) float64 { return c.FilesReadySeconds }, nil),
		DepsReadySeconds:    pick(func(c Cycle) float64 { return c.DepsReadySeconds }, nil),
		ServingSeconds:      byMode(func(c Cycle) float64 { return c.ServingSeconds }),
		AgentWaitSeconds:    byMode(func(c Cycle) float64 { return c.AgentWaitSeconds }),
		S3PutPerCycle:       pick(func(c Cycle) float64 { return c.S3.PutClass }, nil),
		S3GetPerCycle:       pick(func(c Cycle) float64 { return c.S3.GetClass }, nil),
		SidecarCPUSeconds:   pick(func(c Cycle) float64 { return c.Sidecar.CPUSeconds }, nil),
		SidecarPeakRSSBytes: pick(func(c Cycle) float64 { return float64(c.Sidecar.PeakRSSBytes) }, nil),
		AgentCPUSeconds:     pick(func(c Cycle) float64 { return c.Agent.CPUSeconds }, nil),
		AgentPeakRSSBytes:   pick(func(c Cycle) float64 { return float64(c.Agent.PeakRSSBytes) }, nil),
	}
	for _, c := range cs {
		if c.DepsSource == "Cache" {
			s.CacheHits++
		}
	}
	return s
}

func costSim(repos []RepoResult) CostSim {
	out := CostSim{PerRepo: map[string]SimInputs{}}
	var all []SimInputs
	for _, r := range repos {
		in := SimInputs{
			WakeServeSeconds:    r.Summary.ServingSeconds["all"].Avg,
			WakeDepsSeconds:     r.Summary.DepsReadySeconds.Avg,
			SuspendSeconds:      r.Summary.SuspendSeconds.Avg,
			PutRequestsPerCycle: r.Summary.S3PutPerCycle.Avg,
			GetRequestsPerCycle: r.Summary.S3GetPerCycle.Avg,
			SnapshotBytes:       r.Summary.BundleBytes.Avg,
			DepArchiveBytes:     float64(r.DepArchiveBytes),
			SandboxUsdPerHour:   r.SandboxUsdPerHour,
		}
		out.PerRepo[r.Name] = in
		all = append(all, in)
	}
	field := func(f func(SimInputs) float64) float64 {
		xs := make([]float64, len(all))
		for i, in := range all {
			xs[i] = f(in)
		}
		return mean(xs)
	}
	out.Mean = SimInputs{
		WakeServeSeconds:    field(func(s SimInputs) float64 { return s.WakeServeSeconds }),
		WakeDepsSeconds:     field(func(s SimInputs) float64 { return s.WakeDepsSeconds }),
		SuspendSeconds:      field(func(s SimInputs) float64 { return s.SuspendSeconds }),
		PutRequestsPerCycle: field(func(s SimInputs) float64 { return s.PutRequestsPerCycle }),
		GetRequestsPerCycle: field(func(s SimInputs) float64 { return s.GetRequestsPerCycle }),
		SnapshotBytes:       field(func(s SimInputs) float64 { return s.SnapshotBytes }),
		DepArchiveBytes:     field(func(s SimInputs) float64 { return s.DepArchiveBytes }),
		SandboxUsdPerHour:   field(func(s SimInputs) float64 { return s.SandboxUsdPerHour }),
	}
	return out
}
