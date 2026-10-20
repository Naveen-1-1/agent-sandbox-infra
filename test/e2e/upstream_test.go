//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

const busybox = "busybox:1.37"

// orderedPod is a pod whose containers append to a shared log as they start:
// a regular init container ("restore") that takes a while, a native sidecar,
// and the workload. The workload also checks that the restored file exists.
func orderedPod(claim string) corev1.PodSpec {
	logTo := func(line string) string { return "echo " + line + " >> /order/log" }
	mounts := []corev1.VolumeMount{{Name: "order", MountPath: "/order"}, {Name: "workspace", MountPath: "/workspace"}}
	return corev1.PodSpec{
		TerminationGracePeriodSeconds: ptr.To[int64](1),
		InitContainers: []corev1.Container{
			{
				Name: "restore", Image: busybox, VolumeMounts: mounts,
				Command: []string{"sh", "-c", "sleep 2; echo restored > /workspace/marker; " + logTo("restore-done")},
			},
			{
				Name: "sidecar", Image: busybox, VolumeMounts: mounts,
				RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
				Command:       []string{"sh", "-c", logTo("sidecar-start") + "; exec sleep 3600"},
			},
		},
		Containers: []corev1.Container{{
			Name: "main", Image: busybox, VolumeMounts: mounts,
			Command: []string{"sh", "-c", `if [ -f /workspace/marker ]; then ` + logTo("main-start-with-files") +
				`; else ` + logTo("main-start-without-files") + `; fi; exec sleep 3600`},
		}},
		Volumes: []corev1.Volume{
			{Name: "order", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "workspace", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			}},
		},
	}
}

func createSandbox(t *testing.T, c *framework.Clients, ns, name string, spec corev1.PodSpec) {
	t.Helper()
	sb := &sbv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: sbv1.SandboxSpec{SandboxBlueprint: sbv1.SandboxBlueprint{
			PodTemplate: sbv1.PodTemplate{Spec: spec},
			Service:     ptr.To(false),
		}},
	}
	if _, err := c.Sandbox.AgentsV1beta1().Sandboxes(ns).Create(framework.Ctx(t, time.Minute), sb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func waitCondition(t *testing.T, c *framework.Clients, ns, name string, cond sbv1.ConditionType, timeout time.Duration) {
	t.Helper()
	framework.Eventually(t, timeout, 500*time.Millisecond, name+" "+string(cond), func() (bool, error) {
		sb, err := c.Sandbox.AgentsV1beta1().Sandboxes(ns).Get(framework.Ctx(t, 10*time.Second), name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return meta.IsStatusConditionTrue(sb.Status.Conditions, string(cond)), nil
	})
}

// TestInitRestoreRunsBeforeSidecarAndWorkload proves the pod layout we rely
// on: files restored by a regular init container exist before the native
// sidecar starts, and the sidecar starts before the workload.
func TestInitRestoreRunsBeforeSidecarAndWorkload(t *testing.T) {
	c := framework.NewClients(t)
	ns := framework.TestNamespace(t, c)
	framework.CreatePVC(t, c, ns, "ws-order-e0")
	createSandbox(t, c, ns, "order", orderedPod("ws-order-e0"))
	waitCondition(t, c, ns, "order", sbv1.SandboxConditionReady, 3*time.Minute)

	got := framework.Kubectl(t, "-n", ns, "exec", "order", "-c", "main", "--", "cat", "/order/log")
	want := "restore-done\nsidecar-start\nmain-start-with-files"
	if got != want {
		t.Fatalf("container start order:\n%s\nwant:\n%s", got, want)
	}
}

// TestUpstreamResumeWithSwappedPVC proves the wake mechanics: suspending
// deletes the pod and leaves our PVC alone; patching the template to a new
// claim and resuming in one write brings up a fresh pod on the new claim.
func TestUpstreamResumeWithSwappedPVC(t *testing.T) {
	c := framework.NewClients(t)
	ns := framework.TestNamespace(t, c)
	framework.CreatePVC(t, c, ns, "ws-swap-e0")
	createSandbox(t, c, ns, "swap", orderedPod("ws-swap-e0"))
	waitCondition(t, c, ns, "swap", sbv1.SandboxConditionReady, 3*time.Minute)
	firstPod, err := c.Kube.CoreV1().Pods(ns).Get(framework.Ctx(t, time.Minute), "swap", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	patch := func(body map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		if _, err := c.Sandbox.AgentsV1beta1().Sandboxes(ns).Patch(framework.Ctx(t, time.Minute), "swap",
			types.MergePatchType, b, metav1.PatchOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	patch(map[string]any{"spec": map[string]any{"operatingMode": "Suspended"}})
	waitCondition(t, c, ns, "swap", sbv1.SandboxConditionSuspended, 2*time.Minute)
	if _, err := c.Kube.CoreV1().Pods(ns).Get(framework.Ctx(t, time.Minute), "swap", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pod still exists after Suspended=True (err = %v)", err)
	}
	if _, err := c.Kube.CoreV1().PersistentVolumeClaims(ns).Get(framework.Ctx(t, time.Minute), "ws-swap-e0", metav1.GetOptions{}); err != nil {
		t.Fatalf("our PVC was touched by suspend: %v", err)
	}

	// Wake: new epoch's claim plus Running, in a single write.
	framework.CreatePVC(t, c, ns, "ws-swap-e1")
	spec := orderedPod("ws-swap-e1")
	patch(map[string]any{"spec": map[string]any{
		"operatingMode": "Running",
		"podTemplate":   map[string]any{"spec": map[string]any{"volumes": spec.Volumes}},
	}})
	waitCondition(t, c, ns, "swap", sbv1.SandboxConditionReady, 3*time.Minute)

	pod, err := c.Kube.CoreV1().Pods(ns).Get(framework.Ctx(t, time.Minute), "swap", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.UID == firstPod.UID {
		t.Fatal("resume reused the old pod")
	}
	var claims []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.PersistentVolumeClaim.ClaimName)
		}
	}
	if strings.Join(claims, ",") != "ws-swap-e1" {
		t.Fatalf("resumed pod mounts %v, want [ws-swap-e1]", claims)
	}
	pvcs, err := c.Kube.CoreV1().PersistentVolumeClaims(ns).List(framework.Ctx(t, time.Minute), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pvcs.Items) != 2 {
		t.Errorf("found %d PVCs, want exactly our 2 (upstream must not create any)", len(pvcs.Items))
	}
}
