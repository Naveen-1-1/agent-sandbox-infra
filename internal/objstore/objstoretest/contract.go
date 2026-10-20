// Package objstoretest is a behavioural contract every objstore.Store must
// meet. The in-memory fake runs it in unit tests; the real S3 endpoint runs it
// in e2e tests, which is how we know the fake is faithful.
package objstoretest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
)

// Run exercises store under keys starting with prefix. Concurrency tests use
// writers goroutines.
func Run(t *testing.T, store objstore.Store, prefix string, writers int) {
	t.Run("PutGetHead", func(t *testing.T) { putGetHead(t, store, prefix) })
	t.Run("MissingIsNotFound", func(t *testing.T) { missing(t, store, prefix) })
	t.Run("IfNoneMatchCreatesOnce", func(t *testing.T) { ifNoneMatch(t, store, prefix) })
	t.Run("IfMatchCompareAndSwap", func(t *testing.T) { ifMatch(t, store, prefix) })
	t.Run("ConcurrentCreateHasOneWinner", func(t *testing.T) { concurrentCreate(t, store, prefix, writers) })
	t.Run("ConcurrentSwapHasOneWinner", func(t *testing.T) { concurrentSwap(t, store, prefix, writers) })
	t.Run("ChecksumMismatchRejected", func(t *testing.T) { badChecksum(t, store, prefix) })
	t.Run("PresignedPutAndGet", func(t *testing.T) { presigned(t, store, prefix) })
	t.Run("PresignedChecksumMismatchRejected", func(t *testing.T) { presignedBadChecksum(t, store, prefix) })
	t.Run("ListAndDelete", func(t *testing.T) { listDelete(t, store, prefix) })
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func put(t *testing.T, s objstore.Store, key string, body []byte, c objstore.Cond) (objstore.Object, error) {
	t.Helper()
	return s.Put(ctx(t), key, bytes.NewReader(body), int64(len(body)), c)
}

func read(t *testing.T, s objstore.Store, key string) []byte {
	t.Helper()
	rc, _, err := s.Get(ctx(t), key)
	if err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func putGetHead(t *testing.T, s objstore.Store, prefix string) {
	key, body := prefix+"blob", []byte("hello bundle")
	if _, err := put(t, s, key, body, objstore.Cond{SHA256: sum(body)}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, key); !bytes.Equal(got, body) {
		t.Errorf("Get = %q, want %q", got, body)
	}
	h, err := s.Head(ctx(t), key)
	if err != nil {
		t.Fatal(err)
	}
	if h.Size != int64(len(body)) || h.SHA256 != sum(body) || h.ETag == "" {
		t.Errorf("Head = %+v, want size %d, sha256 %s and an ETag", h, len(body), sum(body))
	}
}

func missing(t *testing.T, s objstore.Store, prefix string) {
	if _, _, err := s.Get(ctx(t), prefix+"nope"); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("Get missing: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Head(ctx(t), prefix+"nope"); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("Head missing: err = %v, want ErrNotFound", err)
	}
}

func ifNoneMatch(t *testing.T, s objstore.Store, prefix string) {
	key := prefix + "create-once"
	if _, err := put(t, s, key, []byte("first"), objstore.Cond{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, s, key, []byte("second"), objstore.Cond{IfNoneMatch: true}); !errors.Is(err, objstore.ErrPrecondition) {
		t.Errorf("second create: err = %v, want ErrPrecondition", err)
	}
	if got := read(t, s, key); string(got) != "first" {
		t.Errorf("object = %q, want the first write", got)
	}
}

func ifMatch(t *testing.T, s objstore.Store, prefix string) {
	key := prefix + "manifest"
	v1, err := put(t, s, key, []byte(`{"seq":1}`), objstore.Cond{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := put(t, s, key, []byte(`{"seq":2}`), objstore.Cond{IfMatch: v1.ETag})
	if err != nil {
		t.Fatalf("swap with current ETag: %v", err)
	}
	if _, err := put(t, s, key, []byte(`{"seq":1-stale}`), objstore.Cond{IfMatch: v1.ETag}); !errors.Is(err, objstore.ErrPrecondition) {
		t.Errorf("swap with stale ETag: err = %v, want ErrPrecondition", err)
	}
	if _, err := put(t, s, prefix+"never-written", []byte("x"), objstore.Cond{IfMatch: v2.ETag}); !errors.Is(err, objstore.ErrPrecondition) && !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("IfMatch on a missing key: err = %v, want ErrPrecondition or ErrNotFound", err)
	}
	if got := read(t, s, key); string(got) != `{"seq":2}` {
		t.Errorf("manifest = %s, want seq 2", got)
	}
}

// race runs writers concurrent attempts and returns how many succeeded.
func race(t *testing.T, writers int, attempt func(i int) error) int {
	t.Helper()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		other []error
		start = make(chan struct{})
	)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := attempt(i)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, objstore.ErrPrecondition):
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range other {
		t.Errorf("unexpected error (want success or ErrPrecondition): %v", err)
	}
	return wins
}

func concurrentCreate(t *testing.T, s objstore.Store, prefix string, writers int) {
	key := prefix + "contended-create"
	wins := race(t, writers, func(i int) error {
		_, err := put(t, s, key, []byte(fmt.Sprintf("writer-%d", i)), objstore.Cond{IfNoneMatch: true})
		return err
	})
	if wins != 1 {
		t.Errorf("%d of %d concurrent creates succeeded, want exactly 1", wins, writers)
	}
}

func concurrentSwap(t *testing.T, s objstore.Store, prefix string, writers int) {
	key := prefix + "contended-manifest"
	base, err := put(t, s, key, []byte(`{"seq":0}`), objstore.Cond{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	wins := race(t, writers, func(i int) error {
		_, err := put(t, s, key, []byte(fmt.Sprintf(`{"seq":1,"writer":%d}`, i)), objstore.Cond{IfMatch: base.ETag})
		return err
	})
	if wins != 1 {
		t.Errorf("%d of %d concurrent swaps from the same ETag succeeded, want exactly 1", wins, writers)
	}
}

func badChecksum(t *testing.T, s objstore.Store, prefix string) {
	key := prefix + "bad-checksum"
	_, err := put(t, s, key, []byte("actual body"), objstore.Cond{SHA256: sum([]byte("other body"))})
	if err == nil {
		t.Fatal("Put with a wrong SHA-256 succeeded")
	}
	if _, err := s.Head(ctx(t), key); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("object exists after a rejected write (Head err = %v)", err)
	}
}

func do(t *testing.T, r objstore.Request, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx(t), r.Method, r.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func presigned(t *testing.T, s objstore.Store, prefix string) {
	key, body := prefix+"presigned.bundle", []byte("bundle bytes from a pod")
	up, err := s.PresignPut(ctx(t), key, 5*time.Minute, objstore.Cond{IfNoneMatch: true, SHA256: sum(body)})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg := do(t, up, body); code != http.StatusOK {
		t.Fatalf("presigned PUT: %d %s", code, msg)
	}
	again, err := s.PresignPut(ctx(t), key, 5*time.Minute, objstore.Cond{IfNoneMatch: true, SHA256: sum(body)})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, again, body); code != http.StatusPreconditionFailed {
		t.Errorf("second create-only presigned PUT: status %d, want 412", code)
	}
	down, err := s.PresignGet(ctx(t), key, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	code, got := do(t, down, nil)
	if code != http.StatusOK || !bytes.Equal(got, body) {
		t.Errorf("presigned GET: %d %q, want 200 %q", code, got, body)
	}
	h, err := s.Head(ctx(t), key)
	if err != nil {
		t.Fatal(err)
	}
	if h.SHA256 != sum(body) {
		t.Errorf("Head after presigned PUT: sha256 %q, want %q", h.SHA256, sum(body))
	}
}

func presignedBadChecksum(t *testing.T, s objstore.Store, prefix string) {
	key := prefix + "presigned-bad.bundle"
	up, err := s.PresignPut(ctx(t), key, 5*time.Minute, objstore.Cond{IfNoneMatch: true, SHA256: sum([]byte("promised"))})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, up, []byte("tampered")); code < 400 || code >= 500 {
		t.Errorf("presigned PUT with a body that doesn't match its checksum: status %d, want 4xx", code)
	}
	if _, err := s.Head(ctx(t), key); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("object exists after a rejected presigned write (Head err = %v)", err)
	}
}

func listDelete(t *testing.T, s objstore.Store, prefix string) {
	dir := prefix + "list/"
	for _, k := range []string{"a", "b", "c"} {
		if _, err := put(t, s, dir+k, []byte(k), objstore.Cond{}); err != nil {
			t.Fatal(err)
		}
	}
	objs, err := s.List(ctx(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Errorf("List = %d objects, want 3", len(objs))
	}
	if err := s.Delete(ctx(t), dir+"a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx(t), dir+"a"); err != nil {
		t.Errorf("deleting a missing key: %v, want nil", err)
	}
	if _, err := s.Head(ctx(t), dir+"a"); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("Head after Delete: err = %v, want ErrNotFound", err)
	}
}
