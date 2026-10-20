package sidecar

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	brokerv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/broker/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
)

// Restore fills cfg.Root with this epoch's files: the base commit plus the
// last committed snapshot, as the broker's restore plan says. It runs as an
// init container, so the workload only starts once it has returned.
//
// It is safe to run again. A live volume (a pod restart, or a reused disk)
// is left alone; a half-finished restore is wiped and redone.
func Restore(ctx context.Context, cfg Config, broker brokerv1.BrokerServiceClient) error {
	cfg.defaults()
	vol, found, err := ReadVolume(cfg.Root)
	if err != nil {
		return fmt.Errorf("reading volume record: %w", err)
	}
	if found && vol.State == StateLive {
		cfg.Log.Info("volume is live; nothing to restore", "epoch", vol.Epoch)
		return nil
	}
	if err := WriteVolume(cfg.Root, Volume{Epoch: cfg.Epoch, State: StateRestoring}); err != nil {
		return err
	}
	if err := clearWorkspace(cfg.Root); err != nil {
		return fmt.Errorf("clearing a partial restore: %w", err)
	}

	plan, err := broker.GetRestorePlan(ctx, &brokerv1.GetRestorePlanRequest{})
	if err != nil {
		return fmt.Errorf("getting restore plan: %w", err)
	}

	work := filepath.Join(cfg.Scratch, "restore")
	if err := os.RemoveAll(work); err != nil {
		return err
	}
	defer os.RemoveAll(work)

	opts := snapshot.RestoreOptions{
		OriginURL: plan.GetOriginUrl(),
		Base:      plan.GetBaseSha(),
		Branch:    plan.GetBranch(),
		Depth:     int(plan.GetDepth()),
	}
	var bytes int64
	if b := plan.GetBaseBundle(); b != nil {
		opts.BaseBundle = filepath.Join(work, "base.bundle")
		n, err := download(ctx, cfg.HTTP, b.GetMethod(), b.GetUrl(), b.GetHeaders(), opts.BaseBundle, "")
		if err != nil {
			return fmt.Errorf("downloading base bundle: %w", err)
		}
		bytes += n
	}
	if b := plan.GetSnapshotBundle(); b != nil {
		opts.Snapshot = filepath.Join(work, "snapshot.bundle")
		n, err := download(ctx, cfg.HTTP, b.GetMethod(), b.GetUrl(), b.GetHeaders(), opts.Snapshot, plan.GetSnapshotSha256())
		if err != nil {
			return fmt.Errorf("downloading snapshot bundle: %w", err)
		}
		bytes += n
	}

	// Restore into a staging directory on the same disk, then move it in.
	staging := filepath.Join(metaDir(cfg.Root), "staging")
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := snapshot.Restore(ctx, staging, opts); err != nil {
		return err
	}
	if err := moveContents(staging, cfg.Root); err != nil {
		return fmt.Errorf("moving restored files into place: %w", err)
	}
	if err := os.Remove(staging); err != nil {
		return err
	}
	if err := hideMetaDir(cfg.Root); err != nil {
		return err
	}

	err = WriteVolume(cfg.Root, Volume{
		Epoch:             cfg.Epoch,
		State:             StateLive,
		FilesReadyAt:      cfg.Now().UTC(),
		RestoredBytes:     bytes,
		PendingBaseUpload: plan.GetUploadBase(),
		BaseSHA:           plan.GetBaseSha(),
	})
	if err != nil {
		return err
	}
	cfg.Log.Info("files restored", "epoch", cfg.Epoch, "bytes", bytes,
		"fromSnapshot", opts.Snapshot != "", "baseFromBundle", opts.BaseBundle != "")
	return nil
}

// hideMetaDir keeps sbx's own directory out of the user's `git status` via
// the repository's local exclude file (never committed).
func hideMetaDir(root string) error {
	path := filepath.Join(root, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString("\n/" + snapshot.MetaDir + "/\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// clearWorkspace removes everything in root except sbx's own directory.
func clearWorkspace(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == snapshot.MetaDir {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// moveContents renames every entry of src into dst (same filesystem).
func moveContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		to := filepath.Join(dst, e.Name())
		if _, err := os.Lstat(to); err == nil {
			return fmt.Errorf("%s already exists", to)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(filepath.Join(src, e.Name()), to); err != nil {
			return err
		}
	}
	return nil
}
