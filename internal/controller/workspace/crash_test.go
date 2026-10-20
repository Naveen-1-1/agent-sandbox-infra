package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/workspace"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
)

var errCrash = errors.New("injected crash")

// crasher counts side effects that succeed; the one numbered crashAt is
// applied and then reported as failed, as if the controller died before it
// saw the response.
type crasher struct {
	mu      sync.Mutex
	n       int
	crashAt int
	crashed bool
	before  func(op string, obj client.Object) // invariant checks
}

func (c *crasher) effect(op string, obj client.Object, do func() error) error {
	c.mu.Lock()
	before := c.before
	c.mu.Unlock()
	if before != nil {
		before(op, obj)
	}
	if err := do(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	if c.n == c.crashAt {
		c.crashed = true
		return fmt.Errorf("%w after %s", errCrash, op)
	}
	return nil
}

type crashClient struct {
	client.Client
	c *crasher
}

func (k crashClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return k.c.effect("create", obj, func() error { return k.Client.Create(ctx, obj, opts...) })
}
func (k crashClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return k.c.effect("update", obj, func() error { return k.Client.Update(ctx, obj, opts...) })
}
func (k crashClient) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	return k.c.effect("patch", obj, func() error { return k.Client.Patch(ctx, obj, p, opts...) })
}
func (k crashClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return k.c.effect("delete", obj, func() error { return k.Client.Delete(ctx, obj, opts...) })
}
func (k crashClient) Status() client.SubResourceWriter { return crashStatus{k.Client.Status(), k.c} }

type crashStatus struct {
	client.SubResourceWriter
	c *crasher
}

func (s crashStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return s.c.effect("status-update", obj, func() error { return s.SubResourceWriter.Update(ctx, obj, opts...) })
}

// crashStore makes manifest writes crashable too.
type crashStore struct {
	objstore.Store
	c *crasher
}

func (s crashStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, cond objstore.Cond) (objstore.Object, error) {
	var o objstore.Object
	err := s.c.effect("s3-put "+key, nil, func() (err error) {
		o, err = s.Store.Put(ctx, key, body, size, cond)
		return err
	})
	return o, err
}

// TestCrashAtEverySideEffect runs create -> suspend -> wake once per side
// effect, crashing right after that effect, then finishes with a fresh
// controller. Every run must end in the same place without breaking an
// invariant.
func TestCrashAtEverySideEffect(t *testing.T) {
	if testing.Short() {
		t.Skip("enumerates every crash point")
	}
	ns := namespace(t, "crash-")
	_, total := runCrashScenario(t, ns, 0) // an uncrashed run counts the side effects
	if total == 0 {
		t.Fatal("scenario had no side effects")
	}
	for k := 1; k <= total; k++ {
		if crashed, _ := runCrashScenario(t, ns, k); !crashed {
			t.Errorf("crash point %d of %d was never reached", k, total)
		}
	}
	t.Logf("checked %d crash points", total)
}

func runCrashScenario(t *testing.T, ns string, crashAt int) (crashed bool, effects int) {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("c%d", crashAt)
	ws := newWorkspace(t, ns, name)
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ws)}

	cr := &crasher{crashAt: crashAt}
	var violations []string
	cr.before = func(op string, obj client.Object) {
		pvc, ok := obj.(*corev1.PersistentVolumeClaim)
		if !ok {
			return
		}
		switch op {
		case "delete": // no disk is deleted without a committed snapshot
			cur := get(t, ws)
			m, found, _ := snapstore.New(store).Latest(ctx, string(cur.UID))
			if !found || cur.Status.Suspend == nil || !cur.Status.Suspend.Committed || m.AttemptID != cur.Status.Suspend.AttemptID {
				violations = append(violations, "disk "+pvc.Name+" deleted before its snapshot was committed")
			}
		case "create": // never two live disks for one workspace
			list := &corev1.PersistentVolumeClaimList{}
			_ = k8s.List(ctx, list, client.InNamespace(ns))
			for _, p := range list.Items {
				if strings.HasPrefix(p.Name, "ws-"+name+"-") && p.DeletionTimestamp.IsZero() && p.Name != pvc.Name {
					violations = append(violations, "created "+pvc.Name+" while "+p.Name+" still exists")
				}
			}
		}
	}
	faulty := newReconciler(crashClient{k8s, cr}, k8s, crashStore{store, cr})
	faulty.Pods = crashPods{pods, cr}
	fresh := newReconciler(k8s, k8s, store)

	// drive reconciles until done, switching to a fresh controller after the crash.
	drive := func(what string, done func(*sbxv1.Workspace) bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			r := faulty
			cr.mu.Lock()
			if cr.crashed {
				r = fresh
			}
			cr.mu.Unlock()
			_, _ = r.Reconcile(ctx, req)
			if done(get(t, ws)) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cur := get(t, ws)
		t.Fatalf("crash at %d: stuck waiting for %s (phase %s, epoch %d, suspend %+v)", crashAt, what, cur.Status.Phase, cur.Status.Epoch, cur.Status.Suspend)
	}

	drive("provisioned", phase(sbxv1.PhaseRunning, 1))
	requestSuspend(t, ws)
	drive("suspended", phase(sbxv1.PhaseSuspended, 1))
	requestWake(t, ws, sbxv1.WakeTriggerRequest)
	drive("woken", phase(sbxv1.PhaseRunning, 2))

	cur := get(t, ws)
	m, found, err := snapstore.New(store).Latest(ctx, string(cur.UID))
	switch {
	case err != nil || !found:
		violations = append(violations, fmt.Sprintf("no manifest (err %v)", err))
	case cur.Status.LastSnapshot == nil || m.Seq != 1 || cur.Status.LastSnapshot.Seq != 1 || m.BundleKey != cur.Status.LastSnapshot.BundleKey:
		violations = append(violations, fmt.Sprintf("manifest %+v disagrees with status %+v", m, cur.Status.LastSnapshot))
	}
	if claimExists(t, ns, "ws-"+name+"-e1") || !claimExists(t, ns, "ws-"+name+"-e2") {
		violations = append(violations, "wrong disks after the wake")
	}
	for _, v := range violations {
		t.Errorf("crash at side effect %d: %s", crashAt, v)
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.crashed, cr.n
}

// crashPods makes pod calls crashable: freeze, thaw and snapshot starts.
type crashPods struct {
	*fakePods
	c *crasher
}

func (p crashPods) Agent(ws *sbxv1.Workspace, pod workspace.Pod) workspace.Agent {
	return crashAgent{p.fakePods.Agent(ws, pod), p.c}
}

func (p crashPods) Sidecar(ws *sbxv1.Workspace, pod workspace.Pod) workspace.Sidecar {
	return crashSidecar{p.fakePods.Sidecar(ws, pod), p.c}
}

type crashAgent struct {
	workspace.Agent
	c *crasher
}

func (a crashAgent) Freeze(ctx context.Context, attempt string, lease time.Time) error {
	return a.c.effect("freeze", nil, func() error { return a.Agent.Freeze(ctx, attempt, lease) })
}

func (a crashAgent) Thaw(ctx context.Context, attempt string) error {
	return a.c.effect("thaw", nil, func() error { return a.Agent.Thaw(ctx, attempt) })
}

type crashSidecar struct {
	workspace.Sidecar
	c *crasher
}

func (s crashSidecar) StartSnapshot(ctx context.Context, r *sidecarv1.StartSnapshotRequest) (*sidecarv1.Snapshot, error) {
	var snap *sidecarv1.Snapshot
	err := s.c.effect("start-snapshot", nil, func() (err error) {
		snap, err = s.Sidecar.StartSnapshot(ctx, r)
		return err
	})
	return snap, err
}
