//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

func waitWorkspace(t *testing.T, c *framework.Clients, ws *sbxv1.Workspace, what string, timeout time.Duration, ok func(*sbxv1.Workspace) bool) *sbxv1.Workspace {
	t.Helper()
	cur := &sbxv1.Workspace{}
	framework.Eventually(t, timeout, 250*time.Millisecond, what, func() (bool, error) {
		if err := c.CR.Get(context.Background(), client.ObjectKeyFromObject(ws), cur); err != nil {
			return false, err
		}
		return ok(cur), nil
	})
	return cur
}

func inPhase(p sbxv1.Phase, epoch int64) func(*sbxv1.Workspace) bool {
	return func(ws *sbxv1.Workspace) bool { return ws.Status.Phase == p && ws.Status.Epoch == epoch }
}

func mergePatch(t *testing.T, c *framework.Clients, obj client.Object, body map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	if err := c.CR.Patch(context.Background(), obj, client.RawPatch(types.MergePatchType, b)); err != nil {
		t.Fatal(err)
	}
}

// TestSuspendWakeOnAnotherNode drives a workspace through the Workspace API
// only: the controller provisions it, suspends it to S3 (deleting pod and
// disk), and wakes it on a different node with the work intact.
func TestSuspendWakeOnAnotherNode(t *testing.T) {
	c := framework.NewClients(t)
	p := framework.DeployedPlatform(t, c)
	ns := framework.TestNamespace(t, c)
	ctx := framework.Ctx(t, 10*time.Minute)

	ws := &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "demo"},
		Spec: sbxv1.WorkspaceSpec{
			Source: sbxv1.Source{RepoURL: p.FixturesURL + "/hello-node.git"},
			Image:  "debian:trixie-slim",
		},
	}
	if err := c.CR.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}
	cur := waitWorkspace(t, c, ws, "provisioned", 4*time.Minute, inPhase(sbxv1.PhaseRunning, 1))
	uid := string(cur.UID)
	node1 := cur.Status.Pod.NodeName
	t.Logf("epoch 1 on %s; provisioned in %s", node1, cur.Status.Timeline.ReadyAt.Sub(cur.Status.Timeline.RequestedAt.Time))

	pod1 := connect(t, ns, "demo")
	agent1 := p.Keyring.Derive(token.Agent, uid, 1)
	mustSh(t, pod1, agent1, "echo 'survives the night' > notes.txt")

	// Make the wake land elsewhere.
	framework.Kubectl(t, "cordon", node1)
	t.Cleanup(func() { framework.Kubectl(t, "uncordon", node1) })

	mergePatch(t, c, cur, map[string]any{"metadata": map[string]any{"annotations": map[string]string{sbxv1.AnnotationSuspendRequested: "e2e"}}})
	cur = waitWorkspace(t, c, ws, "suspended", 3*time.Minute, inPhase(sbxv1.PhaseSuspended, 1))
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.CR.Get(ctx, client.ObjectKey{Namespace: ns, Name: "ws-demo-e1"}, pvc); err == nil && pvc.DeletionTimestamp.IsZero() {
		t.Error("epoch 1 disk still exists after suspend")
	}
	if err := c.CR.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("pod still exists after suspend (err %v)", err)
	}
	sb := &sbv1.Sandbox{}
	if err := c.CR.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo"}, sb); err != nil || sb.Spec.OperatingMode != sbv1.SandboxOperatingModeSuspended {
		t.Errorf("sandbox not suspended: %v %v", sb.Spec.OperatingMode, err)
	}
	m, ok, err := snapstore.New(localS3(t)).Latest(ctx, uid)
	if err != nil || !ok || m.Seq != 1 || m.BundleKey != cur.Status.LastSnapshot.BundleKey {
		t.Fatalf("manifest %+v ok=%v err=%v; status %+v", m, ok, err, cur.Status.LastSnapshot)
	}
	t.Logf("snapshot %d: %d bytes", m.Seq, m.Bytes)

	// Wake, as the gateway would: bump spec.wake.seq.
	mergePatch(t, c, cur, map[string]any{"spec": map[string]any{"wake": map[string]any{
		"seq": cur.Spec.Wake.Seq + 1, "trigger": "Request", "at": time.Now().UTC().Format(time.RFC3339),
	}}})
	cur = waitWorkspace(t, c, ws, "woken", 4*time.Minute, inPhase(sbxv1.PhaseRunning, 2))
	tl := cur.Status.Timeline
	t.Logf("epoch 2 on %s; files ready after %s, serving after %s", cur.Status.Pod.NodeName,
		tl.FilesReadyAt.Sub(tl.RequestedAt.Time), tl.ReadyAt.Sub(tl.RequestedAt.Time))
	if cur.Status.Pod.NodeName == node1 {
		t.Errorf("woke on the cordoned node %s", node1)
	}
	pod2 := connect(t, ns, "demo")
	if got := mustSh(t, pod2, p.Keyring.Derive(token.Agent, uid, 2), "cat notes.txt"); got != "survives the night" {
		t.Fatalf("notes.txt after wake = %q", got)
	}
	if err := c.CR.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo"}, sb); err != nil || sb.Spec.PodTemplate.ObjectMeta.Labels[podspec.LabelEpoch] != "2" {
		t.Errorf("sandbox template not on epoch 2: %v", err)
	}

	// Delete: S3 is purged and the workspace goes away.
	if err := c.CR.Delete(ctx, cur); err != nil {
		t.Fatal(err)
	}
	framework.Eventually(t, 2*time.Minute, 250*time.Millisecond, "workspace deleted", func() (bool, error) {
		err := c.CR.Get(ctx, client.ObjectKeyFromObject(ws), &sbxv1.Workspace{})
		return apierrors.IsNotFound(err), nil
	})
	if objs, err := localS3(t).List(ctx, fmt.Sprintf("ws/%s/", uid)); err != nil || len(objs) != 0 {
		t.Errorf("%d objects left in S3 (err %v)", len(objs), err)
	}
}
