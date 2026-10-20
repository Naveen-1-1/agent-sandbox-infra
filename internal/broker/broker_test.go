package broker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	brokerv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/broker/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/broker/brokertest"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func workspace(epoch int64) *sbxv1.Workspace {
	return &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "demo", UID: "uid-demo"},
		Spec:       sbxv1.WorkspaceSpec{Source: sbxv1.Source{RepoURL: "https://example.com/r.git"}},
		Status: sbxv1.WorkspaceStatus{
			Epoch: epoch,
			Base:  &sbxv1.BaseStatus{URL: "https://example.com/r.git", SHA: "abc123", Branch: "main"},
		},
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func send(t *testing.T, r *brokerv1.PresignedRequest, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx(t), r.GetMethod(), r.GetUrl(), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range r.GetHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestAuthentication(t *testing.T) {
	env := brokertest.Start(t, 0)
	ws := workspace(3)
	env.Set(ws)
	other := workspace(3)
	other.Name, other.UID = "other", "uid-other"
	env.Set(other)

	if _, err := env.Client(t, ws, 3).GetRestorePlan(ctx(t), &brokerv1.GetRestorePlanRequest{}); err != nil {
		t.Fatalf("own token: %v", err)
	}
	for name, c := range map[string]struct {
		client brokerv1.BrokerServiceClient
		code   codes.Code
	}{
		"garbage token":           {env.ClientWithToken(t, ws, 3, "garbage"), codes.Unauthenticated},
		"another workspace token": {env.ClientWithToken(t, ws, 3, env.Token(other, 3)), codes.Unauthenticated},
		"older epoch":             {env.Client(t, ws, 2), codes.PermissionDenied},
		"unknown workspace":       {env.Client(t, &sbxv1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "ghost", UID: "u"}}, 3), codes.Unauthenticated},
	} {
		_, err := c.client.GetRestorePlan(ctx(t), &brokerv1.GetRestorePlanRequest{})
		if status.Code(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
}

func TestRestorePlanFollowsWorkspaceState(t *testing.T) {
	env := brokertest.Start(t, 0)
	ws := workspace(1)
	env.Set(ws)
	c := env.Client(t, ws, 1)

	// Fresh workspace: base from origin, and please upload a base bundle.
	plan, err := c.GetRestorePlan(ctx(t), &brokerv1.GetRestorePlanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.GetBaseSha() != "abc123" || plan.GetBranch() != "main" || plan.GetBaseBundle() != nil || plan.GetSnapshotBundle() != nil || !plan.GetUploadBase() {
		t.Fatalf("fresh plan = %v", plan)
	}

	// The pod uploads the base bundle.
	baseBody := []byte("base bundle")
	up, err := c.PresignBaseUpload(ctx(t), &brokerv1.PresignBaseUploadRequest{Sha256: sum(baseBody), SizeBytes: int64(len(baseBody))})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg := send(t, up.GetUpload(), baseBody); code != http.StatusOK {
		t.Fatalf("base upload: %d %s", code, msg)
	}

	// A committed snapshot.
	snapBody := []byte("snapshot bundle")
	key := snapstore.BundleKey(string(ws.UID), sum(snapBody))
	if _, err := env.Store.Put(ctx(t), key, bytes.NewReader(snapBody), int64(len(snapBody)), objstore.Cond{SHA256: sum(snapBody)}); err != nil {
		t.Fatal(err)
	}
	ws.Status.Epoch = 2
	ws.Status.LastSnapshot = &sbxv1.SnapshotRef{Seq: 1, Epoch: 1, BundleKey: key, SHA256: sum(snapBody), Bytes: int64(len(snapBody))}
	env.Set(ws)

	plan, err = env.Client(t, ws, 2).GetRestorePlan(ctx(t), &brokerv1.GetRestorePlanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.GetUploadBase() || plan.GetBaseBundle() == nil || plan.GetSnapshotBundle() == nil || plan.GetSnapshotSha256() != sum(snapBody) {
		t.Fatalf("plan after snapshot = %v", plan)
	}
	if code, got := send(t, plan.GetBaseBundle(), nil); code != http.StatusOK || !bytes.Equal(got, baseBody) {
		t.Errorf("base download: %d %q", code, got)
	}
	if code, got := send(t, plan.GetSnapshotBundle(), nil); code != http.StatusOK || !bytes.Equal(got, snapBody) {
		t.Errorf("snapshot download: %d %q", code, got)
	}
}

func TestShallowWorkspaceNeverUploadsABase(t *testing.T) {
	env := brokertest.Start(t, 0)
	ws := workspace(1)
	ws.Status.Base.Shallow = true
	env.Set(ws)
	plan, err := env.Client(t, ws, 1).GetRestorePlan(ctx(t), &brokerv1.GetRestorePlanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.GetUploadBase() {
		t.Error("a shallow workspace was asked to upload a base bundle")
	}
}

func TestSnapshotUpload(t *testing.T) {
	env := brokertest.Start(t, 64)
	ws := workspace(1)
	env.Set(ws)
	c := env.Client(t, ws, 1)

	body := []byte("a small bundle")
	resp, err := c.PresignSnapshotUpload(ctx(t), &brokerv1.PresignSnapshotUploadRequest{AttemptId: "a1", Sha256: sum(body), SizeBytes: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	if want := snapstore.BundleKey("uid-demo", sum(body)); resp.GetUpload().GetKey() != want {
		t.Errorf("key = %s, want it under the caller's prefix: %s", resp.GetUpload().GetKey(), want)
	}
	if code, msg := send(t, resp.GetUpload(), []byte("tampered body!")); code < 400 {
		t.Errorf("upload with a body that doesn't match the checksum: %d %s", code, msg)
	}
	if code, msg := send(t, resp.GetUpload(), body); code != http.StatusOK {
		t.Fatalf("upload: %d %s", code, msg)
	}
	again, err := c.PresignSnapshotUpload(ctx(t), &brokerv1.PresignSnapshotUploadRequest{AttemptId: "a2", Sha256: sum(body), SizeBytes: int64(len(body))})
	if err != nil || !again.GetAlreadyExists() || again.GetUpload().GetKey() != resp.GetUpload().GetKey() {
		t.Errorf("same bundle again: already_exists=%v key=%q err=%v", again.GetAlreadyExists(), again.GetUpload().GetKey(), err)
	}

	_, err = c.PresignSnapshotUpload(ctx(t), &brokerv1.PresignSnapshotUploadRequest{AttemptId: "a3", Sha256: sum([]byte("x")), SizeBytes: 65})
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("over the size limit: %v, want ResourceExhausted", err)
	}
	_, err = c.PresignSnapshotUpload(ctx(t), &brokerv1.PresignSnapshotUploadRequest{AttemptId: "a4", Sha256: "../../etc", SizeBytes: 1})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("non-hex sha256: %v, want InvalidArgument", err)
	}
}
