package sidecar

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/broker/brokertest"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
	gf "github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/gitfixture"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

type fixture struct {
	env    *brokertest.Env
	ws     *sbxv1.Workspace
	origin string
	base   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	origin := gf.NewRepo(t, map[string]string{"README.md": "hello\n", "src/app.js": "console.log(1)\n"})
	base := gf.Git(t, origin, "rev-parse", "HEAD")
	env := brokertest.Start(t, 0)
	ws := &sbxv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "demo", UID: "uid-demo"},
		Status: sbxv1.WorkspaceStatus{
			Epoch: 1,
			Base:  &sbxv1.BaseStatus{URL: "file://" + origin, SHA: base, Branch: "main"},
		},
	}
	env.Set(ws)
	return &fixture{env: env, ws: ws, origin: origin, base: base}
}

func (f *fixture) config(t *testing.T, epoch int64) Config {
	t.Helper()
	return Config{
		Workspace: "team/demo", WorkspaceUID: string(f.ws.UID), Epoch: epoch, PodUID: "pod-1",
		Root: filepath.Join(t.TempDir(), "workspace"), Scratch: t.TempDir(),
		SidecarToken: "sidecar-token", Exclude: []string{"node_modules"},
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// userState is the restored workspace minus sbx's own directory.
func userState(t *testing.T, root string) map[string]string {
	st := gf.State(t, root)
	for k := range st {
		if strings.HasPrefix(k, "file:.sbx/") {
			delete(st, k)
		}
	}
	return st
}

func TestRestoreFreshWorkspaceAndCacheBase(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(t, 1)
	mkdir(t, cfg.Root)
	b := f.env.Client(t, f.ws, 1)

	if err := Restore(ctx(t), cfg, b); err != nil {
		t.Fatal(err)
	}
	if got := gf.Git(t, cfg.Root, "rev-parse", "HEAD"); got != f.base {
		t.Fatalf("HEAD = %s, want base %s", got, f.base)
	}
	if got := gf.Git(t, cfg.Root, "status", "--porcelain"); got != "" {
		t.Fatalf("fresh workspace is dirty (sbx's directory must be hidden):\n%s", got)
	}
	vol, ok, err := ReadVolume(cfg.Root)
	if err != nil || !ok || vol.State != StateLive || vol.Epoch != 1 || !vol.PendingBaseUpload || vol.FilesReadyAt.IsZero() {
		t.Fatalf("volume = %+v, ok=%v err=%v", vol, ok, err)
	}

	srv := NewServer(cfg, b)
	defer srv.snaps.Close()
	srv.uploadBase(ctx(t))
	if _, err := f.env.Store.Head(ctx(t), snapstore.BaseBundleKey(f.ws.Status.Base.URL, f.base)); err != nil {
		t.Fatalf("base bundle not cached: %v", err)
	}
	if vol, _, _ := ReadVolume(cfg.Root); vol.PendingBaseUpload {
		t.Error("base upload still pending after it succeeded")
	}
}

func TestRestoreLeavesALiveVolumeAlone(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(t, 1)
	mkdir(t, cfg.Root)
	b := f.env.Client(t, f.ws, 1)
	if err := Restore(ctx(t), cfg, b); err != nil {
		t.Fatal(err)
	}
	gf.Write(t, filepath.Join(cfg.Root, "user-work.txt"), "precious\n")

	if err := Restore(ctx(t), cfg, b); err != nil { // the pod restarted
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(cfg.Root, "user-work.txt")); err != nil || string(b) != "precious\n" {
		t.Fatalf("a second restore touched live work: %q, %v", b, err)
	}
}

func TestRestoreRedoesAPartialRestore(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(t, 1)
	mkdir(t, cfg.Root)
	if err := WriteVolume(cfg.Root, Volume{Epoch: 1, State: StateRestoring}); err != nil {
		t.Fatal(err)
	}
	gf.Write(t, filepath.Join(cfg.Root, "half-written.txt"), "junk\n")

	if err := Restore(ctx(t), cfg, f.env.Client(t, f.ws, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "half-written.txt")); !os.IsNotExist(err) {
		t.Fatal("leftovers from the interrupted restore survived")
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "README.md")); err != nil {
		t.Fatal("files not restored")
	}
}

func waitFor(t *testing.T, s *Snapshotter, id string) *sidecarv1.Snapshot {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		op, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if op.GetState() != sidecarv1.SnapshotState_SNAPSHOT_STATE_RUNNING {
			return op
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("attempt %s still running", id)
	return nil
}

func startReq(id string, epoch int64, base string) *sidecarv1.StartSnapshotRequest {
	return &sidecarv1.StartSnapshotRequest{AttemptId: id, Epoch: epoch, BaseSha: base, FreezeTime: timestamppb.New(time.Unix(1_800_000_000, 0))}
}

// TestSnapshotThenWakeIntoNextEpoch is the in-pod half of a sleep/wake
// cycle: snapshot epoch 1, then restore that snapshot into a fresh disk for
// epoch 2 using the cached base bundle.
func TestSnapshotThenWakeIntoNextEpoch(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(t, 1)
	mkdir(t, cfg.Root)
	b1 := f.env.Client(t, f.ws, 1)
	if err := Restore(ctx(t), cfg, b1); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(cfg, b1)
	srv.uploadBase(ctx(t))

	// The agent works.
	gf.Write(t, filepath.Join(cfg.Root, "README.md"), "hello, edited\n")
	gf.Write(t, filepath.Join(cfg.Root, "notes/plan.md"), "untracked plan\n")
	gf.Write(t, filepath.Join(cfg.Root, "src/new.js"), "export const x = 1\n")
	gf.Git(t, cfg.Root, "add", "src/new.js")
	gf.Git(t, cfg.Root, "commit", "-q", "-m", "agent commit")
	gf.Write(t, filepath.Join(cfg.Root, "node_modules/dep/index.js"), "dependency\n")
	want := userState(t, cfg.Root)
	delete(want, "file:node_modules/dep/index.js")
	// node_modules is excluded from the snapshot, so it's untracked noise in status only.
	want["status"] = strings.ReplaceAll(want["status"], "?? node_modules/dep/index.js\n", "")

	op, err := srv.snaps.Start(startReq("a1", 1, f.base))
	if err != nil {
		t.Fatal(err)
	}
	if op.GetState() != sidecarv1.SnapshotState_SNAPSHOT_STATE_RUNNING {
		t.Fatalf("new attempt state = %s", op.GetState())
	}
	op = waitFor(t, srv.snaps, "a1")
	srv.snaps.Close()
	if op.GetState() != sidecarv1.SnapshotState_SNAPSHOT_STATE_SUCCEEDED {
		t.Fatalf("snapshot failed: %v", op.GetError())
	}
	if want := snapstore.BundleKey(string(f.ws.UID), op.GetSha256()); op.GetBundleKey() != want {
		t.Fatalf("bundle key = %s, want %s", op.GetBundleKey(), want)
	}
	h, err := f.env.Store.Head(ctx(t), op.GetBundleKey())
	if err != nil || h.Size != op.GetSizeBytes() || h.SHA256 != op.GetSha256() {
		t.Fatalf("stored bundle = %+v, %v; want %d bytes sha %s", h, err, op.GetSizeBytes(), op.GetSha256())
	}

	// The controller commits the snapshot and wakes the workspace into epoch 2.
	f.ws.Status.Epoch = 2
	f.ws.Status.LastSnapshot = &sbxv1.SnapshotRef{Seq: 1, Epoch: 1, WIPSHA: op.GetWipSha(),
		BundleKey: op.GetBundleKey(), SHA256: op.GetSha256(), Bytes: op.GetSizeBytes()}
	f.env.Set(f.ws)
	cfg2 := f.config(t, 2)
	cfg2.Workspace = cfg.Workspace
	mkdir(t, cfg2.Root)
	if err := os.Rename(f.origin, f.origin+".gone"); err != nil { // origin is down: the base must come from S3
		t.Fatal(err)
	}
	if err := Restore(ctx(t), cfg2, f.env.Client(t, f.ws, 2)); err != nil {
		t.Fatal(err)
	}
	got := userState(t, cfg2.Root)
	for _, k := range []string{"HEAD", "refs", "index", "file:README.md", "file:notes/plan.md", "file:src/new.js"} {
		if got[k] != want[k] {
			t.Errorf("%s after wake = %q, want %q", k, got[k], want[k])
		}
	}
	if _, err := os.Stat(filepath.Join(cfg2.Root, "node_modules")); !os.IsNotExist(err) {
		t.Error("node_modules was restored from the snapshot; it must come from the dependency cache")
	}
}

func TestSnapshotRefusedForSecret(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(t, 1)
	mkdir(t, cfg.Root)
	b := f.env.Client(t, f.ws, 1)
	if err := Restore(ctx(t), cfg, b); err != nil {
		t.Fatal(err)
	}
	gf.Write(t, filepath.Join(cfg.Root, "deploy.sh"), "export AWS_KEY=AKIA"+strings.Repeat("Z", 16)+"\n")
	s := NewSnapshotter(cfg, b)
	defer s.Close()
	if _, err := s.Start(startReq("a1", 1, f.base)); err != nil {
		t.Fatal(err)
	}
	op := waitFor(t, s, "a1")
	if op.GetState() != sidecarv1.SnapshotState_SNAPSHOT_STATE_FAILED ||
		op.GetError().GetCode() != sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_SECRET_DETECTED ||
		len(op.GetError().GetPaths()) != 1 || !strings.HasPrefix(op.GetError().GetPaths()[0], "deploy.sh") {
		t.Fatalf("op = %v, want SECRET_DETECTED for deploy.sh", op)
	}
}

func TestSnapshotAttemptsAreIdempotentAndSurviveRestart(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(t, 1)
	mkdir(t, cfg.Root)
	b := f.env.Client(t, f.ws, 1)
	if err := Restore(ctx(t), cfg, b); err != nil {
		t.Fatal(err)
	}
	gf.Write(t, filepath.Join(cfg.Root, "x.txt"), "x\n")

	s := NewSnapshotter(cfg, b)
	if _, err := s.Start(startReq("a1", 1, f.base)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(startReq("a1", 1, f.base)); err != nil {
		t.Fatal(err)
	}
	first := waitFor(t, s, "a1")
	s.Close()

	restarted := NewSnapshotter(cfg, b)
	defer restarted.Close()
	again, err := restarted.Start(startReq("a1", 1, f.base))
	if err != nil {
		t.Fatal(err)
	}
	if again.GetState() != sidecarv1.SnapshotState_SNAPSHOT_STATE_SUCCEEDED || again.GetSha256() != first.GetSha256() {
		t.Fatalf("after a restart: %v, want the recorded result %v", again, first)
	}
	if _, err := restarted.Start(startReq("a2", 2, f.base)); err == nil {
		t.Error("an attempt for another epoch was accepted")
	}
	if _, err := restarted.Start(startReq("../escape", 1, f.base)); err == nil {
		t.Error("a path-like attempt ID was accepted")
	}
}

func TestServerRequiresTheSidecarToken(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(t, 1)
	mkdir(t, cfg.Root)
	b := f.env.Client(t, f.ws, 1)
	if err := Restore(ctx(t), cfg, b); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(cfg, b)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(serveCtx, l) }()
	defer func() {
		stop()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := sidecarv1.NewSidecarServiceClient(conn)

	bad := metadata.AppendToOutgoingContext(ctx(t), "authorization", "Bearer wrong")
	if _, err := c.Identify(bad, &sidecarv1.IdentifyRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong token: %v, want Unauthenticated", err)
	}
	good := metadata.AppendToOutgoingContext(ctx(t), "authorization", "Bearer sidecar-token")
	id, err := c.Identify(good, &sidecarv1.IdentifyRequest{})
	if err != nil || id.GetEpoch() != 1 || id.GetVolumeState() != sidecarv1.VolumeState_VOLUME_STATE_LIVE {
		t.Fatalf("Identify = %v, %v", id, err)
	}
	if _, err := healthv1.NewHealthClient(conn).Check(ctx(t), &healthv1.HealthCheckRequest{}); err != nil {
		t.Fatalf("health check without a token: %v", err)
	}
	stream, err := c.WatchProgress(good, &sidecarv1.WatchProgressRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := stream.Recv()
	if err != nil || ev.GetStage() != sidecarv1.Stage_STAGE_FILES_READY {
		t.Fatalf("first progress event = %v, %v; want FILES_READY", ev, err)
	}
}
