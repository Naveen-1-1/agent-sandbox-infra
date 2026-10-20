package podspec_test

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/yaml"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/envtestx"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

var update = flag.Bool("update", false, "rewrite golden files")

var env *envtestx.Env

func TestMain(m *testing.M) {
	scheme, err := envtestx.NewScheme(sbv1.AddToScheme, sbxv1.AddToScheme)
	if err == nil {
		env, err = envtestx.Start(envtestx.Options{Scheme: scheme, Upstream: true})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	leaktest.VerifyTestMain(leaktest.WithTeardown(m, env.Stop))
}

func input() podspec.Input {
	return podspec.Input{
		Workspace: &sbxv1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "demo", UID: "0b6e7c1a-uid"}},
		Epoch:     3, ClaimName: "ws-demo-e3",
		Image: "mcr.microsoft.com/devcontainers/base:bookworm", RuntimeImage: "localhost:5001/sbx-runtime:dev",
		PullPolicy: corev1.PullIfNotPresent, TokenSecret: "ws-demo-e3-tokens", AgentTokenSHA256: "ab12",
		BrokerAddr: "sbx-controller.sbx-system:9443",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		},
		RunAsUser: 1000, RunAsGroup: 1000,
		Snapshot: podspec.Snapshot{Exclude: []string{"node_modules", ".venv"}, MaxFileBytes: 50 << 20, MaxNewBytes: 500 << 20, MaxBundleBytes: 500 << 20},
	}
}

func TestGolden(t *testing.T) {
	tmpl, err := podspec.Build(input())
	if err != nil {
		t.Fatal(err)
	}
	got, err := yaml.Marshal(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "epoch-3.golden.yaml")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("pod template differs from %s; rerun with -update if intended:\n%s", path, got)
	}
}

func TestContainerOrderAndSecrets(t *testing.T) {
	tmpl, err := podspec.Build(input())
	if err != nil {
		t.Fatal(err)
	}
	spec := tmpl.Spec
	var names []string
	for _, c := range spec.InitContainers {
		names = append(names, c.Name)
	}
	if fmt.Sprint(names) != fmt.Sprint([]string{podspec.ContainerTools, podspec.ContainerRestore, podspec.ContainerSidecar}) {
		t.Errorf("init order = %v", names)
	}
	if rp := spec.InitContainers[2].RestartPolicy; rp == nil || *rp != corev1.ContainerRestartPolicyAlways {
		t.Error("sbx-sidecar is not a native sidecar")
	}
	for _, m := range spec.Containers[0].VolumeMounts {
		if m.Name == podspec.VolumeTokens {
			t.Error("the workload container can read the pod's tokens")
		}
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("service account token is mounted")
	}
	if got := spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != "ws-demo-e3" {
		t.Errorf("claim = %s", got)
	}
}

func TestMissingInputsRejected(t *testing.T) {
	in := input()
	in.TokenSecret, in.Epoch = "", 0
	if _, err := podspec.Build(in); err == nil {
		t.Fatal("Build accepted an input without a token secret or epoch")
	}
}

// TestAcceptedByAgentSandbox submits the template inside a real upstream
// Sandbox, so schema or CEL changes upstream break this test, not a wake.
func TestAcceptedByAgentSandbox(t *testing.T) {
	tmpl, err := podspec.Build(input())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sb := &sbv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec: sbv1.SandboxSpec{
			SandboxBlueprint: sbv1.SandboxBlueprint{PodTemplate: tmpl, Service: ptr.To(false)},
			OperatingMode:    sbv1.SandboxOperatingModeRunning,
		},
	}
	if err := env.Client.Create(ctx, sb); err != nil {
		t.Fatalf("upstream rejected the pod template: %v", err)
	}
}
