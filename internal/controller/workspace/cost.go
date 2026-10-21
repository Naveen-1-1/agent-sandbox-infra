package workspace

import (
	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/cost"
)

const gib = 1 << 30

// Footprint is what one awake workspace asks for: its pod's requests and
// its disk.
func (c Config) Footprint() cost.Footprint {
	c.defaults()
	cpu, mem := podspec.Requests(c.Resources)
	return cost.Footprint{
		CPUCores:  cpu.AsApproximateFloat64(),
		MemoryGiB: mem.AsApproximateFloat64() / gib,
		DiskGiB:   c.StorageSize.AsApproximateFloat64() / gib,
	}
}

// Priced says what ws holds for pricing: a pod and a disk from the moment
// one is created until the suspend has torn them down, else only its
// latest snapshot.
func Priced(ws *sbxv1.Workspace) cost.Workspace {
	w := cost.Workspace{Phase: string(ws.Status.Phase)}
	switch ws.Status.Phase {
	case sbxv1.PhaseProvisioning, sbxv1.PhaseWaking, sbxv1.PhaseRunning, sbxv1.PhaseSuspending, sbxv1.PhaseTearingDown:
		w.Awake = true
	}
	if s := ws.Status.LastSnapshot; s != nil && !w.Awake {
		w.StoredBytes = s.Bytes
	}
	return w
}
