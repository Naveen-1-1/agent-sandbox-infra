package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// OpCounter reports operations running and waiting for a slot, by class.
type OpCounter func() (running, waiting map[string]int)

// Sleep measures sleep-policy verdicts and the operation queue.
type Sleep struct {
	Decisions *prometheus.CounterVec
}

// NewSleep creates and registers the sleep metrics. ops, if set, backs the
// operation gauges at scrape time.
func NewSleep(reg prometheus.Registerer, ops OpCounter, classes []string) *Sleep {
	m := &Sleep{
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_policy_decisions_total",
			Help: "Sleep-policy verdicts, counted when a workspace's verdict changes, by policy, decision and reason.",
		}, []string{"policy", "decision", "reason"}),
	}
	reg.MustRegister(m.Decisions)
	if ops != nil {
		reg.MustRegister(&opCollector{
			count:   ops,
			classes: classes,
			running: prometheus.NewDesc("sbx_ops_running", "Wakes and suspends in progress, by class.", []string{"class"}, nil),
			waiting: prometheus.NewDesc("sbx_ops_waiting", "Wakes and suspends waiting for a slot, by class.", []string{"class"}, nil),
		})
	}
	return m
}

type opCollector struct {
	count            OpCounter
	classes          []string
	running, waiting *prometheus.Desc
}

func (c *opCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.running; ch <- c.waiting }

func (c *opCollector) Collect(ch chan<- prometheus.Metric) {
	running, waiting := c.count()
	for _, class := range c.classes {
		ch <- prometheus.MustNewConstMetric(c.running, prometheus.GaugeValue, float64(running[class]), class)
		ch <- prometheus.MustNewConstMetric(c.waiting, prometheus.GaugeValue, float64(waiting[class]), class)
	}
}
