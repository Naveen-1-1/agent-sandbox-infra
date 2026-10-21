package costsim

import (
	"math"
	"slices"
	"sort"
)

// Class is what a span of a session was spent on.
type Class string

// Classes, in the order they win when spans overlap.
const (
	ClassTail        Class = "sessionTail" // after the last event, until the sandbox would be deleted
	ClassToolRunning Class = "toolRunning" // a sandbox call is running
	ClassHumanWait   Class = "humanWait"   // waiting for a person: between turns, or a question
	ClassAIThinking  Class = "aiThinking"  // inside a turn with nothing running in the sandbox
)

// Classes lists them for reports.
var Classes = []Class{ClassToolRunning, ClassAIThinking, ClassHumanWait, ClassTail}

// Gap is a maximal span of one class.
type Gap struct {
	Class Class
	S, E  int64
}

// Gaps cuts a session from its start until tail after its last instant
// into gaps; every instant belongs to exactly one.
func Gaps(tl Timeline, tail int64) []Gap {
	if tl.Start > tl.Last {
		return nil
	}
	busy := union(callIntervals(tl.Calls))
	human := slices.Clone(tl.HumanWindows)
	for k := 0; k+1 < len(tl.Turns); k++ {
		human = append(human, Interval{tl.Turns[k].End, tl.Turns[k+1].Start})
	}
	if len(tl.Turns) > 0 && tl.Turns[0].Start > tl.Start {
		human = append(human, Interval{tl.Start, tl.Turns[0].Start})
	}
	human = union(human)

	cuts := []int64{tl.Start, tl.Last, tl.Last + tail}
	for _, iv := range append(slices.Clone(busy), human...) {
		cuts = append(cuts, iv.S, iv.E)
	}
	for _, t := range tl.Turns {
		cuts = append(cuts, t.Start, t.End)
	}
	slices.Sort(cuts)
	cuts = slices.Compact(cuts)

	var out []Gap
	for i := 0; i+1 < len(cuts); i++ {
		a, b := cuts[i], cuts[i+1]
		if a < tl.Start || b > tl.Last+tail || a == b {
			continue
		}
		mid := a + (b-a)/2
		c := ClassAIThinking
		switch {
		case a >= tl.Last:
			c = ClassTail
		case covers(busy, mid):
			c = ClassToolRunning
		case covers(human, mid):
			c = ClassHumanWait
		}
		if n := len(out); n > 0 && out[n-1].Class == c && out[n-1].E == a {
			out[n-1].E = b
			continue
		}
		out = append(out, Gap{Class: c, S: a, E: b})
	}
	return out
}

func callIntervals(cs []Call) []Interval {
	out := make([]Interval, len(cs))
	for i, c := range cs {
		out[i] = Interval{c.S, c.E}
	}
	return out
}

// union merges intervals into disjoint ones, sorted, dropping empty ones.
func union(ivs []Interval) []Interval {
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].S < ivs[j].S })
	var out []Interval
	for _, iv := range ivs {
		if iv.E <= iv.S {
			continue
		}
		if n := len(out); n > 0 && iv.S <= out[n-1].E {
			out[n-1].E = max(out[n-1].E, iv.E)
			continue
		}
		out = append(out, iv)
	}
	return out
}

// covers reports whether t lies inside one of the disjoint sorted ivs.
func covers(ivs []Interval, t int64) bool {
	i := sort.Search(len(ivs), func(i int) bool { return ivs[i].E > t })
	return i < len(ivs) && ivs[i].S <= t
}

// GapStats sums the gaps of one class.
type GapStats struct {
	Hours float64 `json:"hours"`
	Share float64 `json:"share"` // of the sessions' time from start to last event
	Gaps  int     `json:"gaps"`
	P50   float64 `json:"p50Seconds"`
	P90   float64 `json:"p90Seconds"`
}

// SummarizeGaps adds up gaps by class over many sessions.
func SummarizeGaps(all [][]Gap) map[Class]GapStats {
	lengths := map[Class][]float64{}
	var active float64
	for _, gaps := range all {
		for _, g := range gaps {
			s := float64(g.E-g.S) / 1e6
			lengths[g.Class] = append(lengths[g.Class], s)
			if g.Class != ClassTail {
				active += s
			}
		}
	}
	out := map[Class]GapStats{}
	for _, c := range Classes {
		xs := lengths[c]
		st := GapStats{Gaps: len(xs)}
		var sum float64
		for _, x := range xs {
			sum += x
		}
		st.Hours = sum / 3600
		if active > 0 && c != ClassTail {
			st.Share = sum / active
		}
		st.P50, st.P90 = quantile(xs, 0.5), quantile(xs, 0.9)
		out[c] = st
	}
	return out
}

// quantile is the nearest-rank q-quantile of xs, or 0 for none.
func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	return s[max(i, 0)]
}
