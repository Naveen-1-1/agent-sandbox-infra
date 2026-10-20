// Package metrics defines the platform's Prometheus metrics. Every metric is
// named sbx_* and is registered on the registry the binary passes in, so
// tests can use a fresh registry.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Snapshot failure reasons, the values of the "reason" label.
const (
	ReasonSecretDetected = "secret_detected"
	ReasonTooLarge       = "too_large"
	ReasonUploadFailed   = "upload_failed"
	ReasonVerifyFailed   = "verify_failed"
	ReasonGitFailed      = "git_failed"
	ReasonStale          = "stale"
	ReasonInternal       = "internal"
)

// SnapshotReasons lists every failure reason, so their series exist at zero.
var SnapshotReasons = []string{
	ReasonSecretDetected, ReasonTooLarge, ReasonUploadFailed, ReasonVerifyFailed,
	ReasonGitFailed, ReasonStale, ReasonInternal,
}

// bytesBuckets spans 1 KiB to 1 GiB in factors of 4.
var bytesBuckets = prometheus.ExponentialBuckets(1<<10, 4, 11)

// Snapshot measures snapshots as the controller sees them.
type Snapshot struct {
	BundleBytes prometheus.Histogram
	NewBytes    prometheus.Histogram
	Duration    prometheus.Histogram
	Failures    *prometheus.CounterVec
}

// NewSnapshot creates and registers the snapshot metrics.
func NewSnapshot(reg prometheus.Registerer) *Snapshot {
	m := &Snapshot{
		BundleBytes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sbx_snapshot_bundle_bytes",
			Help:    "Size of snapshot bundles pushed to S3.",
			Buckets: bytesBuckets,
		}),
		NewBytes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sbx_snapshot_new_bytes",
			Help:    "Uncompressed size of the files a snapshot adds on top of the base commit.",
			Buckets: bytesBuckets,
		}),
		Duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sbx_snapshot_duration_seconds",
			Help:    "Time from freezing the workload to a verified bundle in S3.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 12), // 50ms .. ~100s
		}),
		Failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_snapshot_failures_total",
			Help: "Snapshot attempts that failed, by reason. The workspace keeps running.",
		}, []string{"reason"}),
	}
	reg.MustRegister(m.BundleBytes, m.NewBytes, m.Duration, m.Failures)
	for _, r := range SnapshotReasons {
		m.Failures.WithLabelValues(r)
	}
	return m
}

// Succeeded records a verified snapshot.
func (m *Snapshot) Succeeded(bundleBytes, newBytes int64, took time.Duration) {
	m.BundleBytes.Observe(float64(bundleBytes))
	m.NewBytes.Observe(float64(newBytes))
	m.Duration.Observe(took.Seconds())
}

// Failed records a failed snapshot attempt.
func (m *Snapshot) Failed(reason string) {
	m.Failures.WithLabelValues(reason).Inc()
}
