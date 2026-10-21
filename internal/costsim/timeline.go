package costsim

import (
	"math"
	"slices"
	"sort"
)

// Call is a sandbox tool call, busy from S to E (microseconds).
type Call struct {
	S, E int64
	Deps bool // it needs the dependency directory
}

// Input is a moment a person acted: a prompt, which starts a turn, or an
// answer to a question the agent asked. A harness sends a wake-hint then.
type Input struct {
	T      int64
	Prompt bool
	// First is the first sandbox call after it and before the next input,
	// or -1: the hidden-wake window is T to First.
	First int64
}

// Turn runs from a prompt (or the session's start) to the last thing the
// agent did before the next prompt, when a harness sends turn-ended.
type Turn struct{ Start, End int64 }

// Interval is a span of time.
type Interval struct{ S, E int64 }

// Timeline is one session group, rebuilt from its timestamps.
type Timeline struct {
	Key         string
	Start, Last int64 // first and last instant
	Calls       []Call
	Inputs      []Input
	Turns       []Turn
	// HumanWindows are spans a tool waited for a person.
	HumanWindows []Interval
	Quality      Quality
}

// Quality counts what the trace got wrong or left out.
type Quality struct {
	OpenTools      int            `json:"openTools"`      // never returned; taken as instant
	NegativeTools  int            `json:"negativeTools"`  // returned before they started
	DuplicateTools int            `json:"duplicateTools"` // the same call id twice
	OtherTools     map[string]int `json:"otherTools"`     // by name: not sandbox, subagent or human
}

func (q *Quality) add(o Quality) {
	q.OpenTools += o.OpenTools
	q.NegativeTools += o.NegativeTools
	q.DuplicateTools += o.DuplicateTools
	if q.OtherTools == nil {
		q.OtherTools = map[string]int{}
	}
	for k, v := range o.OtherTools {
		q.OtherTools[k] += v
	}
}

// BuildOptions shape a timeline.
type BuildOptions struct {
	Deps DepsMode
	// InternalLatency times a sandbox call by its own reported duration,
	// ending at its result; the time before is a person approving it. By
	// default the whole emitted-to-result span is busy.
	InternalLatency bool
}

// Build rebuilds a group's timeline. Only the parent's own prompts are a
// person's: a subagent's prompt comes from its parent.
func Build(g Group, o BuildOptions) Timeline {
	tl := Timeline{Key: g.Key, Start: math.MaxInt64, Last: math.MinInt64, Quality: Quality{OtherTools: map[string]int{}}}
	var agent, prompts []int64
	var answers []int64
	see := func(t int64) {
		tl.Start, tl.Last = min(tl.Start, t), max(tl.Last, t)
	}
	for _, m := range g.Members {
		for _, e := range m.Events {
			see(e.T)
			if e.Human && !m.IsSubagent() {
				prompts = append(prompts, e.T)
			} else {
				agent = append(agent, e.T)
			}
		}
		seen := map[string]bool{}
		for _, t := range m.Tools {
			if t.ID != "" {
				if seen[t.ID] {
					tl.Quality.DuplicateTools++
					continue
				}
				seen[t.ID] = true
			}
			s, e := t.S, t.S
			switch {
			case t.E == nil:
				tl.Quality.OpenTools++
			case *t.E < t.S:
				tl.Quality.NegativeTools++
			default:
				e = *t.E
			}
			see(s)
			see(e)
			switch Classify(t.Name) {
			case ToolSandbox:
				start := s
				if o.InternalLatency && t.InternalMs != nil {
					start = max(s, e-int64(*t.InternalMs*1000))
					if start > s {
						tl.HumanWindows = append(tl.HumanWindows, Interval{s, start})
					}
				}
				tl.Calls = append(tl.Calls, Call{S: start, E: e, Deps: needsDeps(t, o.Deps)})
				agent = append(agent, s, e)
			case ToolHuman:
				agent = append(agent, s)
				if t.E != nil && e > s {
					tl.HumanWindows = append(tl.HumanWindows, Interval{s, e})
					answers = append(answers, e)
				}
			case ToolOther:
				tl.Quality.OtherTools[t.Name]++
				agent = append(agent, s, e)
			default:
				agent = append(agent, s, e)
			}
		}
	}
	slices.Sort(agent)
	slices.Sort(prompts)
	sort.Slice(tl.Calls, func(i, j int) bool {
		a, b := tl.Calls[i], tl.Calls[j]
		return a.S < b.S || a.S == b.S && a.E < b.E
	})

	// Turns: from each prompt to the last agent instant before the next.
	// Agent work before the first prompt is a turn of its own.
	lastBefore := func(lo, hi int64) (int64, bool) { // the latest agent instant in [lo, hi)
		i := sort.Search(len(agent), func(i int) bool { return agent[i] >= hi })
		if i == 0 || agent[i-1] < lo {
			return 0, false
		}
		return agent[i-1], true
	}
	first := int64(math.MaxInt64)
	if len(prompts) > 0 {
		first = prompts[0]
	}
	if e, ok := lastBefore(math.MinInt64, first); ok {
		tl.Turns = append(tl.Turns, Turn{Start: tl.Start, End: e})
	}
	for k, h := range prompts {
		next := int64(math.MaxInt64)
		if k+1 < len(prompts) {
			next = prompts[k+1]
		}
		end := h
		if e, ok := lastBefore(h, next); ok {
			end = e
		}
		tl.Turns = append(tl.Turns, Turn{Start: h, End: end})
	}

	for _, p := range prompts {
		tl.Inputs = append(tl.Inputs, Input{T: p, Prompt: true})
	}
	for _, a := range answers {
		tl.Inputs = append(tl.Inputs, Input{T: a})
	}
	sort.SliceStable(tl.Inputs, func(i, j int) bool {
		a, b := tl.Inputs[i], tl.Inputs[j]
		return a.T < b.T || a.T == b.T && a.Prompt && !b.Prompt
	})
	for i := range tl.Inputs {
		next := int64(math.MaxInt64)
		if i+1 < len(tl.Inputs) {
			next = tl.Inputs[i+1].T
		}
		tl.Inputs[i].First = -1
		j := sort.Search(len(tl.Calls), func(j int) bool { return tl.Calls[j].S >= tl.Inputs[i].T })
		if j < len(tl.Calls) && tl.Calls[j].S < next {
			tl.Inputs[i].First = tl.Calls[j].S
		}
	}
	return tl
}
