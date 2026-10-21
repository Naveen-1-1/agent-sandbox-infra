package bench

import (
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Counters are the controller's object storage counters at one moment,
// summed over its replicas (each runs a broker).
type Counters struct {
	// Requests by "caller op": the controller's own calls.
	Requests map[string]float64
	// Delegated by "op kind": presigned URLs, each one request from a pod.
	Delegated map[string]float64
	// RunningUsdPerHour is what the running workspaces cost now.
	RunningUsdPerHour float64
}

// ParseMetrics reads and adds up the controllers' /metrics pages.
func ParseMetrics(pages []string) (Counters, error) {
	c := Counters{Requests: map[string]float64{}, Delegated: map[string]float64{}}
	for _, page := range pages {
		p := expfmt.NewTextParser(model.UTF8Validation)
		fams, err := p.TextToMetricFamilies(strings.NewReader(page))
		if err != nil {
			return Counters{}, err
		}
		add := func(name string, into map[string]float64, labels ...string) {
			f := fams[name]
			if f == nil {
				return
			}
			for _, m := range f.GetMetric() {
				into[labelKey(m, labels)] += m.GetCounter().GetValue()
			}
		}
		add("sbx_object_store_requests_total", c.Requests, "caller", "op")
		add("sbx_object_store_delegated_requests_total", c.Delegated, "op", "kind")
		if f := fams["sbx_cost_usd_per_hour"]; f != nil {
			for _, m := range f.GetMetric() {
				if labelKey(m, []string{"phase"}) == "Running" {
					c.RunningUsdPerHour = max(c.RunningUsdPerHour, m.GetGauge().GetValue()) // computed alike on every replica
				}
			}
		}
	}
	return c, nil
}

func labelKey(m *dto.Metric, names []string) string {
	vals := make([]string, len(names))
	for _, l := range m.GetLabel() {
		for i, n := range names {
			if l.GetName() == n {
				vals[i] = l.GetValue()
			}
		}
	}
	return strings.Join(vals, " ")
}

// Requests counts requests to object storage in one cycle, priced the way
// S3 bills them: PUT, COPY, POST and LIST in one class, GET, HEAD and the
// rest in the other. DELETE is free.
type Requests struct {
	PutClass float64            `json:"putClass"`
	GetClass float64            `json:"getClass"`
	ByOp     map[string]float64 `json:"byOp"` // "controller put", "pods get deps", ...
}

// Since is what was requested between prev and c, leaving out the usage
// poller's periodic listing.
func (c Counters) Since(prev Counters) Requests {
	r := Requests{ByOp: map[string]float64{}}
	for k, v := range c.Requests {
		d := v - prev.Requests[k]
		caller, op, _ := strings.Cut(k, " ")
		if d <= 0 || caller == "usage" {
			continue
		}
		r.ByOp[caller+" "+op] += d
		switch op {
		case "put", "list":
			r.PutClass += d
		case "get", "head":
			r.GetClass += d
		}
	}
	for k, v := range c.Delegated {
		d := v - prev.Delegated[k]
		if d <= 0 {
			continue
		}
		r.ByOp["pods "+k] += d
		if strings.HasPrefix(k, "put ") {
			r.PutClass += d
		} else {
			r.GetClass += d
		}
	}
	return r
}
