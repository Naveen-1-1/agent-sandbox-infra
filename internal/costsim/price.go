package costsim

import "github.com/Naveen-1-1/agent-sandbox-infra/internal/cost"

// Prices turn a Result into dollars.
type Prices struct {
	AwakeUsdPerHour float64 `json:"awakeUsdPerHour"` // a sandbox's pod and disk
	ObjectGiBMonth  float64 `json:"objectGiBMonth"`  // its snapshot while asleep
	PutPer1k        float64 `json:"putPer1k"`        // S3 PUT, COPY, POST, LIST
	GetPer1k        float64 `json:"getPer1k"`        // S3 GET, HEAD and the rest
}

// ReferenceUsdPerHour is one m7g.large on demand (us-east-1): the price
// every config is simulated at before scaling to a measured sandbox.
const ReferenceUsdPerHour = 0.0816

// Reference prices: AWS us-east-1 list prices.
var Reference = Prices{AwakeUsdPerHour: ReferenceUsdPerHour, ObjectGiBMonth: 0.023, PutPer1k: 0.005, GetPer1k: 0.0004}

// Usage is what one sleep and wake cost in storage and requests.
type Usage struct {
	StoredBytes         int64   // snapshot plus dependency archive, kept while asleep
	PutRequestsPerCycle float64 // charged per suspend
	GetRequestsPerCycle float64 // charged per wake
}

// Cost is a priced Result.
type Cost struct {
	AwakeUsd    float64 `json:"awakeUsd"`
	StorageUsd  float64 `json:"storageUsd"`
	RequestsUsd float64 `json:"requestsUsd"`
	TotalUsd    float64 `json:"totalUsd"`
}

func (c *Cost) add(o Cost) {
	c.AwakeUsd += o.AwakeUsd
	c.StorageUsd += o.StorageUsd
	c.RequestsUsd += o.RequestsUsd
	c.TotalUsd += o.TotalUsd
}

// Of prices a Result.
func (p Prices) Of(r Result, u Usage) Cost {
	c := Cost{
		AwakeUsd:   r.AwakeBilled().Hours() * p.AwakeUsdPerHour,
		StorageUsd: r.Asleep.Hours() * cost.Prices{ObjectGiBMonth: p.ObjectGiBMonth}.Stored(u.StoredBytes),
		RequestsUsd: float64(r.Suspends)*u.PutRequestsPerCycle*p.PutPer1k/1000 +
			float64(r.Wakes())*u.GetRequestsPerCycle*p.GetPer1k/1000,
	}
	c.TotalUsd = c.AwakeUsd + c.StorageUsd + c.RequestsUsd
	return c
}
