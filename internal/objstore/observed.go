package objstore

import (
	"context"
	"io"
	"time"
)

// Op names one kind of store call, for counting requests.
type Op string

// Store calls. A presign makes no request itself: the pod that gets the URL
// makes one.
const (
	OpPut        Op = "put"
	OpGet        Op = "get"
	OpHead       Op = "head"
	OpDelete     Op = "delete"
	OpList       Op = "list"
	OpPresignPut Op = "presign_put"
	OpPresignGet Op = "presign_get"
)

// Observed wraps s so that observe sees every call, failed ones included,
// since S3 bills those too. A list longer than a page and the SDK's own
// retries are several requests but one call.
func Observed(s Store, observe func(op Op, key string)) Store {
	return &observed{s: s, observe: observe}
}

type observed struct {
	s       Store
	observe func(Op, string)
}

func (o *observed) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, c Cond) (Object, error) {
	o.observe(OpPut, key)
	return o.s.Put(ctx, key, body, size, c)
}

func (o *observed) Get(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	o.observe(OpGet, key)
	return o.s.Get(ctx, key)
}

func (o *observed) Head(ctx context.Context, key string) (Object, error) {
	o.observe(OpHead, key)
	return o.s.Head(ctx, key)
}

func (o *observed) Delete(ctx context.Context, key string) error {
	o.observe(OpDelete, key)
	return o.s.Delete(ctx, key)
}

func (o *observed) List(ctx context.Context, prefix string) ([]Object, error) {
	o.observe(OpList, prefix)
	return o.s.List(ctx, prefix)
}

func (o *observed) PresignPut(ctx context.Context, key string, ttl time.Duration, c Cond) (Request, error) {
	o.observe(OpPresignPut, key)
	return o.s.PresignPut(ctx, key, ttl, c)
}

func (o *observed) PresignGet(ctx context.Context, key string, ttl time.Duration) (Request, error) {
	o.observe(OpPresignGet, key)
	return o.s.PresignGet(ctx, key, ttl)
}
