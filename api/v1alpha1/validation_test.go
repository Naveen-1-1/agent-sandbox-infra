package v1alpha1_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/envtestx"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

var k8s client.Client

func TestMain(m *testing.M) {
	scheme, err := envtestx.NewScheme(sbxv1.AddToScheme)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	env, err := envtestx.Start(envtestx.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	k8s = env.Client
	leaktest.VerifyTestMain(leaktest.WithTeardown(m, env.Stop))
}

var seq atomic.Int64

func newWorkspace() *sbxv1.Workspace {
	return &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ws-%d", seq.Add(1)), Namespace: "default"},
		Spec: sbxv1.WorkspaceSpec{
			Source: sbxv1.Source{RepoURL: "https://github.com/octocat/Hello-World.git"},
		},
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func mustCreate(t *testing.T, ws *sbxv1.Workspace) *sbxv1.Workspace {
	t.Helper()
	if err := k8s.Create(ctx(t), ws); err != nil {
		t.Fatalf("create: %v", err)
	}
	return ws
}

func wantInvalid(t *testing.T, err error, msg string) {
	t.Helper()
	if !apierrors.IsInvalid(err) {
		t.Fatalf("err = %v, want an Invalid error mentioning %q", err, msg)
	}
	if !strings.Contains(err.Error(), msg) {
		t.Fatalf("err = %v, want it to mention %q", err, msg)
	}
}

func TestDefaults(t *testing.T) {
	ws := mustCreate(t, newWorkspace())
	if ws.Spec.Mode != sbxv1.ModeAuto {
		t.Errorf("mode = %q, want Auto", ws.Spec.Mode)
	}
	if ws.Spec.Profile != "default" {
		t.Errorf("profile = %q, want default", ws.Spec.Profile)
	}
}

func TestSourceIsImmutable(t *testing.T) {
	ws := mustCreate(t, newWorkspace())
	ws.Spec.Source.Ref = "other-branch"
	wantInvalid(t, k8s.Update(ctx(t), ws), "source is immutable")
}

func TestRepoURLMustBeHTTPWithoutCredentials(t *testing.T) {
	for _, url := range []string{"git@github.com:org/repo.git", "ssh://github.com/org/repo", "file:///etc/passwd", "",
		"https://user:token@github.com/org/repo.git"} {
		ws := newWorkspace()
		ws.Spec.Source.RepoURL = url
		err := k8s.Create(ctx(t), ws)
		if !apierrors.IsInvalid(err) {
			t.Errorf("repoURL %q: err = %v, want Invalid", url, err)
		}
	}
}

func TestDepthNotNegative(t *testing.T) {
	ws := newWorkspace()
	ws.Spec.Source.Depth = -1
	wantInvalid(t, k8s.Create(ctx(t), ws), "spec.source.depth")
}

func TestModeEnum(t *testing.T) {
	ws := newWorkspace()
	ws.Spec.Mode = "Sometimes"
	wantInvalid(t, k8s.Create(ctx(t), ws), "spec.mode")
}

func TestWakeSeqNeverDecreases(t *testing.T) {
	ws := newWorkspace()
	ws.Spec.Wake = sbxv1.WakeRequest{Seq: 5, Trigger: sbxv1.WakeTriggerHint}
	mustCreate(t, ws)

	ws.Spec.Wake.Seq = 6
	if err := k8s.Update(ctx(t), ws); err != nil {
		t.Fatalf("bumping wake.seq: %v", err)
	}
	ws.Spec.Wake.Seq = 2
	wantInvalid(t, k8s.Update(ctx(t), ws), "wake.seq must not decrease")
}

func TestStatusIsASubresource(t *testing.T) {
	ws := mustCreate(t, newWorkspace())
	ws.Status.Phase = sbxv1.PhaseRunning
	ws.Status.Epoch = 3
	if err := k8s.Status().Update(ctx(t), ws); err != nil {
		t.Fatalf("status update: %v", err)
	}
	// A spec update must not be able to change status.
	ws.Status.Epoch = 99
	ws.Spec.Image = "example.com/img:1"
	if err := k8s.Update(ctx(t), ws); err != nil {
		t.Fatal(err)
	}
	got := &sbxv1.Workspace{}
	if err := k8s.Get(ctx(t), client.ObjectKeyFromObject(ws), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Epoch != 3 || got.Spec.Image != "example.com/img:1" {
		t.Errorf("epoch = %d, image = %q; want status untouched by a spec update", got.Status.Epoch, got.Spec.Image)
	}
}
