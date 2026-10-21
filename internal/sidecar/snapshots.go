package sidecar

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	brokerv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/broker/v1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
)

var validAttempt = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Snapshotter runs snapshot attempts for the controller, one at a time.
// Attempts are keyed by ID: starting an attempt that already exists returns
// it, and finished results are kept on the disk so a restarted sidecar or
// controller can pick them up.
type Snapshotter struct {
	cfg    Config
	broker brokerv1.BrokerServiceClient
	deps   *Deps // caches freshly installed dependencies; may be nil

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	slot   chan struct{} // one snapshot at a time

	mu  sync.Mutex
	ops map[string]*sidecarv1.Snapshot
}

// NewSnapshotter returns a Snapshotter; call Close to stop running
// attempts. deps, if set, uploads a fresh dependency install with each
// snapshot.
func NewSnapshotter(cfg Config, broker brokerv1.BrokerServiceClient, deps *Deps) *Snapshotter {
	cfg.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Snapshotter{
		cfg: cfg, broker: broker, deps: deps, ctx: ctx, cancel: cancel,
		slot: make(chan struct{}, 1), ops: map[string]*sidecarv1.Snapshot{},
	}
}

// Close cancels running attempts and waits for them to stop.
func (s *Snapshotter) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Snapshotter) opPath(id string) string {
	return filepath.Join(metaDir(s.cfg.Root), "ops", id+".json")
}

// Start begins the attempt, or returns it if it already exists.
func (s *Snapshotter) Start(r *sidecarv1.StartSnapshotRequest) (*sidecarv1.Snapshot, error) {
	id := r.GetAttemptId()
	if !validAttempt.MatchString(id) {
		return nil, fmt.Errorf("invalid attempt_id %q", id)
	}
	if r.GetEpoch() != s.cfg.Epoch {
		return nil, fmt.Errorf("attempt is for epoch %d but this pod serves epoch %d", r.GetEpoch(), s.cfg.Epoch)
	}
	if r.GetFreezeTime() == nil {
		return nil, errors.New("freeze_time is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.ops[id]; ok {
		return proto.Clone(op).(*sidecarv1.Snapshot), nil
	}
	if op, err := s.load(id); err == nil {
		s.ops[id] = op
		return proto.Clone(op).(*sidecarv1.Snapshot), nil
	}
	op := &sidecarv1.Snapshot{AttemptId: id, State: sidecarv1.SnapshotState_SNAPSHOT_STATE_RUNNING, StartedAt: timestamppb.New(s.cfg.Now())}
	s.ops[id] = op
	s.wg.Add(1)
	go s.run(id, r.GetBaseSha(), r.GetFreezeTime().AsTime())
	return proto.Clone(op).(*sidecarv1.Snapshot), nil
}

// Get returns an attempt's state.
func (s *Snapshotter) Get(id string) (*sidecarv1.Snapshot, error) {
	if !validAttempt.MatchString(id) {
		return nil, fmt.Errorf("invalid attempt_id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.ops[id]; ok {
		return proto.Clone(op).(*sidecarv1.Snapshot), nil
	}
	op, err := s.load(id)
	if err != nil {
		return nil, err
	}
	s.ops[id] = op
	return proto.Clone(op).(*sidecarv1.Snapshot), nil
}

func (s *Snapshotter) load(id string) (*sidecarv1.Snapshot, error) {
	b, err := os.ReadFile(s.opPath(id))
	if err != nil {
		return nil, err
	}
	op := &sidecarv1.Snapshot{}
	return op, protojson.Unmarshal(b, op)
}

func (s *Snapshotter) run(id, base string, freeze time.Time) {
	defer s.wg.Done()
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	case <-s.ctx.Done():
		s.finish(id, nil, &sidecarv1.SnapshotError{Code: sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_INTERNAL, Message: "sidecar shutting down"})
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.SnapshotTimeout)
	defer cancel()

	work := filepath.Join(s.cfg.Scratch, "snapshot-"+id)
	defer os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		s.finish(id, nil, internalErr(err))
		return
	}

	res, err := snapshot.Create(ctx, s.cfg.Root, snapshot.Options{
		Base: base, Exclude: s.cfg.Exclude, Limits: s.cfg.Limits, Time: freeze, WorkDir: work,
	})
	if err != nil {
		s.finish(id, nil, snapshotErr(err))
		return
	}
	resp, err := s.broker.PresignSnapshotUpload(ctx, &brokerv1.PresignSnapshotUploadRequest{
		AttemptId: id, Sha256: res.SHA256, SizeBytes: res.Size,
	})
	if err != nil {
		s.finish(id, nil, &sidecarv1.SnapshotError{Code: sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_UPLOAD_FAILED, Message: err.Error()})
		return
	}
	u := resp.GetUpload()
	if !resp.GetAlreadyExists() {
		if err := upload(ctx, s.cfg.HTTP, u.GetMethod(), u.GetUrl(), u.GetHeaders(), res.BundlePath); err != nil {
			s.finish(id, nil, &sidecarv1.SnapshotError{Code: sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_UPLOAD_FAILED, Message: err.Error()})
			return
		}
	}
	key := u.GetKey()
	var du *sidecarv1.DepsUpload
	if s.deps != nil {
		// Still frozen, so the directory can't change under the archive.
		du = s.deps.Upload(s.ctx)
	}
	s.finish(id, &sidecarv1.Snapshot{
		WipSha: res.WIP, IndexSha: res.Index, Head: res.Head,
		BundleKey: key, Sha256: res.SHA256, SizeBytes: res.Size, NewBytes: res.NewBytes, DepsUpload: du,
	}, nil)
}

// finish records the outcome in memory and on disk.
func (s *Snapshotter) finish(id string, done *sidecarv1.Snapshot, fail *sidecarv1.SnapshotError) {
	s.mu.Lock()
	op := s.ops[id]
	if done != nil {
		started := op.GetStartedAt()
		op = done
		op.AttemptId, op.StartedAt = id, started
		op.State = sidecarv1.SnapshotState_SNAPSHOT_STATE_SUCCEEDED
	} else {
		op.State, op.Error = sidecarv1.SnapshotState_SNAPSHOT_STATE_FAILED, fail
	}
	op.FinishedAt = timestamppb.New(s.cfg.Now())
	s.ops[id] = op
	snap := proto.Clone(op).(*sidecarv1.Snapshot)
	s.mu.Unlock()

	b, err := protojson.Marshal(snap)
	if err == nil {
		err = writeFileAtomic(s.opPath(id), b)
	}
	if err != nil {
		s.cfg.Log.Error("recording snapshot result", "attempt", id, "err", err)
	}
	if fail != nil {
		s.cfg.Log.Warn("snapshot failed", "attempt", id, "code", fail.GetCode().String(), "message", fail.GetMessage(), "paths", fail.GetPaths())
	} else {
		s.cfg.Log.Info("snapshot uploaded", "attempt", id, "bytes", snap.GetSizeBytes(), "key", snap.GetBundleKey())
	}
}

func snapshotErr(err error) *sidecarv1.SnapshotError {
	var pe *snapshot.PolicyError
	if errors.As(err, &pe) {
		code := sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_TOO_LARGE
		if pe.Code == snapshot.CodeSecretDetected {
			code = sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_SECRET_DETECTED
		}
		return &sidecarv1.SnapshotError{Code: code, Message: pe.Message, Paths: pe.Paths}
	}
	return &sidecarv1.SnapshotError{Code: sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_GIT_FAILED, Message: err.Error()}
}

func internalErr(err error) *sidecarv1.SnapshotError {
	return &sidecarv1.SnapshotError{Code: sidecarv1.SnapshotErrorCode_SNAPSHOT_ERROR_CODE_INTERNAL, Message: err.Error()}
}
