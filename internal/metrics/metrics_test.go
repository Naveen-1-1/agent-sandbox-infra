package metrics_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestSnapshotMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.NewSnapshot(reg)
	m.Succeeded(4096, 10_000, 1500*time.Millisecond)
	m.Failed(metrics.ReasonSecretDetected)
	m.Failed(metrics.ReasonSecretDetected)

	if got := testutil.ToFloat64(m.Failures.WithLabelValues(metrics.ReasonSecretDetected)); got != 2 {
		t.Errorf("secret_detected failures = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.Failures.WithLabelValues(metrics.ReasonTooLarge)); got != 0 {
		t.Errorf("too_large failures = %v, want a zero series", got)
	}
	if n := testutil.CollectAndCount(m.Duration); n != 1 {
		t.Errorf("duration collected %d series, want 1", n)
	}
}

func TestObjectStoreMetrics(t *testing.T) {
	m := metrics.NewObjectStore(prometheus.NewRegistry())
	m.Requests.WithLabelValues("broker", "head").Inc()
	m.Delegated.WithLabelValues("put", "deps").Add(2)
	if got := testutil.ToFloat64(m.Requests.WithLabelValues("broker", "head")); got != 1 {
		t.Errorf("broker heads = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.Delegated.WithLabelValues("put", "deps")); got != 2 {
		t.Errorf("delegated deps puts = %v, want 2", got)
	}
}

// TestNamingConvention keeps every metric in the sbx_ namespace with help text.
func TestNamingConvention(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics.NewSnapshot(reg)
	st := metrics.NewObjectStore(reg)
	st.Requests.WithLabelValues("controller", "get").Inc()
	st.Delegated.WithLabelValues("get", "snapshots").Inc()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "sbx_") {
			t.Errorf("metric %s is not prefixed sbx_", f.GetName())
		}
		if f.GetHelp() == "" {
			t.Errorf("metric %s has no help text", f.GetName())
		}
	}
}
