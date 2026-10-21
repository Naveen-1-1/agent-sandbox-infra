package workspace_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/metrics"
)

func depsReason(reason string) func(*sbxv1.Workspace) bool {
	return func(ws *sbxv1.Workspace) bool {
		c := meta.FindStatusCondition(ws.Status.Conditions, sbxv1.ConditionDepsReady)
		return ws.Status.Phase == sbxv1.PhaseRunning && c != nil && c.Reason == reason
	}
}

func npm(state sidecarv1.DepsState, src sidecarv1.DepsSource) *sidecarv1.Deps {
	return &sidecarv1.Deps{State: state, Source: src, Profile: "npm", Key: "k1", Bytes: 4096, FinishedAt: timestamppb.Now()}
}

func TestDependencyRestoreIsTrackedUntilReady(t *testing.T) {
	ns := namespace(t, "deps-")
	pods.setDeps(ns, "demo", npm(sidecarv1.DepsState_DEPS_STATE_RESTORING, 0))
	before := testutil.ToFloat64(wsm.DepsRestores.WithLabelValues("npm", metrics.DepsSourceCache))
	ws := newWorkspace(t, ns, "demo")

	cur := waitFor(t, ws, "serving while dependencies restore", depsReason(sbxv1.DepsReasonRestoring))
	if !sbxv1.DepsPending(cur) || cur.Status.Deps == nil || cur.Status.Deps.Profile != "npm" {
		t.Fatalf("status = %+v", cur.Status.Deps)
	}
	pods.mu.Lock()
	image := pods.depImage[ns+"/demo"]
	pods.mu.Unlock()
	if image != "image:test@sha256:fake" {
		t.Errorf("restore asked for image %q, want the workload's digest", image)
	}

	pods.setDeps(ns, "demo", npm(sidecarv1.DepsState_DEPS_STATE_READY, sidecarv1.DepsSource_DEPS_SOURCE_CACHE))
	cur = waitFor(t, ws, "dependencies ready", depsReason(sbxv1.DepsReasonReady))
	if sbxv1.DepsPending(cur) || cur.Status.Deps.Source != sbxv1.DepsSourceCache || cur.Status.Timeline.DepsReadyAt == nil {
		t.Fatalf("after the restore: deps %+v, timeline %+v", cur.Status.Deps, cur.Status.Timeline)
	}
	if testutil.ToFloat64(wsm.DepsRestores.WithLabelValues("npm", metrics.DepsSourceCache)) != before+1 {
		t.Error("the cache hit wasn't counted")
	}
}

func TestAFailedRestoreStopsHoldingCommands(t *testing.T) {
	ns := namespace(t, "depsfail-")
	pods.setDeps(ns, "demo", &sidecarv1.Deps{State: sidecarv1.DepsState_DEPS_STATE_FAILED, Profile: "npm", Message: "npm ERR! 404"})
	ws := newWorkspace(t, ns, "demo")
	cur := waitFor(t, ws, "the failure reported", depsReason(sbxv1.DepsReasonFailed))
	if sbxv1.DepsPending(cur) {
		t.Fatal("a failed restore still holds commands")
	}
	if c := meta.FindStatusCondition(cur.Status.Conditions, sbxv1.ConditionDepsReady); c.Message != "npm ERR! 404" {
		t.Errorf("message = %q", c.Message)
	}
}

func TestServicesStartAfterDependenciesAndAgainAfterAWake(t *testing.T) {
	ns := namespace(t, "svc-")
	pods.setDeps(ns, "demo", npm(sidecarv1.DepsState_DEPS_STATE_INSTALLING, 0))
	ws := &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "demo"},
		Spec: sbxv1.WorkspaceSpec{
			Source: sbxv1.Source{RepoURL: "https://example.com/r.git"},
			Services: []sbxv1.ServiceSpec{
				{Name: "web", Command: []string{"npm", "run", "dev"}},
				{Name: "docs", Command: []string{"python3", "-m", "http.server"}, WaitForDeps: ptr.To(false)},
			},
		},
	}
	if err := k8s.Create(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	cur := waitFor(t, ws, "installing", depsReason(sbxv1.DepsReasonInstalling))
	eventually(t, "docs started before the install finished", func() bool { return slices.Equal(pods.running(cur), []string{"docs"}) })
	if get(t, ws).Status.ServicesApplied != "" {
		t.Fatal("services marked applied while web still waits")
	}

	pods.setDeps(ns, "demo", npm(sidecarv1.DepsState_DEPS_STATE_READY, sidecarv1.DepsSource_DEPS_SOURCE_INSTALL))
	cur = waitFor(t, ws, "services applied", func(w *sbxv1.Workspace) bool { return w.Status.ServicesApplied != "" })
	if got := pods.running(cur); !slices.Equal(got, []string{"docs", "web"}) {
		t.Fatalf("running %v after the install", got)
	}

	// A new pod after a sleep runs them again.
	pods.setUpload(ns, "demo", &sidecarv1.DepsUpload{Key: "k1", SizeBytes: 5000, Uploaded: true})
	uploaded := testutil.ToFloat64(wsm.DepsUploads.WithLabelValues(metrics.DepsUploaded))
	requestSuspend(t, ws)
	cur = waitFor(t, ws, "suspended", phase(sbxv1.PhaseSuspended, 1))
	if c := meta.FindStatusCondition(cur.Status.Conditions, sbxv1.ConditionDepsReady); c.Reason != sbxv1.DepsReasonSuspended {
		t.Errorf("DepsReady while suspended: %s", c.Reason)
	}
	if testutil.ToFloat64(wsm.DepsUploads.WithLabelValues(metrics.DepsUploaded)) != uploaded+1 {
		t.Error("the dependency upload wasn't counted")
	}
	pods.setDeps(ns, "demo", npm(sidecarv1.DepsState_DEPS_STATE_READY, sidecarv1.DepsSource_DEPS_SOURCE_CACHE))
	requestWake(t, ws, sbxv1.WakeTriggerRequest)
	cur = waitFor(t, ws, "awake with services", func(w *sbxv1.Workspace) bool {
		return w.Status.Phase == sbxv1.PhaseRunning && w.Status.Epoch == 2 && w.Status.ServicesApplied != "" &&
			w.Status.ServicesApplied != cur.Status.ServicesApplied
	})
	if got := pods.running(cur); !slices.Equal(got, []string{"docs", "web"}) {
		t.Fatalf("running %v in the new pod", got)
	}

	// Undeclaring a service stops it.
	eventually(t, "docs undeclared", func() bool {
		fresh := get(t, ws)
		fresh.Spec.Services = fresh.Spec.Services[:1]
		return k8s.Update(context.Background(), fresh) == nil
	})
	eventually(t, "docs stopped", func() bool { return slices.Equal(pods.running(cur), []string{"web"}) })
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
