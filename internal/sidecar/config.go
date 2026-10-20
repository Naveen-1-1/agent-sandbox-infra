package sidecar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/snapshot"
)

// Config is the sidecar's view of its pod.
type Config struct {
	Workspace    string // "namespace/name"
	WorkspaceUID string
	Epoch        int64
	PodUID       string
	// Root is the workspace disk's mount point and the repository root.
	Root string
	// Scratch is a per-pod scratch directory for bundles and shadow repos.
	Scratch string

	BrokerAddr  string
	BrokerToken string
	// SidecarToken is what the controller must present to call us.
	SidecarToken string
	ListenAddr   string

	Exclude         []string
	Limits          snapshot.Limits
	SnapshotTimeout time.Duration

	HTTP *http.Client
	Log  *slog.Logger
	Now  func() time.Time
}

func (c *Config) defaults() {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Minute}
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.SnapshotTimeout <= 0 {
		c.SnapshotTimeout = 10 * time.Minute
	}
}

// download fetches a presigned GET into path and returns its size. If
// wantSHA is set, a body with a different SHA-256 is an error.
func download(ctx context.Context, client *http.Client, method, url string, headers map[string]string, path, wantSHA string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("download: %s: %s", resp.Status, b)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if wantSHA != "" && hex.EncodeToString(h.Sum(nil)) != wantSHA {
		return 0, errors.New("download: checksum mismatch")
	}
	return n, nil
}

// upload sends path with a presigned PUT. 412 means a create-only key is
// already taken; for content-addressed keys that is the same content.
func upload(ctx context.Context, client *http.Client, method, url string, headers map[string]string, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	// A plain reader (not *os.File) keeps net/http off sendfile, which some
	// sandboxes forbid; the bundles are small enough that it doesn't matter.
	body := struct{ io.Reader }{f}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPreconditionFailed:
		return nil
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload: %s: %s", resp.Status, b)
	}
}
