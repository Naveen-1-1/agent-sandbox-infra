// Package objstore is the small slice of S3 the platform needs: immutable
// content-addressed blobs (bundles, dependency archives), a per-workspace
// manifest advanced only by conditional writes, and presigned URLs so pods
// can move data without ever holding S3 credentials.
package objstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

var (
	// ErrNotFound means the object does not exist.
	ErrNotFound = errors.New("objstore: object not found")
	// ErrPrecondition means a conditional write lost: the object already
	// existed (IfNoneMatch) or changed since it was read (IfMatch).
	ErrPrecondition = errors.New("objstore: precondition failed")
	// ErrChecksum means the body did not match the declared SHA-256.
	ErrChecksum = errors.New("objstore: checksum mismatch")
)

// Cond makes a write conditional and/or integrity-checked.
type Cond struct {
	// IfNoneMatch writes only if the key does not exist yet.
	IfNoneMatch bool
	// IfMatch writes only if the object's current ETag equals this value.
	IfMatch string
	// SHA256 is the hex SHA-256 of the body. The store rejects a body that
	// doesn't match it, and records it on the object.
	SHA256 string
}

// Object describes a stored object.
type Object struct {
	Key      string
	Size     int64
	ETag     string
	SHA256   string // hex, if the writer provided one
	Modified time.Time
}

// Request is a presigned HTTP request. The client must send exactly these
// headers; they are part of the signature.
type Request struct {
	Method  string
	URL     string
	Header  http.Header
	Expires time.Time
}

// Store is implemented by S3 (production, SeaweedFS locally) and Mem (tests).
type Store interface {
	// Put writes body under key. The body must be seekable so the request
	// can be signed and retried.
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64, c Cond) (Object, error)
	Get(ctx context.Context, key string) (io.ReadCloser, Object, error)
	Head(ctx context.Context, key string) (Object, error)
	Delete(ctx context.Context, key string) error
	// List returns every object whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	PresignPut(ctx context.Context, key string, ttl time.Duration, c Cond) (Request, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (Request, error)
}
