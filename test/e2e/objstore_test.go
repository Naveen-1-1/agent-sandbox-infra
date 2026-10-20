//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore/objstoretest"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

// localS3 connects to the deployed SeaweedFS endpoint through a port-forward.
func localS3(t *testing.T) *objstore.S3 {
	t.Helper()
	ns := framework.Namespace()
	addr := framework.PortForward(t, ns, "svc/sbx-s3", 8333)
	secret := "sbx-s3-credentials"
	store, err := objstore.NewS3(objstore.S3Config{
		Endpoint:  "http://" + addr,
		Bucket:    "sbx",
		AccessKey: framework.SecretValue(t, ns, secret, "accessKey"),
		SecretKey: framework.SecretValue(t, ns, secret, "secretKey"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestLocalS3MeetsContract runs the objstore contract against the deployed
// SeaweedFS endpoint: create-once and compare-and-swap must have exactly one
// winner under contention, and presigned writes must enforce their checksum.
// This is what lets the snapshot manifest live in S3.
func TestLocalS3MeetsContract(t *testing.T) {
	objstoretest.Run(t, localS3(t), fmt.Sprintf("e2e/contract-%d/", time.Now().UnixNano()), 32)
}

// TestManifestCommitRaceOnLocalS3 races snapshot commits against the real
// store: exactly one wins and the rest are told they are stale.
func TestManifestCommitRaceOnLocalS3(t *testing.T) {
	ctx := framework.Ctx(t, 2*time.Minute)
	obj := localS3(t)
	s := snapstore.New(obj)
	ws := fmt.Sprintf("e2e-%d", time.Now().UnixNano())

	bundle := func(name string) snapstore.Manifest {
		sum := sha256.Sum256([]byte(name))
		h := hex.EncodeToString(sum[:])
		key := snapstore.BundleKey(ws, h)
		if _, err := obj.Put(ctx, key, bytes.NewReader([]byte(name)), int64(len(name)), objstore.Cond{SHA256: h}); err != nil {
			t.Fatal(err)
		}
		return snapstore.Manifest{Workspace: ws, Epoch: 1, AttemptID: name, BundleKey: key, SHA256: h, Bytes: int64(len(name))}
	}
	if _, err := s.Commit(ctx, 0, bundle("first")); err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		m := bundle(fmt.Sprintf("racer-%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = commit(ctx, s, m)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
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

func commit(ctx context.Context, s *snapstore.Store, m snapstore.Manifest) error {
	_, err := s.Commit(ctx, 1, m)
	return err
}
