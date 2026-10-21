package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Wake stages, the values of the "stage" label.
const (
	StageFilesReady = "files_ready" // files restored; the workload is starting
	StageReady      = "ready"       // the workload is serving
	StageDepsReady  = "deps_ready"  // dependency directories restored
)

// Suspend results, the values of the "result" label.
const (
	SuspendCommitted = "committed" // snapshot committed; pod and disk deleted
	SuspendAborted   = "aborted"   // a wake or new activity won the race
	SuspendFailed    = "failed"    // snapshot failed; the workspace keeps running
)

// PhaseCounter lists how many workspaces are in each phase right now.
type PhaseCounter func(ctx context.Context) (map[string]int, error)

// Workspace measures workspace lifecycles.
type Workspace struct {
	PhaseDuration *prometheus.HistogramVec
	WakeDuration  *prometheus.HistogramVec
	RestoreBytes  prometheus.Histogram
	Wakes         *prometheus.CounterVec
	Suspends      *prometheus.CounterVec

	DepsRestores    *prometheus.CounterVec
	DepsBytes       prometheus.Histogram
	DepsUploads     *prometheus.CounterVec
	DepsUploadBytes prometheus.Histogram
}

// Dependency restore sources and upload results, the label values.
const (
	DepsSourceCache   = "cache"   // restored from the shared cache
	DepsSourceInstall = "install" // a cache miss: installed
	DepsSourcePresent = "present" // already in the workspace
	DepsSourceFailed  = "failed"

	DepsUploaded      = "uploaded"
	DepsUploadSkipped = "skipped"
	DepsUploadFailed  = "failed"
)

// NewWorkspace creates and registers the workspace metrics. count, if set,
// backs the sbx_workspaces gauge at scrape time.
func NewWorkspace(reg prometheus.Registerer, count PhaseCounter) *Workspace {
	m := &Workspace{
		PhaseDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sbx_workspace_phase_duration_seconds",
			Help:    "Time workspaces spent in a phase, observed when they leave it.",
			Buckets: prometheus.ExponentialBuckets(0.5, 3, 12), // 0.5s .. ~1 day
		}, []string{"phase"}),
		WakeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sbx_wake_duration_seconds",
			Help:    "Time from a wake request to each wake stage.",
			Buckets: prometheus.ExponentialBuckets(0.25, 1.6, 14), // 0.25s .. ~110s
		}, []string{"stage", "trigger"}),
		RestoreBytes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sbx_restore_bytes",
			Help:    "Bytes downloaded to restore a workspace's files on wake.",
			Buckets: bytesBuckets,
		}),
		Wakes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_wakes_total",
			Help: "Wakes started, by trigger.",
		}, []string{"trigger"}),
		Suspends: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_suspends_total",
			Help: "Suspend attempts by result.",
		}, []string{"result"}),
	}
	m.DepsRestores = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sbx_deps_restores_total",
		Help: "Dependency directory restores by profile and source; cache over cache+install is the hit rate.",
	}, []string{"profile", "source"})
	m.DepsBytes = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "sbx_deps_restore_bytes",
		Help:    "Bytes downloaded to restore a dependency directory from the cache.",
		Buckets: bytesBuckets,
	})
	m.DepsUploads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sbx_deps_uploads_total",
		Help: "Dependency archives offered to the cache during a suspend, by result.",
	}, []string{"result"})
	m.DepsUploadBytes = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "sbx_deps_upload_bytes",
		Help:    "Size of dependency archives uploaded to the cache.",
		Buckets: bytesBuckets,
	})
	reg.MustRegister(m.PhaseDuration, m.WakeDuration, m.RestoreBytes, m.Wakes, m.Suspends,
		m.DepsRestores, m.DepsBytes, m.DepsUploads, m.DepsUploadBytes)
	for _, r := range []string{SuspendCommitted, SuspendAborted, SuspendFailed} {
		m.Suspends.WithLabelValues(r)
	}
	if count != nil {
		reg.MustRegister(&phaseCollector{count: count, desc: prometheus.NewDesc(
			"sbx_workspaces", "Workspaces in each phase.", []string{"phase"}, nil)})
	}
	return m
}

// ObservePhase records time spent in a phase that just ended.
func (m *Workspace) ObservePhase(phase string, since time.Time, now time.Time) {
	if phase == "" || since.IsZero() {
		return
	}
	m.PhaseDuration.WithLabelValues(phase).Observe(now.Sub(since).Seconds())
}

type phaseCollector struct {
	count PhaseCounter
	desc  *prometheus.Desc
}

func (c *phaseCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *phaseCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	counts, err := c.count(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc, err)
		return
	}
	for phase, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), phase)
	}
}
