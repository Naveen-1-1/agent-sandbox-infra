// Package sidecar runs in every workspace pod next to the workload. As an
// init container it restores the workspace files; as a native sidecar it
// takes snapshots for the controller and reports restore progress.
package sidecar

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
)

// Volume states recorded on the workspace disk.
const (
	// StateRestoring means a restore started and has not finished; the next
	// attempt wipes the files and starts over.
	StateRestoring = "restoring"
	// StateLive means the files are restored and hold the user's live work;
	// nothing ever restores over a live volume.
	StateLive = "live"
)

// Volume is the restore record kept at <root>/.sbx/volume.json.
type Volume struct {
	Epoch         int64     `json:"epoch"`
	State         string    `json:"state"`
	FilesReadyAt  time.Time `json:"filesReadyAt,omitzero"`
	RestoredBytes int64     `json:"restoredBytes,omitempty"`
	// PendingBaseUpload is set when no base bundle is cached yet; the
	// sidecar uploads one in the background.
	PendingBaseUpload bool   `json:"pendingBaseUpload,omitempty"`
	BaseSHA           string `json:"baseSHA,omitempty"`
}

func metaDir(root string) string { return filepath.Join(root, snapshot.MetaDir) }

func volumePath(root string) string { return filepath.Join(metaDir(root), "volume.json") }

// ReadVolume returns the restore record; ok is false on a fresh disk.
func ReadVolume(root string) (v Volume, ok bool, err error) {
	b, err := os.ReadFile(volumePath(root))
	if errors.Is(err, os.ErrNotExist) {
		return Volume{}, false, nil
	}
	if err != nil {
		return Volume{}, false, err
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return Volume{}, false, err
	}
	return v, true, nil
}

// WriteVolume replaces the restore record atomically.
func WriteVolume(root string, v Volume) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(volumePath(root), b)
}

// writeFileAtomic writes data to path via a temp file, fsync and rename, so a
// crash leaves either the old or the new content.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
