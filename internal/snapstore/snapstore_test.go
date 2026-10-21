package snapstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

const ws = "uid-1234"

// putBundle stores a bundle and returns a manifest pointing at it.
func putBundle(t *testing.T, obj objstore.Store, attempt string, epoch int64, body string) snapstore.Manifest {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	hexSum := hex.EncodeToString(sum[:])
	key := snapstore.BundleKey(ws, hexSum)
	if _, err := obj.Put(context.Background(), key, bytes.NewReader([]byte(body)), int64(len(body)),
		objstore.Cond{SHA256: hexSum}); err != nil && !errors.Is(err, objstore.ErrPrecondition) {
		t.Fatal(err)
	}
	return snapstore.Manifest{
		Workspace: ws, Epoch: epoch, AttemptID: attempt, WIPSHA: "wip-" + attempt,
		BundleKey: key, SHA256: hexSum, Bytes: int64(len(body)),
	}
}

func TestCommitAdvancesAndRetriesAreIdempotent(t *testing.T) {
	ctx := context.Background()
	obj := objstore.NewMem()
	s := snapstore.New(obj)

	if _, ok, err := s.Latest(ctx, ws); err != nil || ok {
		t.Fatalf("Latest on an empty store = ok %v, err %v", ok, err)
	}
	first := putBundle(t, obj, "a1", 1, "bundle one")
	m, err := s.Commit(ctx, 0, first)
	if err != nil || m.Seq != 1 {
		t.Fatalf("first commit: seq %d, err %v", m.Seq, err)
	}
	// The controller crashed after the write and retries the same attempt.
	if m, err = s.Commit(ctx, 0, first); err != nil || m.Seq != 1 {
		t.Fatalf("retried commit: seq %d, err %v; want seq 1 and no error", m.Seq, err)
	}
	second := putBundle(t, obj, "a2", 2, "bundle two")
	if m, err = s.Commit(ctx, 1, second); err != nil || m.Seq != 2 {
		t.Fatalf("second commit: seq %d, err %v", m.Seq, err)
	}
	got, ok, err := s.Latest(ctx, ws)
	if err != nil || !ok || got.AttemptID != "a2" || got.Seq != 2 {
		t.Fatalf("Latest = %+v, ok %v, err %v; want attempt a2 at seq 2", got, ok, err)
	}
}

func TestOlderSnapshotNeverReplacesNewer(t *testing.T) {
	ctx := context.Background()
	obj := objstore.NewMem()
	s := snapstore.New(obj)
	if _, err := s.Commit(ctx, 0, putBundle(t, obj, "a1", 1, "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, 1, putBundle(t, obj, "a2", 2, "two")); err != nil {
		t.Fatal(err)
	}

	// A late attempt that started before seq 2 existed.
	if _, err := s.Commit(ctx, 1, putBundle(t, obj, "late", 2, "late")); !errors.Is(err, snapstore.ErrStale) {
		t.Errorf("commit building on an old seq: err = %v, want ErrStale", err)
	}
	// A zombie pod from an older epoch.
	if _, err := s.Commit(ctx, 2, putBundle(t, obj, "zombie", 1, "zombie")); !errors.Is(err, snapstore.ErrStale) {
		t.Errorf("commit from an older epoch: err = %v, want ErrStale", err)
	}
	if got, _, _ := s.Latest(ctx, ws); got.AttemptID != "a2" {
		t.Errorf("latest = %s, want a2 untouched", got.AttemptID)
	}
}

func TestCommitRefusesMissingOrMismatchedBundle(t *testing.T) {
	ctx := context.Background()
	obj := objstore.NewMem()
	s := snapstore.New(obj)

	m := putBundle(t, obj, "a1", 1, "real bundle")
	missing := m
	missing.BundleKey = snapstore.BundleKey(ws, "0000")
	if _, err := s.Commit(ctx, 0, missing); !errors.Is(err, snapstore.ErrBundleMismatch) {
		t.Errorf("missing bundle: err = %v, want ErrBundleMismatch", err)
	}
	wrongSize := m
	wrongSize.Bytes++
	if _, err := s.Commit(ctx, 0, wrongSize); !errors.Is(err, snapstore.ErrBundleMismatch) {
		t.Errorf("wrong size: err = %v, want ErrBundleMismatch", err)
	}
	if _, ok, _ := s.Latest(ctx, ws); ok {
		t.Error("a manifest was written for an unverified bundle")
	}
}

func TestConcurrentCommitsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	obj := objstore.NewMem()
	s := snapstore.New(obj)
	if _, err := s.Commit(ctx, 0, putBundle(t, obj, "base", 1, "base")); err != nil {
		t.Fatal(err)
	}

	const n = 16
	manifests := make([]snapstore.Manifest, n)
	for i := range manifests {
		manifests[i] = putBundle(t, obj, fmt.Sprintf("racer-%d", i), 1, fmt.Sprintf("body-%d", i))
	}
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = s.Commit(ctx, 1, manifests[i])
		}()
	}
	wg.Wait()

	wins := 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, snapstore.ErrStale):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d of %d concurrent commits won, want exactly 1", wins, n)
	}
}

func TestKeys(t *testing.T) {
	if got := snapstore.BundleKey("u1", "abc"); got != "ws/u1/b/abc.bundle" {
		t.Errorf("BundleKey = %s", got)
	}
	a := snapstore.BaseBundleKey("https://github.com/a/b.git", "sha1")
	b := snapstore.BaseBundleKey("https://github.com/a/c.git", "sha1")
	if a == b {
		t.Error("different repositories share a base bundle key")
	}
	for key, kind := range map[string]string{
		snapstore.BundleKey("u1", "abc"): snapstore.KindSnapshots,
		a:                                snapstore.KindBases,
		snapstore.DepsKey("npm", "k"):    snapstore.KindDeps,
		"elsewhere/x":                    "other",
	} {
		if got := snapstore.KindOf(key); got != kind {
			t.Errorf("KindOf(%s) = %s, want %s", key, got, kind)
		}
	}
}

func TestUsageByKind(t *testing.T) {
	ctx := context.Background()
	obj := objstore.NewMem()
	put := func(key string, n int) {
		t.Helper()
		if _, err := obj.Put(ctx, key, bytes.NewReader(make([]byte, n)), int64(n), objstore.Cond{}); err != nil {
			t.Fatal(err)
		}
	}
	put(snapstore.BundleKey("uid-a", strings.Repeat("a", 64)), 100)
	put(snapstore.ManifestKey("uid-a"), 10)
	put(snapstore.BaseBundleKey("https://example.com/r.git", "abc"), 1000)
	put(snapstore.DepsKey("npm", strings.Repeat("b", 64)), 5000)
	put(snapstore.DepsKey("pip", strings.Repeat("c", 64)), 7000)

	u, err := snapstore.New(obj).Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]snapstore.Usage{
		snapstore.KindSnapshots: {Bytes: 110, Objects: 2},
		snapstore.KindBases:     {Bytes: 1000, Objects: 1},
		snapstore.KindDeps:      {Bytes: 12000, Objects: 2},
	}
	for k, w := range want {
		if u[k] != w {
			t.Errorf("%s = %+v, want %+v", k, u[k], w)
		}
	}
}
