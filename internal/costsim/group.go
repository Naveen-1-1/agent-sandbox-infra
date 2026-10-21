package costsim

import (
	"sort"
)

// Group is a top-level session with its subagents: one sandbox.
type Group struct {
	Key      string
	Provider string
	// Members holds the parent first (when the trace has it), then its
	// subagents by key.
	Members []Session
}

// Counts are session counts, by provider and in total.
type Counts map[string]int

func (c Counts) add(provider string) {
	c[provider]++
	c["total"]++
}

// GroupStats describes the grouping.
type GroupStats struct {
	Raw       Counts `json:"raw"`
	Subagents Counts `json:"subagents"`
	Roots     Counts `json:"roots"`
	// Orphans are subagents whose parent session isn't in the trace; they
	// form a group of their own.
	Orphans int `json:"orphans"`
	// Empty are roots with no timestamped event at all; they aren't
	// simulated.
	Empty int `json:"empty"`
}

// GroupSessions puts every subagent with its parent, by the root key the
// export computed from the session id, and returns the groups that have
// something to simulate.
func GroupSessions(all []Session) ([]Group, GroupStats) {
	st := GroupStats{Raw: Counts{}, Subagents: Counts{}, Roots: Counts{}}
	byRoot := map[string]*Group{}
	var keys []string
	for _, s := range all {
		st.Raw.add(s.Provider)
		if s.IsSubagent() {
			st.Subagents.add(s.Provider)
		}
		g := byRoot[s.Root]
		if g == nil {
			g = &Group{Key: s.Root, Provider: s.Provider}
			byRoot[s.Root] = g
			keys = append(keys, s.Root)
		}
		g.Members = append(g.Members, s)
	}
	sort.Strings(keys)
	out := make([]Group, 0, len(keys))
	for _, k := range keys {
		g := byRoot[k]
		sort.SliceStable(g.Members, func(i, j int) bool {
			pi, pj := !g.Members[i].IsSubagent(), !g.Members[j].IsSubagent()
			if pi != pj {
				return pi
			}
			return g.Members[i].Key < g.Members[j].Key
		})
		if g.Members[0].IsSubagent() {
			st.Orphans++
		}
		st.Roots.add(g.Provider)
		if !hasTime(g) {
			st.Empty++
			continue
		}
		out = append(out, *g)
	}
	return out, st
}

func hasTime(g *Group) bool {
	for _, m := range g.Members {
		if len(m.Events) > 0 || len(m.Tools) > 0 {
			return true
		}
	}
	return false
}
