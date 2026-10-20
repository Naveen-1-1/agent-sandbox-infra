package objstore

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // ETags only, as S3 does
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mem is an in-memory Store with the same conditional-write and presigned-URL
// semantics as S3, for unit tests. Presigned URLs are served by Handler.
type Mem struct {
	mu      sync.Mutex
	objects map[string]memObject
	grants  map[string]grant
	baseURL string
	now     func() time.Time
}

type memObject struct {
	data     []byte
	etag     string
	sha256   string
	modified time.Time
}

type grant struct {
	method  string
	key     string
	cond    Cond
	expires time.Time
}

// NewMem returns an empty store. Call SetBaseURL with the address Handler is
// served on before presigning.
func NewMem() *Mem {
	return &Mem{objects: map[string]memObject{}, grants: map[string]grant{}, now: time.Now}
}

// SetBaseURL sets the address presigned URLs point at.
func (m *Mem) SetBaseURL(u string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.baseURL = strings.TrimSuffix(u, "/")
}

func (m *Mem) Put(_ context.Context, key string, body io.ReadSeeker, _ int64, c Cond) (Object, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return Object{}, err
	}
	return m.put(key, data, c)
}

func (m *Mem) put(key string, data []byte, c Cond) (Object, error) {
	if c.SHA256 != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != c.SHA256 {
			return Object{}, ErrChecksum
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, exists := m.objects[key]
	if c.IfNoneMatch && exists {
		return Object{}, ErrPrecondition
	}
	if c.IfMatch != "" && (!exists || cur.etag != c.IfMatch) {
		return Object{}, ErrPrecondition
	}
	sum := md5.Sum(data) //nolint:gosec // ETag
	o := memObject{data: data, etag: `"` + hex.EncodeToString(sum[:]) + `"`, sha256: c.SHA256, modified: m.now()}
	m.objects[key] = o
	return o.info(key), nil
}

func (m *Mem) Get(_ context.Context, key string) (io.ReadCloser, Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, Object{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data)), o.info(key), nil
}

func (m *Mem) Head(_ context.Context, key string) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return Object{}, ErrNotFound
	}
	return o.info(key), nil
}

func (m *Mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *Mem) List(_ context.Context, prefix string) ([]Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Object
	for k, o := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o.info(k))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *Mem) PresignPut(_ context.Context, key string, ttl time.Duration, c Cond) (Request, error) {
	r, err := m.grant(http.MethodPut, key, ttl, c)
	if err != nil {
		return Request{}, err
	}
	if c.IfNoneMatch {
		r.Header.Set("If-None-Match", "*")
	}
	if c.IfMatch != "" {
		r.Header.Set("If-Match", c.IfMatch)
	}
	if c.SHA256 != "" {
		raw, _ := hex.DecodeString(c.SHA256)
		r.Header.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(raw))
	}
	return r, nil
}

func (m *Mem) PresignGet(_ context.Context, key string, ttl time.Duration) (Request, error) {
	return m.grant(http.MethodGet, key, ttl, Cond{})
}

func (m *Mem) grant(method, key string, ttl time.Duration, c Cond) (Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.baseURL == "" {
		return Request{}, fmt.Errorf("objstore: Mem has no base URL; call SetBaseURL")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return Request{}, err
	}
	token := hex.EncodeToString(b)
	exp := m.now().Add(ttl)
	m.grants[token] = grant{method: method, key: key, cond: c, expires: exp}
	return Request{Method: method, URL: m.baseURL + "/presigned/" + token, Header: http.Header{}, Expires: exp}, nil
}

// Handler serves presigned requests with S3's status codes: 403 for a bad or
// expired signature, 404, 412 for a lost condition, 400 for a bad checksum.
func (m *Mem) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/presigned/")
		m.mu.Lock()
		g, ok := m.grants[token]
		m.mu.Unlock()
		if !ok || g.method != r.Method || m.now().After(g.expires) {
			http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet:
			rc, _, err := m.Get(r.Context(), g.key)
			if err != nil {
				http.Error(w, "NoSuchKey", http.StatusNotFound)
				return
			}
			defer rc.Close()
			_, _ = io.Copy(w, rc)
		case http.MethodPut:
			if !signedHeadersMatch(r, g.cond) {
				http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
				return
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			o, err := m.put(g.key, data, g.cond)
			switch {
			case err == nil:
				w.Header().Set("ETag", o.ETag)
			case errors.Is(err, ErrPrecondition):
				http.Error(w, "PreconditionFailed", http.StatusPreconditionFailed)
			case errors.Is(err, ErrChecksum):
				http.Error(w, "BadDigest", http.StatusBadRequest)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		default:
			http.Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed)
		}
	})
}

// signedHeadersMatch checks the client sent the conditional headers the URL
// was signed with, as S3 requires.
func signedHeadersMatch(r *http.Request, c Cond) bool {
	if c.IfNoneMatch && r.Header.Get("If-None-Match") != "*" {
		return false
	}
	if c.IfMatch != "" && r.Header.Get("If-Match") != c.IfMatch {
		return false
	}
	if c.SHA256 != "" {
		raw, _ := hex.DecodeString(c.SHA256)
		if r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(raw) {
			return false
		}
	}
	return true
}

func (o memObject) info(key string) Object {
	return Object{Key: key, Size: int64(len(o.data)), ETag: o.etag, SHA256: o.sha256, Modified: o.modified}
}
