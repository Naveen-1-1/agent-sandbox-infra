//go:build e2e

package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	sbv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	processv1 "sigs.k8s.io/agent-sandbox/packages/sandboxd/spec/process/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	agentv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/agent/v1"
	brokerv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/broker/v1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/agent"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/broker"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/controller/podspec"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/token"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

// fixtureHead returns the commit the fixtures server has for repo's main.
func fixtureHead(t *testing.T, repo string) string {
	t.Helper()
	addr := framework.PortForward(t, framework.Namespace(), "svc/sbx-fixtures", 80)
	req, _ := http.NewRequestWithContext(framework.Ctx(t, time.Minute), http.MethodGet, "http://"+addr+"/"+repo+".git/info/refs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(b), "\n") {
		if sha, ref, ok := strings.Cut(line, "\t"); ok && ref == "refs/heads/main" {
			return sha
		}
	}
	t.Fatalf("no refs/heads/main in %q", b)
	return ""
}

// epochResources creates what the controller would for one epoch: the token
// Secret and the PVC, and returns the pod template.
func epochResources(t *testing.T, c *framework.Clients, p framework.Platform, ws *sbxv1.Workspace, epoch int64) sbv1.PodTemplate {
	t.Helper()
	ctx := framework.Ctx(t, time.Minute)
	uid := string(ws.UID)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ws.Namespace, Name: fmt.Sprintf("%s-e%d-tokens", ws.Name, epoch)},
		StringData: podspec.Tokens(p.Keyring, uid, epoch),
	}
	if err := c.CR.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	claim := fmt.Sprintf("ws-%s-e%d", ws.Name, epoch)
	framework.CreatePVC(t, c, ws.Namespace, claim)
	tmpl, err := podspec.Build(podspec.Input{
		Workspace: ws, Epoch: epoch, ClaimName: claim,
		Image: "debian:trixie-slim", RuntimeImage: p.RuntimeImage, PullPolicy: corev1.PullIfNotPresent,
		TokenSecret: secret.Name, AgentTokenSHA256: hex.EncodeToString(token.SHA256(p.Keyring.Derive(token.Agent, uid, epoch))),
		BrokerAddr: p.BrokerAddr, RunAsUser: 1000, RunAsGroup: 1000,
		Snapshot: podspec.Snapshot{Exclude: []string{"node_modules"}, MaxFileBytes: 10 << 20, MaxNewBytes: 50 << 20, MaxBundleBytes: 50 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

type podConns struct {
	proc    processv1.ProcessServiceClient
	agent   agentv1.AgentServiceClient
	sidecar sidecarv1.SidecarServiceClient
}

func connect(t *testing.T, ns, pod string) podConns {
	t.Helper()
	a := framework.Dial(t, framework.PortForward(t, ns, "pod/"+pod, podspec.AgentGRPCPort))
	s := framework.Dial(t, framework.PortForward(t, ns, "pod/"+pod, podspec.SidecarPort))
	return podConns{proc: processv1.NewProcessServiceClient(a), agent: agentv1.NewAgentServiceClient(a), sidecar: sidecarv1.NewSidecarServiceClient(s)}
}

func bearer(t *testing.T, tok string) context.Context {
	return metadata.AppendToOutgoingContext(framework.Ctx(t, 2*time.Minute), "authorization", "Bearer "+tok)
}

func sh(t *testing.T, c podConns, tok, script string) (string, error) {
	t.Helper()
	resp, err := c.proc.Execute(bearer(t, tok), &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"sh", "-c", script}}})
	if err != nil {
		return "", err
	}
	if resp.GetExitCode() != 0 {
		return "", fmt.Errorf("exit %d: %s", resp.GetExitCode(), resp.GetStderr())
	}
	return strings.TrimSpace(string(resp.GetStdout())), nil
}

func mustSh(t *testing.T, c podConns, tok, script string) string {
	t.Helper()
	out, err := sh(t, c, tok, script)
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return out
}

// TestWorkspacePodSleepsAndWakesByHand drives one workspace through a full
// cycle by hand, doing what the controller will automate: restore a fresh
// workspace, work in it, freeze and snapshot it, then bring up the next
// epoch on a new disk and check the work came back.
func TestWorkspacePodSleepsAndWakesByHand(t *testing.T) {
	c := framework.NewClients(t)
	p := framework.DeployedPlatform(t, c)
	ns := framework.TestNamespace(t, c)
	ctx := framework.Ctx(t, 10*time.Minute)
	base := fixtureHead(t, "hello-node")

	// Paused: this test plays the controller's part itself.
	ws := &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "demo", Annotations: map[string]string{sbxv1.AnnotationPaused: "true"}},
		Spec:       sbxv1.WorkspaceSpec{Source: sbxv1.Source{RepoURL: p.FixturesURL + "/hello-node.git"}},
	}
	if err := c.CR.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}
	ws.Status = sbxv1.WorkspaceStatus{Epoch: 1, Base: &sbxv1.BaseStatus{URL: ws.Spec.Source.RepoURL, SHA: base, Branch: "main"}}
	if err := c.CR.Status().Update(ctx, ws); err != nil {
		t.Fatal(err)
	}

	// Epoch 1: fresh restore from the fixtures server.
	sb := &sbv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "demo"},
		Spec: sbv1.SandboxSpec{SandboxBlueprint: sbv1.SandboxBlueprint{
			PodTemplate: epochResources(t, c, p, ws, 1), Service: ptr.To(false),
		}},
	}
	if err := c.CR.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	waitCondition(t, c, ns, "demo", sbv1.SandboxConditionReady, 4*time.Minute)
	pod1 := connect(t, ns, "demo")
	agent1 := p.Keyring.Derive(token.Agent, string(ws.UID), 1)
	sidecar1 := p.Keyring.Derive(token.Sidecar, string(ws.UID), 1)

	if got := mustSh(t, pod1, agent1, "cat README.md"); !strings.Contains(got, "hello-node") {
		t.Fatalf("README.md = %q", got)
	}
	mustSh(t, pod1, agent1, "echo 'agent work' > notes.txt && mkdir -p src && echo 'export {}' > src/new.js")

	// PID 1 reaps orphans: background children whose parent exits don't linger as zombies.
	mustSh(t, pod1, agent1, "for i in 1 2 3 4 5; do (sleep 0.2 &); done; sleep 1")
	if z := mustSh(t, pod1, agent1, "grep -l ') Z ' /proc/[0-9]*/stat 2>/dev/null | wc -l"); z != "0" {
		t.Errorf("%s zombie processes; PID 1 is not reaping", z)
	}

	// Freeze: writes are refused, then snapshot while frozen.
	if _, err := pod1.agent.Freeze(bearer(t, agent1), &agentv1.FreezeRequest{AttemptId: "a1", LeaseUntil: timestamppb.New(time.Now().Add(5 * time.Minute))}); err != nil {
		t.Fatal(err)
	}
	var trailer metadata.MD
	_, err := pod1.proc.Execute(bearer(t, agent1), &processv1.ExecuteRequest{Config: &processv1.ProcessConfig{Command: []string{"touch", "late.txt"}}}, grpc.Trailer(&trailer))
	if status.Code(err) != codes.Unavailable || trailer.Get(agent.FrozenTrailer) == nil {
		t.Fatalf("exec while frozen: %v (trailer %v), want Unavailable + %s", err, trailer, agent.FrozenTrailer)
	}
	if _, err := pod1.sidecar.StartSnapshot(bearer(t, sidecar1), &sidecarv1.StartSnapshotRequest{
		AttemptId: "a1", Epoch: 1, BaseSha: base, FreezeTime: timestamppb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	var snap *sidecarv1.Snapshot
	framework.Eventually(t, 2*time.Minute, 250*time.Millisecond, "snapshot a1", func() (bool, error) {
		r, err := pod1.sidecar.GetSnapshot(bearer(t, sidecar1), &sidecarv1.GetSnapshotRequest{AttemptId: "a1"})
		if err != nil {
			return false, err
		}
		snap = r.GetSnapshot()
		return snap.GetState() != sidecarv1.SnapshotState_SNAPSHOT_STATE_RUNNING, nil
	})
	if snap.GetState() != sidecarv1.SnapshotState_SNAPSHOT_STATE_SUCCEEDED {
		t.Fatalf("snapshot failed: %v", snap.GetError())
	}
	t.Logf("snapshot: %d bytes at %s", snap.GetSizeBytes(), snap.GetBundleKey())

	// Commit and wake into epoch 2 on a new disk.
	ws.Status.Epoch = 2
	ws.Status.LastSnapshot = &sbxv1.SnapshotRef{Seq: 1, Epoch: 1, WIPSHA: snap.GetWipSha(),
		BundleKey: snap.GetBundleKey(), SHA256: snap.GetSha256(), Bytes: snap.GetSizeBytes()}
	if err := c.CR.Status().Update(ctx, ws); err != nil {
		t.Fatal(err)
	}
	patch := func(body map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		if err := c.CR.Patch(ctx, &sbv1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "demo"}}, rawPatch(b)); err != nil {
			t.Fatal(err)
		}
	}
	patch(map[string]any{"spec": map[string]any{"operatingMode": "Suspended"}})
	waitCondition(t, c, ns, "demo", sbv1.SandboxConditionSuspended, 2*time.Minute)
	tmpl2 := epochResources(t, c, p, ws, 2)
	patch(map[string]any{"spec": map[string]any{"operatingMode": "Running", "podTemplate": tmpl2}})
	waitCondition(t, c, ns, "demo", sbv1.SandboxConditionReady, 4*time.Minute)

	pod2 := connect(t, ns, "demo")
	agent2 := p.Keyring.Derive(token.Agent, string(ws.UID), 2)
	if got := mustSh(t, pod2, agent2, "cat notes.txt src/new.js"); got != "agent work\nexport {}" {
		t.Fatalf("restored work = %q", got)
	}
	if _, err := sh(t, pod2, agent2, "test ! -e late.txt"); err != nil {
		t.Error("a write refused during the freeze showed up after the wake")
	}
	id, err := pod2.sidecar.Identify(bearer(t, p.Keyring.Derive(token.Sidecar, string(ws.UID), 2)), &sidecarv1.IdentifyRequest{})
	if err != nil || id.GetEpoch() != 2 || id.GetVolumeState() != sidecarv1.VolumeState_VOLUME_STATE_LIVE {
		t.Fatalf("Identify = %v, %v", id, err)
	}

	// The old epoch's tokens no longer work anywhere.
	if _, err := sh(t, pod2, agent1, "true"); status.Code(err) != codes.Unauthenticated {
		t.Errorf("epoch-1 agent token on the epoch-2 pod: %v, want Unauthenticated", err)
	}
	bconn := framework.Dial(t, framework.PortForward(t, framework.Namespace(), "svc/sbx-controller", 9443),
		broker.Credentials(p.Keyring.Derive(token.Broker, string(ws.UID), 1), ns+"/demo", 1))
	if _, err := brokerv1.NewBrokerServiceClient(bconn).GetRestorePlan(ctx, &brokerv1.GetRestorePlanRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("epoch-1 broker token after the wake: %v, want PermissionDenied", err)
	}
}

type rawPatch []byte

func (rawPatch) Type() types.PatchType                  { return types.MergePatchType }
func (p rawPatch) Data(_ client.Object) ([]byte, error) { return p, nil }
