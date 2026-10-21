// Package policy decides when a running workspace should go to sleep.
//
// A Policy is a pure function of what is known about a workspace (its
// Signals) and the time. The controller gathers the signals, asks the
// policy, and acts on the Decision. Policies are registered by name and
// built from string parameters, so the cluster default and per-workspace
// overrides are plain configuration.
package policy

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Policy decides whether a running workspace should suspend now.
type Policy interface {
	// Name is the registered name, for example "fixed".
	Name() string
	// Decide must not suspend on missing data: when s.Unknown is not empty
	// it keeps the workspace.
	Decide(ws Workspace, s Signals, now time.Time) Decision
}

// Workspace is what a policy knows about the workspace itself.
type Workspace struct {
	// Key is "namespace/name".
	Key string
	// AwakeSince is when the workspace last started serving.
	AwakeSince time.Time
}

// Reason is a short, stable explanation of a decision. It is shown in
// workspace status and used as a metric label.
type Reason string

// Reasons a workspace is kept awake.
const (
	ReasonActive         Reason = "Active"         // the idle timer is running
	ReasonCommandRunning Reason = "CommandRunning" // a blocking command is running
	ReasonStreamOpen     Reason = "StreamOpen"     // a blocking connection is open
	ReasonRequestsHeld   Reason = "RequestsHeld"   // requests are waiting for it
	ReasonSignalsUnknown Reason = "SignalsUnknown" // a signal source couldn't be read
	ReasonNever          Reason = "Never"          // the policy never suspends
)

// Reasons a workspace is suspended.
const (
	ReasonIdle      Reason = "Idle"      // no activity for the idle timeout
	ReasonTurnEnded Reason = "TurnEnded" // the agent's turn ended and nothing happened since
)

// Decision is a policy's verdict.
type Decision struct {
	Suspend bool
	Reason  Reason
	Message string
	// SuspendAt is when the workspace will suspend if nothing changes; zero
	// when it won't on its own.
	SuspendAt time.Time
	// RecheckAfter is when to ask again; zero means only when something
	// changes.
	RecheckAfter time.Duration
}

// Param describes one policy parameter.
type Param struct {
	Name    string `json:"name"`
	Default string `json:"default"`
	Help    string `json:"help"`
}

// Spec registers a policy.
type Spec struct {
	Name   string
	Help   string
	Params []Param
	// New builds the policy. params holds every declared parameter, with
	// defaults filled in.
	New func(params map[string]string) (Policy, error)
}

var (
	mu       sync.RWMutex
	registry = map[string]Spec{}
)

// Register adds a policy. It panics on a duplicate name.
func Register(s Spec) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[s.Name]; dup {
		panic("policy: duplicate registration of " + s.Name)
	}
	registry[s.Name] = s
}

// Specs lists the registered policies by name.
func Specs() []Spec {
	mu.RLock()
	defer mu.RUnlock()
	out := slices.Collect(maps.Values(registry))
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// New builds the named policy. Unknown parameters are an error, so a typo
// in configuration is reported instead of silently ignored.
func New(name string, params map[string]string) (Policy, error) {
	mu.RLock()
	s, ok := registry[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown policy %q (have %s)", name, strings.Join(names(), ", "))
	}
	full := map[string]string{}
	for _, p := range s.Params {
		full[p.Name] = p.Default
	}
	for k, v := range params {
		if _, known := full[k]; !known {
			return nil, fmt.Errorf("policy %s has no parameter %q", name, k)
		}
		full[k] = v
	}
	return s.New(full)
}

func names() []string {
	var out []string
	for _, s := range Specs() {
		out = append(out, s.Name)
	}
	return out
}
