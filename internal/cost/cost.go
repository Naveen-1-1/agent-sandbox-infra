// Package cost prices what a workspace holds right now: a running one pays
// for its pod's CPU and memory and for its disk; a suspended one only for
// the snapshot it keeps in object storage. The prices are list prices from
// configuration, so the numbers show the scale of the saving, not a bill.
package cost

// HoursPerMonth converts monthly storage prices to hourly ones.
const HoursPerMonth = 730

// Prices are list prices for the resources a workspace holds.
type Prices struct {
	CPUCoreHour    float64 `json:"cpuCoreHour"`
	MemoryGiBHour  float64 `json:"memoryGiBHour"`
	DiskGiBMonth   float64 `json:"diskGiBMonth"`
	ObjectGiBMonth float64 `json:"objectGiBMonth"`
}

// Defaults are on-demand list prices (AWS us-east-1: Fargate per vCPU and
// GiB, gp3 volumes, S3 Standard), to be overridden.
var Defaults = Prices{CPUCoreHour: 0.04048, MemoryGiBHour: 0.004445, DiskGiBMonth: 0.08, ObjectGiBMonth: 0.023}

const gib = 1 << 30

// Footprint is what a workspace's pod and disk ask for while it is awake.
type Footprint struct {
	CPUCores  float64
	MemoryGiB float64
	DiskGiB   float64
}

// Awake is the hourly cost of a workspace with this footprint while awake.
func (p Prices) Awake(f Footprint) float64 {
	return f.CPUCores*p.CPUCoreHour + f.MemoryGiB*p.MemoryGiBHour + f.DiskGiB*p.DiskGiBMonth/HoursPerMonth
}

// Stored is the hourly cost of keeping bytes in object storage.
func (p Prices) Stored(bytes int64) float64 {
	return float64(bytes) / gib * p.ObjectGiBMonth / HoursPerMonth
}

// Workspace is one workspace as priced.
type Workspace struct {
	Phase string
	// Awake is true while the workspace holds a pod or a disk.
	Awake bool
	// StoredBytes is its latest snapshot in object storage.
	StoredBytes int64
}

// Summary is what the fleet costs per hour now.
type Summary struct {
	ByPhase map[string]float64
	// Saved is what the sleeping workspaces would cost if they were awake,
	// less what they cost asleep.
	Saved float64
}

// Summarize prices every workspace, all with the same footprint.
func Summarize(p Prices, f Footprint, all []Workspace) Summary {
	s := Summary{ByPhase: map[string]float64{}}
	awake := p.Awake(f)
	for _, w := range all {
		if w.Awake {
			s.ByPhase[w.Phase] += awake
			continue
		}
		stored := p.Stored(w.StoredBytes)
		s.ByPhase[w.Phase] += stored
		s.Saved += awake - stored
	}
	return s
}
