package objstore_test

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore/objstoretest"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestMemMeetsContract(t *testing.T) {
	mem := objstore.NewMem()
	srv := httptest.NewServer(mem.Handler())
	t.Cleanup(srv.Close)
	mem.SetBaseURL(srv.URL)

	objstoretest.Run(t, mem, "ws/test/", 32)
}

func TestObservedSeesEveryCall(t *testing.T) {
	mem := objstore.NewMem()
	srv := httptest.NewServer(mem.Handler())
	t.Cleanup(srv.Close)
	mem.SetBaseURL(srv.URL)
	var mu sync.Mutex
	seen := map[objstore.Op]int{}
	s := objstore.Observed(mem, func(op objstore.Op, _ string) {
		mu.Lock()
		seen[op]++
		mu.Unlock()
	})

	// The wrapper changes nothing the store does...
	objstoretest.Run(t, s, "ws/test/", 8)

	// ...and reports each call once, with its key.
	var keys []string
	exact := objstore.Observed(mem, func(op objstore.Op, key string) { keys = append(keys, string(op)+" "+key) })
	ctx := context.Background()
	if _, err := exact.Put(ctx, "deps/npm/k.tar.zst", strings.NewReader("x"), 1, objstore.Cond{}); err != nil {
		t.Fatal(err)
	}
	_, _ = exact.Head(ctx, "deps/npm/k.tar.zst")
	_, _ = exact.Head(ctx, "deps/npm/missing") // failed calls count too
	_, _ = exact.PresignGet(ctx, "deps/npm/k.tar.zst", time.Minute)
	_, _ = exact.List(ctx, "deps/")
	_ = exact.Delete(ctx, "deps/npm/k.tar.zst")
	want := []string{"put deps/npm/k.tar.zst", "head deps/npm/k.tar.zst", "head deps/npm/missing",
		"presign_get deps/npm/k.tar.zst", "list deps/", "delete deps/npm/k.tar.zst"}
	if !slices.Equal(keys, want) {
		t.Errorf("observed %q, want %q", keys, want)
	}
	for _, op := range []objstore.Op{objstore.OpPut, objstore.OpGet, objstore.OpHead, objstore.OpList, objstore.OpPresignPut, objstore.OpPresignGet} {
		if seen[op] == 0 {
			t.Errorf("the contract run made no %s call through the wrapper", op)
		}
	}
}
