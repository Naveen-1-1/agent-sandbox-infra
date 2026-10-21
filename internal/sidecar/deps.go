package sidecar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	brokerv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/broker/v1"
	sidecarv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/sidecar/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/depcache"
)

// Installer runs a profile's install command in the workload container,
// where the language runtime and package manager are.
type Installer interface {
	Install(ctx context.Context, command, dir string) (exitCode int, output string, err error)
}

// Deps restores this epoch's dependency directory once the files are in
// place: from the shared cache when an archive for the same lockfile,
// image and arch exists, otherwise by running the install. The workload
// keeps serving meanwhile; only commands that need the directory wait.
//
// The state is kept on the disk, so a restarted sidecar reports it, and
// resumes one that was cut off.
type Deps struct {
	cfg    Config
	broker brokerv1.BrokerServiceClient

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	st      *sidecarv1.Deps
	running bool
}

// NewDeps returns a Deps; call Close to stop a running restore.
func NewDeps(cfg Config, broker brokerv1.BrokerServiceClient) *Deps {
	cfg.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Deps{cfg: cfg, broker: broker, ctx: ctx, cancel: cancel}
}

// Close stops a running restore and waits for it.
func (d *Deps) Close() {
	d.cancel()
	d.wg.Wait()
}

func (d *Deps) statePath() string { return filepath.Join(metaDir(d.cfg.Root), "deps.json") }

// Start begins the restore, or reports the one already started.
func (d *Deps) Start(r *sidecarv1.StartDepsRequest) (*sidecarv1.Deps, error) {
	if r.GetEpoch() != d.cfg.Epoch {
		return nil, fmt.Errorf("request is for epoch %d but this pod serves epoch %d", r.GetEpoch(), d.cfg.Epoch)
	}
	if r.GetImageId() == "" {
		return nil, errors.New("image_id is required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st == nil {
		d.st = d.load()
	}
	if st := d.st; st != nil && (d.running || terminal(st.GetState())) {
		return proto.Clone(st).(*sidecarv1.Deps), nil
	}
	// An install cut off by a restart left a partial directory: install
	// again rather than take it as present.
	reinstall := d.st.GetState() == sidecarv1.DepsState_DEPS_STATE_INSTALLING

	now := timestamppb.New(d.cfg.Now())
	p, lockSHA, ok, err := depcache.Detect(d.cfg.Root)
	switch {
	case err != nil:
		d.st = &sidecarv1.Deps{State: sidecarv1.DepsState_DEPS_STATE_FAILED, Message: err.Error(), StartedAt: now, FinishedAt: now}
	case !ok:
		d.st = &sidecarv1.Deps{State: sidecarv1.DepsState_DEPS_STATE_READY, Source: sidecarv1.DepsSource_DEPS_SOURCE_NONE, StartedAt: now, FinishedAt: now}
	default:
		d.st = &sidecarv1.Deps{
			State: sidecarv1.DepsState_DEPS_STATE_RESTORING, Profile: p.Name, LockSha256: lockSHA,
			Key: depcache.Key(p.Name, lockSHA, r.GetImageId(), d.cfg.Arch), StartedAt: now,
		}
		d.running = true
		d.wg.Add(1)
		go d.run(p, reinstall)
	}
	d.save()
	return proto.Clone(d.st).(*sidecarv1.Deps), nil
}

func terminal(s sidecarv1.DepsState) bool {
	return s == sidecarv1.DepsState_DEPS_STATE_READY || s == sidecarv1.DepsState_DEPS_STATE_FAILED
}

func (d *Deps) run(p depcache.Profile, reinstall bool) {
	defer d.wg.Done()
	defer func() {
		d.mu.Lock()
		d.running = false
		d.mu.Unlock()
	}()
	if !reinstall {
		target := filepath.Join(d.cfg.Root, p.Dir)
		if _, err := os.Lstat(target); err == nil {
			d.finish(sidecarv1.DepsSource_DEPS_SOURCE_PRESENT, 0, "")
			return
		}
		hit, n, err := d.fromCache(p, target)
		switch {
		case err != nil:
			d.cfg.Log.Warn("restoring dependencies from the cache failed; installing instead", "profile", p.Name, "err", err)
		case hit:
			d.finish(sidecarv1.DepsSource_DEPS_SOURCE_CACHE, n, "")
			return
		}
	}

	d.mu.Lock()
	d.st.State = sidecarv1.DepsState_DEPS_STATE_INSTALLING
	d.save()
	d.mu.Unlock()
	if d.cfg.Installer == nil {
		d.fail("no installer configured")
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, d.cfg.DepsInstallTimeout)
	defer cancel()
	code, out, err := d.cfg.Installer.Install(ctx, p.Install, d.cfg.Root)
	switch {
	case err != nil:
		d.fail(fmt.Sprintf("%s: %v", p.Install, err))
	case code != 0:
		d.fail(fmt.Sprintf("%s exited with %d:\n%s", p.Install, code, tail(out, 2048)))
	default:
		d.finish(sidecarv1.DepsSource_DEPS_SOURCE_INSTALL, 0, tail(out, 2048))
	}
}

// fromCache downloads and extracts the cached archive into target, if
// there is one. A directory that appears meanwhile is left alone.
func (d *Deps) fromCache(p depcache.Profile, target string) (hit bool, n int64, err error) {
	d.mu.Lock()
	key := &brokerv1.DepsKey{Profile: p.Name, Key: d.st.GetKey()}
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(d.ctx, d.cfg.DepsInstallTimeout)
	defer cancel()
	resp, err := d.broker.LookupDeps(ctx, &brokerv1.LookupDepsRequest{Keys: []*brokerv1.DepsKey{key}})
	if err != nil {
		return false, 0, err
	}
	if len(resp.GetEntries()) != 1 || !resp.GetEntries()[0].GetHit() {
		return false, 0, nil
	}
	e := resp.GetEntries()[0]
	archive := filepath.Join(d.cfg.Scratch, "deps-"+key.GetKey()+".tar.zst")
	defer os.Remove(archive)
	dl := e.GetDownload()
	if n, err = download(ctx, d.cfg.HTTP, dl.GetMethod(), dl.GetUrl(), dl.GetHeaders(), archive, e.GetSha256()); err != nil {
		return false, 0, err
	}
	f, err := os.Open(archive)
	if err != nil {
		return false, 0, err
	}
	defer f.Close()

	// Extract next to the target and rename, so the directory appears
	// whole or not at all.
	staging := filepath.Join(metaDir(d.cfg.Root), "deps-staging")
	if err := os.RemoveAll(staging); err != nil {
		return false, 0, err
	}
	if _, err := depcache.Extract(ctx, f, staging, d.cfg.DepsLimits); err != nil {
		_ = os.RemoveAll(staging)
		return false, 0, err
	}
	if _, err := os.Lstat(target); err == nil {
		_ = os.RemoveAll(staging)
		return true, n, nil // something created it meanwhile; keep theirs
	}
	return true, n, os.Rename(staging, target)
}

func (d *Deps) finish(src sidecarv1.DepsSource, n int64, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.st.State, d.st.Source, d.st.Bytes, d.st.Message = sidecarv1.DepsState_DEPS_STATE_READY, src, n, msg
	d.st.FinishedAt = timestamppb.New(d.cfg.Now())
	d.save()
	d.cfg.Log.Info("dependencies ready", "profile", d.st.GetProfile(), "source", src.String(), "bytes", n)
}

func (d *Deps) fail(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.st.State, d.st.Message = sidecarv1.DepsState_DEPS_STATE_FAILED, msg
	d.st.FinishedAt = timestamppb.New(d.cfg.Now())
	d.save()
	d.cfg.Log.Warn("dependency restore failed", "profile", d.st.GetProfile(), "message", msg)
}

// save records the state on the disk. Callers hold mu.
func (d *Deps) save() {
	b, err := protojson.Marshal(d.st)
	if err == nil {
		err = writeFileAtomic(d.statePath(), b)
	}
	if err != nil {
		d.cfg.Log.Error("recording dependency state", "err", err)
	}
}

func (d *Deps) load() *sidecarv1.Deps {
	b, err := os.ReadFile(d.statePath())
	if err != nil {
		return nil
	}
	st := &sidecarv1.Deps{}
	if protojson.Unmarshal(b, st) != nil {
		return nil
	}
	return st
}

// Upload caches the dependency directory as it is now, if it came from an
// install this sidecar ran against the lockfile that is still in place:
// that is the only state known to match its key. It runs inside a
// snapshot, while the workload is frozen, and never fails it.
func (d *Deps) Upload(ctx context.Context) *sidecarv1.DepsUpload {
	d.mu.Lock()
	st := d.st
	if st == nil {
		st = d.load()
		d.st = st
	}
	if st != nil {
		st = proto.Clone(st).(*sidecarv1.Deps)
	}
	d.mu.Unlock()
	if st == nil || st.GetProfile() == "" {
		return nil
	}
	up := &sidecarv1.DepsUpload{Key: st.GetKey()}
	p, _ := depcache.Lookup(st.GetProfile())
	switch {
	case st.GetState() != sidecarv1.DepsState_DEPS_STATE_READY:
		up.Skipped = "the dependencies weren't ready"
	case st.GetSource() == sidecarv1.DepsSource_DEPS_SOURCE_CACHE:
		up.Skipped = "restored from the cache"
	case st.GetSource() != sidecarv1.DepsSource_DEPS_SOURCE_INSTALL:
		up.Skipped = "not installed here"
	case st.GetUploaded():
		up.Skipped = "already uploaded"
	default:
		if lock, err := p.LockSHA(d.cfg.Root); err != nil || lock != st.GetLockSha256() {
			up.Skipped = "the lockfile changed since the install"
		}
	}
	if up.Skipped != "" {
		return up
	}

	ctx, cancel := context.WithTimeout(ctx, d.cfg.DepsUploadTimeout)
	defer cancel()
	archive := filepath.Join(d.cfg.Scratch, "deps-upload.tar.zst")
	defer os.Remove(archive)
	size, sum, err := packTo(ctx, filepath.Join(d.cfg.Root, p.Dir), archive)
	if err != nil {
		up.Error = err.Error()
		return up
	}
	up.SizeBytes = size
	if max := d.cfg.DepsMaxBytes; max > 0 && size > max {
		up.Skipped = fmt.Sprintf("the archive is %d bytes, over the %d-byte limit", size, max)
		return up
	}
	resp, err := d.broker.PresignDepsUpload(ctx, &brokerv1.PresignDepsUploadRequest{
		Key: &brokerv1.DepsKey{Profile: p.Name, Key: st.GetKey()}, Sha256: sum, SizeBytes: size,
	})
	if err != nil {
		up.Error = err.Error()
		return up
	}
	if resp.GetAlreadyExists() {
		up.Skipped = "another pod uploaded it first"
	} else {
		u := resp.GetUpload()
		if err := upload(ctx, d.cfg.HTTP, u.GetMethod(), u.GetUrl(), u.GetHeaders(), archive); err != nil {
			up.Error = err.Error()
			return up
		}
		up.Uploaded = true
	}
	d.mu.Lock()
	d.st.Uploaded = true
	d.save()
	d.mu.Unlock()
	return up
}

// packTo writes dir's archive to path and returns its size and SHA-256.
func packTo(ctx context.Context, dir, path string) (int64, string, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(f, h)}
	_, err = depcache.Pack(ctx, dir, cw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return cw.n, hex.EncodeToString(h.Sum(nil)), err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
