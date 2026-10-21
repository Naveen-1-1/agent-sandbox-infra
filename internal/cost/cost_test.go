package cost_test

import (
	"math"
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/cost"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestPrices(t *testing.T) {
	p := cost.Prices{CPUCoreHour: 0.04, MemoryGiBHour: 0.005, DiskGiBMonth: 0.073, ObjectGiBMonth: 0.0219}
	if got := p.Awake(cost.Footprint{CPUCores: 2, MemoryGiB: 4, DiskGiB: 10}); !near(got, 0.08+0.02+0.001) {
		t.Errorf("awake = %v", got)
	}
	if got := p.Stored(1 << 30); !near(got, 0.00003) {
		t.Errorf("1 GiB stored = %v", got)
	}
}

func TestSummarize(t *testing.T) {
	p := cost.Defaults
	f := cost.Footprint{CPUCores: 1, MemoryGiB: 2, DiskGiB: 5}
	awake := p.Awake(f)
	s := cost.Summarize(p, f, []cost.Workspace{
		{Phase: "Running", Awake: true},
		{Phase: "Running", Awake: true},
		{Phase: "Waking", Awake: true},
		{Phase: "Suspended", StoredBytes: 1 << 20},
		{Phase: "Suspended", StoredBytes: 0},
	})
	if !near(s.ByPhase["Running"], 2*awake) || !near(s.ByPhase["Waking"], awake) {
		t.Errorf("by phase = %v", s.ByPhase)
	}
	stored := p.Stored(1 << 20)
	if !near(s.ByPhase["Suspended"], stored) || !near(s.Saved, 2*awake-stored) {
		t.Errorf("suspended %v saved %v", s.ByPhase["Suspended"], s.Saved)
	}
	if s.ByPhase["Suspended"] > awake/1000 {
		t.Errorf("a sleeping workspace costs %v/h against %v/h awake; the point is that it's tiny", s.ByPhase["Suspended"], awake)
	}
}
