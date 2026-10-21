// Package sleep feeds the controller's sleep decisions: which policy applies
// to a workspace, and what the gateways know about its traffic.
package sleep

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
)

// ConfigMap names, in the controller's namespace.
const (
	// PolicyConfigMap is written at runtime (the settings page). It wins
	// over the defaults and survives chart upgrades.
	PolicyConfigMap = "sbx-policy"
	// DefaultsConfigMap is rendered by the chart.
	DefaultsConfigMap = "sbx-policy-defaults"
)

// Policies resolves the policy for each workspace: its own spec.policy, or
// else the cluster policy. The cluster policy is read from ConfigMaps on
// every call (from the cache) and parsed once per ConfigMap version. A bad
// config is reported with an Event and the last good one stays in force.
type Policies struct {
	Client    client.Reader // cached ConfigMaps in Namespace
	Namespace string
	Recorder  events.EventRecorder

	mu     sync.Mutex
	parsed map[string]parsed // ConfigMap name -> its last parsed version
	good   policy.Policy
}

type parsed struct {
	version string
	policy  policy.Policy
	err     error
}

// For returns the policy for ws.
func (p *Policies) For(ctx context.Context, ws *sbxv1.Workspace) (policy.Policy, error) {
	if ref := ws.Spec.Policy; ref != nil {
		pol, err := policy.New(ref.Name, ref.Params)
		if err != nil {
			return nil, fmt.Errorf("spec.policy: %w", err)
		}
		return pol, nil
	}
	return p.Cluster(ctx)
}

// Cluster returns the cluster-wide policy: sbx-policy if it exists, else the
// chart's defaults, else "never". If the config in force fails to parse,
// the last good policy stays (or, right after a start, the defaults).
func (p *Policies) Cluster(ctx context.Context) (policy.Policy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range []string{PolicyConfigMap, DefaultsConfigMap} {
		cm := &corev1.ConfigMap{}
		err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: name}, cm)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err == nil {
			r := p.parse(cm)
			if r.err == nil {
				p.good = r.policy
				return r.policy, nil
			}
		}
		if p.good != nil {
			return p.good, nil
		}
	}
	never, _ := policy.New("never", nil)
	return never, nil
}

// parse reads cm once per version, reporting the outcome as an Event.
func (p *Policies) parse(cm *corev1.ConfigMap) parsed {
	if r, ok := p.parsed[cm.Name]; ok && r.version == cm.ResourceVersion {
		return r
	}
	_, pol, err := policy.ParseConfig([]byte(cm.Data[policy.ConfigKey]))
	r := parsed{version: cm.ResourceVersion, policy: pol, err: err}
	if p.parsed == nil {
		p.parsed = map[string]parsed{}
	}
	p.parsed[cm.Name] = r
	if p.Recorder != nil {
		if err != nil {
			p.Recorder.Eventf(cm, nil, corev1.EventTypeWarning, "InvalidPolicy", "LoadPolicy",
				"ignoring this config: %v; the previous policy stays in force", err)
		} else {
			p.Recorder.Eventf(cm, nil, corev1.EventTypeNormal, "PolicyLoaded", "LoadPolicy", "sleep policy %s", pol.Name())
		}
	}
	return r
}
