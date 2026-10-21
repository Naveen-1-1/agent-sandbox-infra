package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Gateway rejection reasons, the values of the "reason" label.
const (
	RejectQueueFull   = "queue_full"
	RejectWakeTimeout = "wake_timeout"
	RejectOff         = "workspace_off"
	RejectAuth        = "unauthorized"
)

// Gateway measures request holding and wake requests.
type Gateway struct {
	HoldDuration *prometheus.HistogramVec
	Rejected     *prometheus.CounterVec
	WakeRequests *prometheus.CounterVec
	HintLead     prometheus.Histogram
	Requests     *prometheus.CounterVec
}

// NewGateway creates and registers the gateway metrics. held reports how
// many requests are waiting right now.
func NewGateway(reg prometheus.Registerer, held func() int) *Gateway {
	m := &Gateway{
		HoldDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sbx_gateway_hold_duration_seconds",
			Help:    "How long requests were held while their workspace woke, by outcome. This is the wake latency users actually see.",
			Buckets: prometheus.ExponentialBuckets(0.05, 1.8, 14), // 50ms .. ~110s
		}, []string{"outcome"}),
		Rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_gateway_rejected_requests_total",
			Help: "Requests the gateway refused, by reason.",
		}, []string{"reason"}),
		WakeRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_gateway_wake_requests_total",
			Help: "Wakes this gateway asked for (bumps of spec.wake.seq), by trigger.",
		}, []string{"trigger"}),
		HintLead: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sbx_wake_hint_lead_seconds",
			Help:    "Time between a wake-hint that started a wake and the first real request: how much of the wake was hidden behind the agent's thinking.",
			Buckets: prometheus.ExponentialBuckets(0.1, 1.8, 12), // 0.1s .. ~65s
		}),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_gateway_requests_total",
			Help: "API requests by route and status code.",
		}, []string{"route", "code"}),
	}
	reg.MustRegister(m.HoldDuration, m.Rejected, m.WakeRequests, m.HintLead, m.Requests)
	for _, r := range []string{RejectQueueFull, RejectWakeTimeout, RejectOff, RejectAuth} {
		m.Rejected.WithLabelValues(r)
	}
	if held != nil {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "sbx_gateway_held_requests",
			Help: "Requests currently held while their workspace wakes.",
		}, func() float64 { return float64(held()) }))
	}
	return m
}

// ObserveHold records one finished hold.
func (m *Gateway) ObserveHold(outcome string, waited time.Duration) {
	m.HoldDuration.WithLabelValues(outcome).Observe(waited.Seconds())
}
