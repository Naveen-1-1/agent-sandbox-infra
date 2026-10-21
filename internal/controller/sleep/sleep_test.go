package sleep_test

import (
	"context"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	gatewayv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/gateway/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/sleep"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/gateway"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

const ns = "sbx-system"

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func configMap(name, yaml string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string]string{policy.ConfigKey: yaml}}
}

func clusterPolicy(t *testing.T, p *sleep.Policies) string {
	t.Helper()
	pol, err := p.Cluster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return pol.Name()
}

func drain(r *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-r.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestClusterPolicyFollowsConfigMaps(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	rec := events.NewFakeRecorder(10)
	p := &sleep.Policies{Client: c, Namespace: ns, Recorder: rec}

	if got := clusterPolicy(t, p); got != "never" {
		t.Fatalf("no config: %s, want never", got)
	}
	defaults := configMap(sleep.DefaultsConfigMap, "name: fixed\nparams: {idleTimeout: 10m}\n")
	if err := c.Create(ctx, defaults); err != nil {
		t.Fatal(err)
	}
	if got := clusterPolicy(t, p); got != "fixed" {
		t.Fatalf("defaults only: %s, want fixed", got)
	}
	override := configMap(sleep.PolicyConfigMap, "name: never\n")
	if err := c.Create(ctx, override); err != nil {
		t.Fatal(err)
	}
	if got := clusterPolicy(t, p); got != "never" {
		t.Fatalf("with sbx-policy: %s, want never (it wins over the defaults)", got)
	}
	drain(rec)

	// A broken edit keeps the policy in force and says why, once.
	override.Data[policy.ConfigKey] = "name: fixed\nparams: {idleTimeout: often}\n"
	if err := c.Update(ctx, override); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := clusterPolicy(t, p); got != "never" {
			t.Fatalf("after a bad edit: %s, want the previous policy (never)", got)
		}
	}
	if evs := drain(rec); len(evs) != 1 || !strings.Contains(evs[0], "Warning InvalidPolicy") {
		t.Fatalf("events = %q, want one InvalidPolicy warning", evs)
	}

	if err := c.Delete(ctx, override); err != nil {
		t.Fatal(err)
	}
	if got := clusterPolicy(t, p); got != "fixed" {
		t.Fatalf("after deleting sbx-policy: %s, want the defaults (fixed)", got)
	}
}

func TestBrokenConfigAtStartUsesTheDefaults(t *testing.T) {
	c := newClient(t,
		configMap(sleep.PolicyConfigMap, "name: [\n"),
		configMap(sleep.DefaultsConfigMap, "name: fixed\n"))
	p := &sleep.Policies{Client: c, Namespace: ns, Recorder: events.NewFakeRecorder(10)}
	if got := clusterPolicy(t, p); got != "fixed" {
		t.Fatalf("got %s, want the defaults", got)
	}
}

func TestWorkspacePolicyOverridesTheCluster(t *testing.T) {
	p := &sleep.Policies{Client: newClient(t, configMap(sleep.DefaultsConfigMap, "name: fixed\n")), Namespace: ns}
	ws := &sbxv1.Workspace{Spec: sbxv1.WorkspaceSpec{Policy: &sbxv1.PolicyRef{Name: "never"}}}
	if pol, err := p.For(context.Background(), ws); err != nil || pol.Name() != "never" {
		t.Fatalf("got %v, %v", pol, err)
	}
	ws.Spec.Policy = &sbxv1.PolicyRef{Name: "fixed", Params: map[string]string{"idleTimeout": "nope"}}
	if _, err := p.For(context.Background(), ws); err == nil {
		t.Fatal("a bad spec.policy was accepted")
	}
}

// replica is one gateway's real signals service on a local port.
type replica struct {
	signals *gateway.Signals
	holder  *gateway.Holder
	addr    string
	stop    func()
}

func startReplica(t *testing.T) *replica {
	t.Helper()
	h := &gateway.Holder{PerWorkspace: 10, Global: 10, Timeout: time.Minute}
	r := &replica{signals: gateway.NewSignals(h), holder: h}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	gatewayv1.RegisterGatewaySignalsServiceServer(g, r.signals)
	go func() { _ = g.Serve(l) }()
	r.addr, r.stop = l.Addr().String(), g.Stop
	t.Cleanup(g.Stop)
	return r
}

// slice publishes replicas the way the EndpointSlice controller would.
func slice(name string, ready bool, rs ...*replica) *discoveryv1.EndpointSlice {
	s := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{discoveryv1.LabelServiceName: "sbx-gateway-signals"}},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	for _, r := range rs {
		host, port, _ := net.SplitHostPort(r.addr)
		p, _ := strconv.Atoi(port)
		// One slice per port in this test, since each replica listens on its own.
		s.Ports = []discoveryv1.EndpointPort{{Name: ptr.To("signals"), Port: ptr.To(int32(p))}}
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{Addresses: []string{host}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(ready)}})
	}
	return s
}

func TestCollectorMergesEveryReadyReplica(t *testing.T) {
	a, b, starting := startReplica(t), startReplica(t), startReplica(t)
	t0 := time.Now()
	a.signals.Now = func() time.Time { return t0 }
	b.signals.Now = func() time.Time { return t0.Add(time.Second) }
	a.signals.TurnStarted("ns/w")
	b.signals.TurnEnded("ns/w")
	b.signals.Touch("ns/other")
	starting.signals.TurnEnded("ns/w") // not ready: must be ignored

	var mu sync.Mutex
	var changed []string
	col := &sleep.Collector{
		Reader:    newClient(t, slice("a", true, a), slice("b", true, b), slice("c", false, starting)),
		Namespace: ns, Service: "sbx-gateway-signals",
		Changed: func(keys []string) { mu.Lock(); changed = append(changed, keys...); mu.Unlock() },
	}
	t.Cleanup(col.Close)
	if _, ok := col.Signals("ns/w"); ok {
		t.Fatal("signals reported before any poll")
	}
	col.Collect(context.Background())
	s, ok := col.Signals("ns/w")
	if !ok || !s.LastTurnStarted.Equal(t0) || !s.LastTurnEnded.Equal(t0.Add(time.Second)) {
		t.Fatalf("ns/w = %+v, %v", s, ok)
	}
	if s, ok := col.Signals("ns/never-seen"); !ok || !s.LastActivity.IsZero() {
		t.Fatalf("an unseen workspace should be known-quiet: %+v, %v", s, ok)
	}
	mu.Lock()
	if !slices.Contains(changed, "ns/w") || slices.Contains(changed, "ns/other") {
		t.Errorf("changed = %v, want ns/w only (its turn moved; other only had activity)", changed)
	}
	changed = nil
	mu.Unlock()

	col.Collect(context.Background())
	mu.Lock()
	if len(changed) != 0 {
		t.Errorf("changed = %v after a poll where nothing moved", changed)
	}
	mu.Unlock()
}

func TestCollectorReportsUnknownWhenAReplicaIsDown(t *testing.T) {
	up, down := startReplica(t), startReplica(t)
	down.stop()
	col := &sleep.Collector{
		Reader:    newClient(t, slice("up", true, up), slice("down", true, down)),
		Namespace: ns, Service: "sbx-gateway-signals", Timeout: 500 * time.Millisecond,
	}
	t.Cleanup(col.Close)
	col.Collect(context.Background())
	if _, ok := col.Signals("ns/w"); ok {
		t.Fatal("a replica that didn't answer may hold requests or streams; signals must be unknown")
	}
}

func TestCollectorSignalsGoStale(t *testing.T) {
	now := time.Now()
	col := &sleep.Collector{Reader: newClient(t), Namespace: ns, Service: "sbx-gateway-signals",
		Interval: time.Second, Now: func() time.Time { return now }}
	t.Cleanup(col.Close)
	col.Collect(context.Background())
	if _, ok := col.Signals("ns/w"); !ok {
		t.Fatal("no gateway replicas is a complete (empty) view")
	}
	now = now.Add(4 * time.Second)
	if _, ok := col.Signals("ns/w"); ok {
		t.Fatal("a view three polls old is still trusted")
	}
}
