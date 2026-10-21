// Package snapstore knows where workspace state lives in S3 and owns the
// per-workspace manifest that names the latest committed snapshot.
//
// Layout:
//
//	ws/<uid>/manifest.json         latest snapshot; replaced only by compare-and-swap
//	ws/<uid>/b/<sha256>.bundle     snapshot bundles, content addressed, written once
//	bases/<urlhash>/<sha>.bundle   cached upstream base commits, written once
//	deps/<profile>/<key>.tar.zst   dependency directories, shared, written once
//
// The manifest only moves forward: a commit names the sequence number it
// builds on, and loses (ErrStale) if a newer snapshot got there first.
package snapstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
)

// ManifestVersion is the manifest format version.
const ManifestVersion = 1

var (
	// ErrStale means a newer snapshot is already committed; the caller's
	// snapshot must not replace it.
	ErrStale = errors.New("snapstore: a newer snapshot is already committed")
	// ErrBundleMismatch means the bundle is missing or differs from what the
	// sidecar reported.
	ErrBundleMismatch = errors.New("snapstore: bundle missing or does not match")
)

// Manifest names a workspace's latest committed snapshot.
type Manifest struct {
	Version   int       `json:"version"`
	Workspace string    `json:"workspace"`
	Seq       int64     `json:"seq"`
	Epoch     int64     `json:"epoch"`
	AttemptID string    `json:"attemptID"`
	WIPSHA    string    `json:"wipSHA"`
	BundleKey string    `json:"bundleKey"`
	SHA256    string    `json:"sha256"`
	Bytes     int64     `json:"bytes"`
	Base      Base      `json:"base"`
	CreatedAt time.Time `json:"createdAt"`
}

// Base is the upstream commit a workspace started from.
type Base struct {
	URL       string `json:"url"`
	SHA       string `json:"sha"`
	Branch    string `json:"branch,omitempty"`
	BundleKey string `json:"bundleKey,omitempty"`
	Shallow   bool   `json:"shallow,omitempty"`
}

// ManifestKey is the manifest's object key.
func ManifestKey(workspace string) string { return "ws/" + workspace + "/manifest.json" }

// BundleKey is a snapshot bundle's object key.
func BundleKey(workspace, sha256Hex string) string {
	return "ws/" + workspace + "/b/" + sha256Hex + ".bundle"
}

// BaseBundleKey is the object key of a cached base commit.
func BaseBundleKey(repoURL, sha string) string {
	h := sha256.Sum256([]byte(repoURL))
	return "bases/" + hex.EncodeToString(h[:16]) + "/" + sha + ".bundle"
}

// Kinds of stored objects, by key prefix.
const (
	KindSnapshots = "snapshots" // ws/: manifests and snapshot bundles
	KindBases     = "bases"     // bases/: cached upstream commits
	KindDeps      = "deps"      // deps/: dependency archives
)

// prefixes are where each kind lives.
var prefixes = map[string]string{KindSnapshots: "ws/", KindBases: "bases/", KindDeps: "deps/"}

// KindOf is the kind of the object at key, or "other".
func KindOf(key string) string {
	for kind, prefix := range prefixes {
		if strings.HasPrefix(key, prefix) {
			return kind
		}
	}
	return "other"
}

// Usage is how much is stored of one kind.
type Usage struct{ Bytes, Objects int64 }

// Usage lists the store and adds up what each kind takes.
func (s *Store) Usage(ctx context.Context) (map[string]Usage, error) {
	out := map[string]Usage{}
	for kind, prefix := range prefixes {
		objs, err := s.obj.List(ctx, prefix)
		if err != nil {
			return nil, err
		}
		var u Usage
		for _, o := range objs {
			u.Bytes += o.Size
			u.Objects++
		}
		out[kind] = u
	}
	return out, nil
}

// DepsKey is the object key of a dependency archive (see package depcache).
func DepsKey(profile, key string) string { return "deps/" + profile + "/" + key + ".tar.zst" }

// Store reads and commits manifests.
type Store struct {
	obj objstore.Store
	now func() time.Time
}

// New returns a Store backed by obj.
func New(obj objstore.Store) *Store { return &Store{obj: obj, now: time.Now} }

// Latest returns the committed manifest; ok is false if there is none yet.
func (s *Store) Latest(ctx context.Context, workspace string) (m Manifest, ok bool, err error) {
	m, _, ok, err = s.load(ctx, workspace)
	return m, ok, err
}

func (s *Store) load(ctx context.Context, workspace string) (Manifest, string, bool, error) {
	rc, obj, err := s.obj.Get(ctx, ManifestKey(workspace))
	if errors.Is(err, objstore.ErrNotFound) {
		return Manifest{}, "", false, nil
	}
	if err != nil {
		return Manifest{}, "", false, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return Manifest{}, "", false, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, "", false, fmt.Errorf("snapstore: corrupt manifest for %s: %w", workspace, err)
	}
	return m, obj.ETag, true, nil
}

// VerifyBundle checks that key holds an object of the given size and SHA-256.
func (s *Store) VerifyBundle(ctx context.Context, key, sha256Hex string, size int64) error {
	h, err := s.obj.Head(ctx, key)
	if errors.Is(err, objstore.ErrNotFound) {
		return fmt.Errorf("%w: %s not found", ErrBundleMismatch, key)
	}
	if err != nil {
		return err
	}
	if h.Size != size || h.SHA256 != sha256Hex {
		return fmt.Errorf("%w: %s is %d bytes sha256 %q, want %d bytes sha256 %q",
			ErrBundleMismatch, key, h.Size, h.SHA256, size, sha256Hex)
	}
	return nil
}

// Commit makes next the latest snapshot, building on prevSeq (the sequence
// number the caller believes is current; 0 for none). It verifies the bundle
// first and sets next.Seq to prevSeq+1.
//
// Commit is safe to retry: if the manifest already holds this attempt, it
// returns it. It returns ErrStale if a different snapshot already moved the
// manifest past prevSeq, or if next comes from an older epoch.
func (s *Store) Commit(ctx context.Context, prevSeq int64, next Manifest) (Manifest, error) {
	if next.Workspace == "" || next.AttemptID == "" {
		return Manifest{}, errors.New("snapstore: Workspace and AttemptID are required")
	}
	if err := s.VerifyBundle(ctx, next.BundleKey, next.SHA256, next.Bytes); err != nil {
		return Manifest{}, err
	}
	next.Version = ManifestVersion
	next.Seq = prevSeq + 1
	if next.CreatedAt.IsZero() {
		next.CreatedAt = s.now().UTC()
	}

	// Each lost compare-and-swap means someone else wrote; re-read and decide.
	for range 5 {
		cur, etag, found, err := s.load(ctx, next.Workspace)
		if err != nil {
			return Manifest{}, err
		}
		if found && cur.AttemptID == next.AttemptID && cur.BundleKey == next.BundleKey {
			return cur, nil // this attempt already landed
		}
		if found && (cur.Seq != prevSeq || cur.Epoch > next.Epoch) {
			return cur, ErrStale
		}
		if !found && prevSeq != 0 {
			return Manifest{}, fmt.Errorf("snapstore: manifest for %s is missing but caller expected seq %d", next.Workspace, prevSeq)
		}

		body, err := json.Marshal(next)
		if err != nil {
			return Manifest{}, err
		}
		cond := objstore.Cond{IfNoneMatch: !found, IfMatch: etag}
		_, err = s.obj.Put(ctx, ManifestKey(next.Workspace), bytes.NewReader(body), int64(len(body)), cond)
		if errors.Is(err, objstore.ErrPrecondition) {
			continue
		}
		if err != nil {
			return Manifest{}, err
		}
		return next, nil
	}
	return Manifest{}, fmt.Errorf("snapstore: manifest for %s kept changing; giving up", next.Workspace)
}
