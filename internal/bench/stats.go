package bench

import (
	"math"
	"slices"
)

// Summary describes a set of measurements.
type Summary struct {
	N   int     `json:"n"`
	Avg float64 `json:"avg"`
	P95 float64 `json:"p95"` // nearest rank: with few samples, close to the max
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Summarize summarizes xs; an empty set is all zeros.
func Summarize(xs []float64) Summary {
	if len(xs) == 0 {
		return Summary{}
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	rank := int(math.Ceil(0.95*float64(len(s)))) - 1
	return Summary{N: len(s), Avg: sum / float64(len(s)), P95: s[rank], Min: s[0], Max: s[len(s)-1]}
}

// mean averages xs, or is 0 for none.
func mean(xs []float64) float64 { return Summarize(xs).Avg }
