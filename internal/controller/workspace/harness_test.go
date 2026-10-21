package workspace_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sbcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/ops"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/sleep"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/workspace"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/envtestx"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/fakekubelet"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
)

// Shared by every test in the package: one API server running upstream's
// real Sandbox controller, the fake kubelet, and our Workspace controller
// (for namespaces not prefixed "crash-", which tests drive by hand).
var (
	k8s     client.Client
	store   *objstore.Mem
	pods    *fakePods
	keys    token.Keyring
	scheme  *runtime.Scheme
	wsm     *metrics.Workspace
	kubelet *fakekubelet.Kubelet

	gateways = &fakeGateways{}
	sleepm   *metrics.Sleep
)

// policyNS holds the cluster policy ConfigMaps.
const policyNS = "sbx-system"

const baseSHA = "1111111111111111111111111111111111111111"

type noopTracer struct{}

func (noopTracer) StartSpan(ctx context.Context, _ metav1.Object, _ string, _ map[string]string) (context.Context, func()) {
	return ctx, func() {}
}
func (noopTracer) GetTraceContext(context.Context) string              { return "" }
func (noopTracer) AddEvent(context.Context, string, map[string]string) {}
func (noopTracer) IsRecording(context.Context) bool                    { return false }

type fixedInspector struct{}

func (fixedInspector) Resolve(context.Context, sbxv1.Source) (workspace.Base, error) {
	return workspace.Base{SHA: baseSHA, Branch: "main"}, nil
}

func TestMain(m *testing.M) {
	ctrllog.SetLogger(logr.Discard())
	var err error
	scheme, err = envtestx.NewScheme(clientgoscheme.AddToScheme, sbv1.AddToScheme, sbxv1.AddToScheme)
	if err != nil {
		fail(err)
	}
	env, err := envtestx.Start(envtestx.Options{Scheme: scheme, Upstream: true})
	if err != nil {
		fail(err)
	}
	k8s = env.Client
	store = objstore.NewMem()
	pods = newFakePods(store)
	keys, _ = token.NewKeyring(bytes.Repeat([]byte{9}, 32))

	mgr, err := ctrl.NewManager(env.Config, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		fail(err)
	}
	if err := (&sbcontrollers.SandboxReconciler{Client: mgr.GetClient(), Scheme: scheme, Tracer: noopTracer{}}).SetupWithManager(mgr, 4); err != nil {
		fail(err)
	}
	if kubelet, err = fakekubelet.Setup(mgr); err != nil {
		fail(err)
	}
	kubelet.Gate = brokerAccepts
	wsm = metrics.NewWorkspace(prometheus.NewRegistry(), nil)
	if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: policyNS}}); err != nil {
		fail(err)
	}
	r := newReconciler(mgr.GetClient(), mgr.GetAPIReader(), store)
	r.Policies = &sleep.Policies{Client: mgr.GetClient(), Namespace: policyNS, Recorder: mgr.GetEventRecorder("sbx-controller")}
	r.Gateways = gateways
	r.Ops = &ops.Limiter{Max: 64, MinSuspend: 8}
	sleepm = metrics.NewSleep(prometheus.NewRegistry(), nil, nil)
	r.Sleep = sleepm
	r.Config.MaxConcurrent = 8
	notCrash := predicate.NewPredicateFuncs(func(o client.Object) bool { return !strings.HasPrefix(o.GetNamespace(), "crash-") })
	if err := r.SetupWithManager(mgr, notCrash); err != nil {
		fail(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "manager:", err)
		}
	}()
	leaktest.VerifyTestMain(leaktest.WithTeardown(m, func() {
		cancel()
		<-done
		env.Stop()
	}))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func newReconciler(c client.Client, api client.Reader, obj objstore.Store) *workspace.Reconciler {
	return &workspace.Reconciler{
		Client: c, API: api, Scheme: scheme, Keys: keys,
		Store: snapstore.New(obj), Objects: obj, Pods: pods, Inspector: fixedInspector{},
		Metrics: wsm,
		Config: workspace.Config{
			RuntimeImage: "runtime:test", BrokerAddr: "broker:9443", DefaultImage: "image:test",
			RunAsUser: 1000, RunAsGroup: 1000, Poll: 50 * time.Millisecond,
			SuspendTimeout: time.Minute, RPCTimeout: 2 * time.Second,
			Snapshot: podspec.Snapshot{Exclude: []string{"node_modules"}},
		},
	}
}

// fakePods is every workspace pod's agent and sidecar. Snapshots "upload" a
// small bundle straight into the store, as the real sidecar would.
type fakePods struct {
	store *objstore.Mem

	mu       sync.Mutex
	frozen   map[string]string // pod IP -> attempt
	held     map[string]bool   // pod IP -> frozen without a lease
	snaps    map[string]*sidecarv1.Snapshot
	blocked  map[string]bool // workspace UID -> snapshots stay RUNNING
	failures map[string]sidecarv1.SnapshotErrorCode
	signals  map[string]*signalsv1.Signals // workspace UID -> what its agent reports
	thaws    int
	hook     func(what string) error // called before each side effect; an error fails it
}

func newFakePods(s *objstore.Mem) *fakePods {
	return &fakePods{store: s, frozen: map[string]string{}, held: map[string]bool{}, snaps: map[string]*sidecarv1.Snapshot{},
		blocked: map[string]bool{}, failures: map[string]sidecarv1.SnapshotErrorCode{}, signals: map[string]*signalsv1.Signals{}}
}

func (f *fakePods) Agent(ws *sbxv1.Workspace, p workspace.Pod) workspace.Agent {
	return fakeAgent{f: f, ip: p.IP, uid: string(ws.UID)}
}
func (f *fakePods) Sidecar(ws *sbxv1.Workspace, p workspace.Pod) workspace.Sidecar {
	return fakeSidecar{f: f, ws: ws, ip: p.IP}
}

func (f *fakePods) run(what string) error {
	f.mu.Lock()
	h := f.hook
	f.mu.Unlock()
	if h != nil {
		return h(what)
	}
	return nil
}

func (f *fakePods) isFrozen(ip string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.frozen[ip]
	return ok, f.held[ip]
}

type fakeAgent struct {
	f       *fakePods
	ip, uid string
}

// Signals reports what a test set for the workspace; by default the agent
// was just used, so no policy suspends it.
func (a fakeAgent) Signals(context.Context) (*signalsv1.Signals, error) {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if s, ok := a.f.signals[a.uid]; ok {
		if s == nil {
			return nil, errors.New("agent unreachable")
		}
		return proto.Clone(s).(*signalsv1.Signals), nil
	}
	return &signalsv1.Signals{LastActivity: timestamppb.Now()}, nil
}

// setSignals makes ws's agent report s; nil makes it unreachable.
func (f *fakePods) setSignals(ws *sbxv1.Workspace, s *signalsv1.Signals) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals[string(ws.UID)] = s
}

// fakeGateways is the gateway replicas' view, set by tests.
type fakeGateways struct {
	mu      sync.Mutex
	signals map[string]policy.Signals
	down    bool
}

func (g *fakeGateways) Signals(key string) (policy.Signals, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.signals[key], !g.down
}

func (g *fakeGateways) set(ws *sbxv1.Workspace, s policy.Signals) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.signals == nil {
		g.signals = map[string]policy.Signals{}
	}
	g.signals[client.ObjectKeyFromObject(ws).String()] = s
}

func (a fakeAgent) Freeze(_ context.Context, attempt string, lease time.Time) error {
	if err := a.f.run("freeze"); err != nil {
		return err
	}
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	a.f.frozen[a.ip] = attempt
	a.f.held[a.ip] = lease.IsZero()
	return nil
}

func (a fakeAgent) Thaw(_ context.Context, attempt string) error {
	if err := a.f.run("thaw"); err != nil {
		return err
	}
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if a.f.frozen[a.ip] == attempt {
		delete(a.f.frozen, a.ip)
		delete(a.f.held, a.ip)
		a.f.thaws++
	}
	return nil
}

type fakeSidecar struct {
	f  *fakePods
	ws *sbxv1.Workspace
	ip string
}

func (s fakeSidecar) StartSnapshot(ctx context.Context, r *sidecarv1.StartSnapshotRequest) (*sidecarv1.Snapshot, error) {
	if err := s.f.run("start-snapshot"); err != nil {
		return nil, err
	}
	return s.state(ctx, r.GetAttemptId(), true)
}

func (s fakeSidecar) GetSnapshot(ctx context.Context, attempt string) (*sidecarv1.Snapshot, error) {
	return s.state(ctx, attempt, false)
}

func (s fakeSidecar) state(ctx context.Context, attempt string, start bool) (*sidecarv1.Snapshot, error) {
	f := s.f
	f.mu.Lock()
	defer f.mu.Unlock()
	uid := string(s.ws.UID)
	if f.blocked[uid] {
		return &sidecarv1.Snapshot{AttemptId: attempt, State: sidecarv1.SnapshotState_SNAPSHOT_STATE_RUNNING}, nil
	}
	if snap, ok := f.snaps[attempt]; ok {
		return snap, nil
	}
	if !start {
		return nil, fmt.Errorf("no attempt %s", attempt)
	}
	if code, ok := f.failures[uid]; ok {
		snap := &sidecarv1.Snapshot{AttemptId: attempt, State: sidecarv1.SnapshotState_SNAPSHOT_STATE_FAILED,
			Error: &sidecarv1.SnapshotError{Code: code, Message: "refused", Paths: []string{"deploy.sh (AWS access key)"}}}
		f.snaps[attempt] = snap
		return snap, nil
	}
	body := []byte("bundle for " + attempt)
	sum := sha256.Sum256(body)
	h := hex.EncodeToString(sum[:])
	key := snapstore.BundleKey(uid, h)
	if _, err := f.store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), objstore.Cond{SHA256: h}); err != nil {
		return nil, err
	}
	snap := &sidecarv1.Snapshot{AttemptId: attempt, State: sidecarv1.SnapshotState_SNAPSHOT_STATE_SUCCEEDED,
		WipSha: "wip-" + attempt, BundleKey: key, Sha256: h, SizeBytes: int64(len(body)), NewBytes: 100}
	f.snaps[attempt] = snap
	return snap, nil
}

func (s fakeSidecar) FilesReady(context.Context) (time.Time, int64, error) {
	return time.Now(), 2048, nil
}

func (f *fakePods) block(ws *sbxv1.Workspace, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked[string(ws.UID)] = on
}

func (f *fakePods) failSnapshots(ws *sbxv1.Workspace, code sidecarv1.SnapshotErrorCode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[string(ws.UID)] = code
}

// brokerAccepts mirrors the broker: a pod's restore only succeeds once its
// epoch is the workspace's current epoch.
func brokerAccepts(ctx context.Context, pod *corev1.Pod) bool {
	ws := &sbxv1.Workspace{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Labels[podspec.LabelWorkspace]}, ws); err != nil {
		return false
	}
	return pod.Labels[podspec.LabelEpoch] == fmt.Sprint(ws.Status.Epoch)
}

// --- helpers ---

var nsSeq sync.Mutex
var nsCount int

func namespace(t *testing.T, prefix string) string {
	t.Helper()
	nsSeq.Lock()
	nsCount++
	name := fmt.Sprintf("%s%d", prefix, nsCount)
	nsSeq.Unlock()
	if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatal(err)
	}
	return name
}

func newWorkspace(t *testing.T, ns, name string) *sbxv1.Workspace {
	t.Helper()
	ws := &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       sbxv1.WorkspaceSpec{Source: sbxv1.Source{RepoURL: "https://example.com/r.git"}},
	}
	if err := k8s.Create(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	return ws
}

func get(t *testing.T, ws *sbxv1.Workspace) *sbxv1.Workspace {
	t.Helper()
	out := &sbxv1.Workspace{}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(ws), out); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitFor(t *testing.T, ws *sbxv1.Workspace, what string, ok func(*sbxv1.Workspace) bool) *sbxv1.Workspace {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cur := get(t, ws); ok(cur) {
			return cur
		}
		time.Sleep(20 * time.Millisecond)
	}
	cur := get(t, ws)
	t.Fatalf("timed out waiting for %s; phase=%s epoch=%d suspend=%+v conditions=%+v", what, cur.Status.Phase, cur.Status.Epoch, cur.Status.Suspend, cur.Status.Conditions)
	return nil
}

func phase(p sbxv1.Phase, epoch int64) func(*sbxv1.Workspace) bool {
	return func(ws *sbxv1.Workspace) bool { return ws.Status.Phase == p && ws.Status.Epoch == epoch }
}

func requestSuspend(t *testing.T, ws *sbxv1.Workspace) {
	t.Helper()
	patch := []byte(`{"metadata":{"annotations":{"` + sbxv1.AnnotationSuspendRequested + `":"test"}}}`)
	if err := k8s.Patch(context.Background(), ws, client.RawPatch("application/merge-patch+json", patch)); err != nil {
		t.Fatal(err)
	}
}

// requestWake bumps spec.wake.seq the way the gateway does, retrying on conflicts.
func requestWake(t *testing.T, ws *sbxv1.Workspace, trigger sbxv1.WakeTrigger) {
	t.Helper()
	for range 50 {
		cur := get(t, ws)
		cur.Spec.Wake = sbxv1.WakeRequest{Seq: cur.Spec.Wake.Seq + 1, Trigger: trigger, At: &metav1.Time{Time: time.Now()}}
		if err := k8s.Update(context.Background(), cur); err == nil {
			return
		}
	}
	t.Fatal("could not bump wake.seq")
}

// claimExists reports whether a disk exists and isn't being deleted (envtest
// has no controller to clear Kubernetes' pvc-protection finalizer).
func claimExists(t *testing.T, ns, name string) bool {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, pvc)
	return err == nil && pvc.DeletionTimestamp.IsZero()
}

func sandbox(t *testing.T, ws *sbxv1.Workspace) *sbv1.Sandbox {
	t.Helper()
	sb := &sbv1.Sandbox{}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(ws), sb); err != nil {
		t.Fatal(err)
	}
	return sb
}

func pause() { time.Sleep(20 * time.Millisecond) }
