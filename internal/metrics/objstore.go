package metrics

import "github.com/prometheus/client_golang/prometheus"

// ObjectStore counts requests to object storage: the controller's own, and
// the ones pods make with URLs the broker presigned for them.
type ObjectStore struct {
	Requests  *prometheus.CounterVec
	Delegated *prometheus.CounterVec
}

// NewObjectStore registers the object storage request counters.
func NewObjectStore(reg prometheus.Registerer) *ObjectStore {
	m := &ObjectStore{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_object_store_requests_total",
			Help: "Requests the controller made to object storage, by caller (controller, broker, usage) and operation (put, get, head, delete, list).",
		}, []string{"caller", "op"}),
		Delegated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sbx_object_store_delegated_requests_total",
			Help: "Presigned URLs the broker issued, by operation (put, get) and kind (snapshots, bases, deps); each is one request a pod makes to object storage.",
		}, []string{"op", "kind"}),
	}
	reg.MustRegister(m.Requests, m.Delegated)
	return m
}
