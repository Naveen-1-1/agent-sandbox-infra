package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/cost"
)

// CostSource prices the fleet as it is now.
type CostSource func(ctx context.Context) (cost.Summary, error)

// Storage measures what the platform keeps in object storage.
type Storage struct {
	Bytes   *prometheus.GaugeVec
	Objects *prometheus.GaugeVec
	Cost    prometheus.Gauge
}

// NewCost registers the cost gauges, computed at scrape time from source.
func NewCost(reg prometheus.Registerer, source CostSource) {
	reg.MustRegister(&costCollector{
		source: source,
		byPhase: prometheus.NewDesc("sbx_cost_usd_per_hour",
			"What the workspaces cost per hour now at list prices, by phase: pod and disk while awake, the snapshot while asleep.",
			[]string{"phase"}, nil),
		saved: prometheus.NewDesc("sbx_cost_saved_usd_per_hour",
			"What the sleeping workspaces would cost per hour awake, less what they cost asleep.", nil, nil),
	})
}

// NewStorage registers the object storage gauges; a poller sets them.
func NewStorage(reg prometheus.Registerer) *Storage {
	m := &Storage{
		Bytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sbx_object_store_bytes",
			Help: "Bytes in object storage by kind: snapshots, bases, deps.",
		}, []string{"kind"}),
		Objects: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sbx_object_store_objects",
			Help: "Objects in object storage by kind.",
		}, []string{"kind"}),
		Cost: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sbx_object_store_cost_usd_per_hour",
			Help: "What everything in object storage costs per hour at list prices.",
		}),
	}
	reg.MustRegister(m.Bytes, m.Objects, m.Cost)
	return m
}

type costCollector struct {
	source         CostSource
	byPhase, saved *prometheus.Desc
}

func (c *costCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.byPhase; ch <- c.saved }

func (c *costCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.source(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.byPhase, err)
		return
	}
	for phase, usd := range s.ByPhase {
		ch <- prometheus.MustNewConstMetric(c.byPhase, prometheus.GaugeValue, usd, phase)
	}
	ch <- prometheus.MustNewConstMetric(c.saved, prometheus.GaugeValue, s.Saved)
}
